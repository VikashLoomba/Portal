//! Remote file-transfer command construction and directory-list decoding.
//!
//! Files themselves never enter the JSON control protocol. The local client
//! sends a tar stream after a two-phase `upload_files` handshake, and the
//! daemon forwards that stream over a dedicated SSH exec channel.

use portal_transport::shell_quote;

use crate::localapi::{RemoteDirectory, RemoteDirectoryEntry};

/// Accept only home-relative or absolute remote paths. Relative paths are
/// ambiguous because SSH commands do not have a stable working directory.
pub fn validate_remote_path(path: &str) -> Result<(), String> {
    if path == "~" || path.starts_with("~/") || path.starts_with('/') {
        if path.as_bytes().contains(&0) {
            return Err("remote path contains a NUL byte".into());
        }
        return Ok(());
    }
    Err("remote path must be absolute or begin with ~/".into())
}

/// A shell expression that expands `~` without evaluating any other user
/// input. Every non-home component remains single-quoted.
fn path_expression(path: &str) -> Result<String, String> {
    validate_remote_path(path)?;
    if path == "~" {
        Ok(r#""$HOME""#.into())
    } else if let Some(rest) = path.strip_prefix("~/") {
        Ok(format!(r#""$HOME"/{}"#, shell_quote(rest)))
    } else {
        Ok(shell_quote(path))
    }
}

/// Extract a client-produced tar stream into the chosen folder.
pub fn extract_script(destination: &str) -> Result<String, String> {
    let destination = path_expression(destination)?;
    Ok(format!(
        "set -e; destination={destination}; mkdir -p -- \"$destination\"; \
         tar -xf - -C \"$destination\""
    ))
}

/// Print current canonical path, parent canonical path, then immediate child
/// directory names, all NUL-delimited. NUL framing keeps spaces and newlines
/// in valid Unix names unambiguous.
pub fn list_directory_script(path: &str) -> Result<String, String> {
    let path = path_expression(path)?;
    Ok(format!(
        "set -e; directory={path}; cd -- \"$directory\"; current=$(pwd -P); \
         printf '%s\\0' \"$current\"; \
         if [ \"$current\" = / ]; then printf '\\0'; \
         else parent=$(cd .. && pwd -P); printf '%s\\0' \"$parent\"; fi; \
         find . -mindepth 1 -maxdepth 1 -type d -printf '%f\\0' 2>/dev/null | LC_ALL=C sort -z"
    ))
}

pub fn create_directory_script(path: &str) -> Result<String, String> {
    let path = path_expression(path)?;
    Ok(format!(
        "set -e; directory={path}; mkdir -p -- \"$directory\"; cd -- \"$directory\"; printf '%s' \"$PWD\""
    ))
}

pub fn decode_directory_listing(bytes: &[u8]) -> Result<RemoteDirectory, String> {
    let mut fields = bytes.split(|byte| *byte == 0);
    let path = decode_path(fields.next(), "directory path")?;
    let parent = decode_path(fields.next(), "parent path")?;
    let parent = (!parent.is_empty()).then_some(parent);
    let mut directories = Vec::new();

    for field in fields {
        if field.is_empty() {
            continue;
        }
        let name = String::from_utf8(field.to_vec())
            .map_err(|_| "remote directory contains a name that is not valid UTF-8".to_string())?;
        let child_path = if path == "/" {
            format!("/{name}")
        } else {
            format!("{}/{name}", path.trim_end_matches('/'))
        };
        directories.push(RemoteDirectoryEntry {
            name,
            path: child_path,
        });
    }

    Ok(RemoteDirectory {
        path,
        parent,
        directories,
    })
}

fn decode_path(field: Option<&[u8]>, label: &str) -> Result<String, String> {
    let field = field.ok_or_else(|| format!("remote listing omitted {label}"))?;
    String::from_utf8(field.to_vec()).map_err(|_| format!("remote {label} is not valid UTF-8"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn paths_are_shell_quoted_without_losing_home_expansion() {
        let script = extract_script("~/tmp/it's here").unwrap();
        assert!(script.contains(r#"destination="$HOME"/'tmp/it'\''s here'"#));
        assert!(!script.contains("it's here"));
        assert!(extract_script("relative/path").is_err());
    }

    #[test]
    fn listing_decodes_paths_and_directory_names() {
        let listing = decode_directory_listing(b"/home/me\0/home\0src\0space here\0").unwrap();
        assert_eq!(listing.path, "/home/me");
        assert_eq!(listing.parent.as_deref(), Some("/home"));
        assert_eq!(listing.directories[0].path, "/home/me/src");
        assert_eq!(listing.directories[1].name, "space here");
    }

    #[test]
    fn root_listing_has_no_parent() {
        let listing = decode_directory_listing(b"/\0\0etc\0").unwrap();
        assert!(listing.parent.is_none());
        assert_eq!(listing.directories[0].path, "/etc");
    }
}
