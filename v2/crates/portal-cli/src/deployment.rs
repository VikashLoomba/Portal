//! Transactional installation of the single `portal` executable.
//!
//! Both LaunchAgents execute the same path, so callers acquire a deployment
//! lock, unregister both jobs, then activate a swap. The candidate is staged
//! beside the destination and renamed over it atomically; a verified backup
//! remains available until the newly registered daemon proves healthy.
//! Rollback is explicit because only the lifecycle coordinator knows that both
//! jobs are safely unregistered.

use std::fs::{self, File, OpenOptions};
use std::io::{self, Write as _};
use std::path::{Path, PathBuf};

use fs2::FileExt as Fs2FileExt;
use tempfile::Builder;

/// Exclusive ownership of install/upgrade lifecycle transitions for one
/// destination. It deliberately begins before launchd is touched, not merely
/// around the rename, so concurrent commands cannot interleave stop/swap/start.
pub struct Deployment {
    destination: PathBuf,
    parent: PathBuf,
    lock: File,
}

impl Deployment {
    pub fn acquire(destination: &Path) -> Result<Self, String> {
        let parent = destination
            .parent()
            .ok_or_else(|| format!("{} has no parent directory", destination.display()))?
            .to_path_buf();
        fs::create_dir_all(&parent).map_err(|e| format!("create {}: {e}", parent.display()))?;

        let lock_path = parent.join(".portal.install.lock");
        let lock = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(&lock_path)
            .map_err(|e| format!("open deployment lock {}: {e}", lock_path.display()))?;
        Fs2FileExt::try_lock_exclusive(&lock).map_err(|e| {
            format!("another portal install or upgrade is already in progress ({e})")
        })?;

        Ok(Self {
            destination: destination.to_path_buf(),
            parent,
            lock,
        })
    }

    /// Copy `candidate` into a same-directory staging file, fsync it, and
    /// atomically rename it over the destination. The prior executable is
    /// copied to a private same-directory rollback file first, so the public
    /// path is never absent—even if the process crashes during deployment.
    pub fn swap(&self, candidate: &Path) -> Result<BinarySwap<'_>, String> {
        let mut staged = Builder::new()
            .prefix(".portal.candidate.")
            .tempfile_in(&self.parent)
            .map_err(|e| format!("create staged binary: {e}"))?;
        copy_executable(candidate, staged.as_file_mut())
            .map_err(|e| format!("stage {}: {e}", candidate.display()))?;

        let backup = if self.destination.exists() {
            let mut file = Builder::new()
                .prefix(".portal.rollback.")
                .tempfile_in(&self.parent)
                .map_err(|e| format!("create rollback file: {e}"))?;
            copy_executable(&self.destination, file.as_file_mut())
                .map_err(|e| format!("back up {}: {e}", self.destination.display()))?;
            let (_, path) = file
                .keep()
                .map_err(|e| format!("keep rollback file: {e}"))?;
            Some(path)
        } else {
            None
        };

        if let Err(e) = staged.persist(&self.destination) {
            if let Some(path) = &backup {
                let _ = fs::remove_file(path);
            }
            return Err(format!(
                "atomically install {}: {}",
                self.destination.display(),
                e.error
            ));
        }
        sync_directory(&self.parent).map_err(|e| format!("sync {}: {e}", self.parent.display()))?;

        Ok(BinarySwap {
            deployment: self,
            backup,
            finished: false,
        })
    }
}

impl Drop for Deployment {
    fn drop(&mut self) {
        let _ = Fs2FileExt::unlock(&self.lock);
    }
}

/// Active executable replacement tied to its deployment lock.
pub struct BinarySwap<'a> {
    deployment: &'a Deployment,
    backup: Option<PathBuf>,
    finished: bool,
}

impl BinarySwap<'_> {
    /// Make the replacement permanent and remove rollback state.
    pub fn commit(mut self) -> Result<(), String> {
        if let Some(path) = self.backup.take() {
            fs::remove_file(&path)
                .map_err(|e| format!("remove rollback file {}: {e}", path.display()))?;
        }
        sync_directory(&self.deployment.parent)
            .map_err(|e| format!("sync {}: {e}", self.deployment.parent.display()))?;
        self.finished = true;
        Ok(())
    }

    /// Restore the executable that was present before this transaction.
    pub fn rollback(mut self) -> Result<(), String> {
        self.rollback_inner()
    }

    fn rollback_inner(&mut self) -> Result<(), String> {
        if self.finished {
            return Ok(());
        }
        if let Some(backup) = self.backup.take() {
            fs::rename(&backup, &self.deployment.destination).map_err(|e| {
                format!(
                    "restore {} from {}: {e}",
                    self.deployment.destination.display(),
                    backup.display()
                )
            })?;
        } else if self.deployment.destination.exists() {
            fs::remove_file(&self.deployment.destination)
                .map_err(|e| format!("remove {}: {e}", self.deployment.destination.display()))?;
        }
        sync_directory(&self.deployment.parent)
            .map_err(|e| format!("sync {}: {e}", self.deployment.parent.display()))?;
        self.finished = true;
        Ok(())
    }
}

fn copy_executable(source: &Path, destination: &mut File) -> io::Result<()> {
    let mut source_file = File::open(source)?;
    io::copy(&mut source_file, destination)?;
    destination.flush()?;
    destination.set_permissions(source_file.metadata()?.permissions())?;
    destination.sync_all()
}

fn sync_directory(path: &Path) -> io::Result<()> {
    File::open(path)?.sync_all()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt as _;

    fn fixture() -> (tempfile::TempDir, PathBuf, PathBuf) {
        let dir = tempfile::tempdir().unwrap();
        let candidate = dir.path().join("candidate");
        let destination = dir.path().join("portal");
        fs::write(&candidate, b"new").unwrap();
        fs::set_permissions(&candidate, fs::Permissions::from_mode(0o755)).unwrap();
        (dir, candidate, destination)
    }

    #[test]
    fn commit_keeps_new_executable() {
        let (_dir, candidate, destination) = fixture();
        fs::write(&destination, b"old").unwrap();
        let deployment = Deployment::acquire(&destination).unwrap();
        let swap = deployment.swap(&candidate).unwrap();
        assert_eq!(fs::read(&destination).unwrap(), b"new");
        swap.commit().unwrap();
        assert_eq!(fs::read(&destination).unwrap(), b"new");
    }

    #[test]
    fn explicit_rollback_restores_old_executable() {
        let (_dir, candidate, destination) = fixture();
        fs::write(&destination, b"old").unwrap();
        let deployment = Deployment::acquire(&destination).unwrap();
        deployment.swap(&candidate).unwrap().rollback().unwrap();
        assert_eq!(fs::read(&destination).unwrap(), b"old");
    }

    #[test]
    fn rollback_of_first_install_removes_destination() {
        let (_dir, candidate, destination) = fixture();
        let deployment = Deployment::acquire(&destination).unwrap();
        deployment.swap(&candidate).unwrap().rollback().unwrap();
        assert!(!destination.exists());
    }

    #[test]
    fn concurrent_deployments_are_rejected() {
        let (_dir, _candidate, destination) = fixture();
        let first = Deployment::acquire(&destination).unwrap();
        let err = Deployment::acquire(&destination)
            .err()
            .expect("second deployment must fail");
        assert!(err.contains("already in progress"), "{err}");
        drop(first);
        Deployment::acquire(&destination).unwrap();
    }
}
