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
}

impl Drop for CortexLock {
    fn drop(&mut self) {
        let _ = fs2::FileExt::unlock(&self.file);
    }
}
