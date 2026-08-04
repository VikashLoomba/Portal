//! Remembered-secret storage seam (port of internal/keychain).
//!
//! Backend plan: v1 shells out to `security add-generic-password -U` /
//! `find-generic-password -w` / `delete-generic-password` with a
//! portal-namespaced service name. Rust can use the `security-framework`
//! crate instead of subprocesses; the items must stay ordinary generic
//! passwords (v1 caveat: Touch ID gates portal's RELEASE decision, it does
//! not re-bind items to biometrics — same Keychain access semantics).

/// Blocking storage of remembered credentials, keyed by sanitized label.
pub trait Keychain: Send + Sync {
    /// Read a remembered secret; `Ok(None)` when the item does not exist.
    fn get(&self, label: &str) -> Result<Option<Vec<u8>>, String>;
    /// Persist a newly approved secret under `label` (upsert).
    fn set(&self, label: &str, secret: &[u8]) -> Result<(), String>;
    /// Remove a remembered item, tolerating absence.
    fn delete(&self, label: &str) -> Result<(), String>;
    /// Labels of remembered items (for `portal keychain list`).
    fn list(&self) -> Result<Vec<String>, String>;
}

/// In-memory fake for policy tests and `--dev` runs.
#[derive(Debug, Default)]
pub struct MemoryKeychain {
    items: std::sync::Mutex<std::collections::BTreeMap<String, Vec<u8>>>,
}

impl Keychain for MemoryKeychain {
    fn get(&self, label: &str) -> Result<Option<Vec<u8>>, String> {
        Ok(self.items.lock().unwrap().get(label).cloned())
    }
    fn set(&self, label: &str, secret: &[u8]) -> Result<(), String> {
        self.items
            .lock()
            .unwrap()
            .insert(label.to_string(), secret.to_vec());
        Ok(())
    }
    fn delete(&self, label: &str) -> Result<(), String> {
        self.items.lock().unwrap().remove(label);
        Ok(())
    }
    fn list(&self) -> Result<Vec<String>, String> {
        Ok(self.items.lock().unwrap().keys().cloned().collect())
    }
}
