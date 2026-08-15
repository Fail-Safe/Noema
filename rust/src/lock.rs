use anyhow::{Context, Result};
use fs2::FileExt;
use std::{
    fs::{self, File, OpenOptions},
    path::{Path, PathBuf},
};

pub struct CortexLock {
    file: File,
    pub path: PathBuf,
}

impl CortexLock {
    pub fn acquire(runtime_dir: &Path, cortex_id: &str) -> Result<Self> {
        fs::create_dir_all(runtime_dir)?;
        let path = runtime_dir.join(format!("{cortex_id}.lock"));
        let file = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(&path)?;
        file.try_lock_exclusive()
            .with_context(|| format!("cortex is already locked ({})", path.display()))?;
        Ok(Self { file, path })
    }

    pub fn try_acquire_background(cortex_id: &str) -> Result<Option<Self>> {
        if cortex_id.is_empty() {
            anyhow::bail!("cortex ID required for background lock path")
        }
        let base = std::env::var_os("XDG_RUNTIME_DIR")
            .map(PathBuf::from)
            .unwrap_or_else(std::env::temp_dir);
        let runtime_dir = base.join("noema").join(cortex_id);
        fs::create_dir_all(&runtime_dir).with_context(|| {
            format!("creating background runtime dir {}", runtime_dir.display())
        })?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&runtime_dir, fs::Permissions::from_mode(0o700))?;
        }
        let path = runtime_dir.join("background.lock");
        Self::try_acquire_path(&path)
    }

    fn try_acquire_path(path: &Path) -> Result<Option<Self>> {
        let file = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(path)
            .with_context(|| format!("opening background lock {}", path.display()))?;
        match file.try_lock_exclusive() {
            Ok(()) => Ok(Some(Self {
                file,
                path: path.to_path_buf(),
            })),
            Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => Ok(None),
            Err(error) => {
                Err(error).with_context(|| format!("acquiring background lock {}", path.display()))
            }
        }
    }
}

impl Drop for CortexLock {
    fn drop(&mut self) {
        let _ = fs2::FileExt::unlock(&self.file);
    }
}

#[cfg(test)]
mod tests {
    use super::CortexLock;

    #[test]
    fn background_lock_has_a_single_owner_and_can_be_reacquired() {
        let temp = tempfile::tempdir().unwrap();
        let path = temp.path().join("background.lock");

        let first = CortexLock::try_acquire_path(&path).unwrap().unwrap();
        assert!(CortexLock::try_acquire_path(&path).unwrap().is_none());

        drop(first);
        assert!(CortexLock::try_acquire_path(&path).unwrap().is_some());
    }
}
