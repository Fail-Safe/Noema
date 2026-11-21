use anyhow::{Context, Result, bail};
use fs2::FileExt;
use std::{
    fs::{self, File, OpenOptions},
    io::{Seek, SeekFrom, Write},
    path::{Path, PathBuf},
};

const BACKGROUND_LOCK_FILE: &str = ".noema-background.lock";

/// Outcome of checking a held background lock against the directory entry.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LockRefresh {
    /// The lock file still names this process's open inode.
    Held,
    /// The directory entry was replaced and this process locked the new file.
    Reacquired,
    /// Another live process holds the replacement file.
    Lost,
}

pub struct CortexLock {
    files: Vec<File>,
    pub path: PathBuf,
    #[cfg(unix)]
    identity: (u64, u64),
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
        Ok(Self::from_files(vec![file], path))
    }

    /// One owner per cortex for sync, watch, expiry, consolidation, and embedding.
    ///
    /// The authoritative lock is `.noema-background.lock` inside the cortex
    /// directory, so processes with different runtime directories still contend.
    /// `flock` releases that lock when the owner exits or is killed. A PID
    /// recorded in the file is diagnostic only; a dead PID does not block the
    /// next owner. If a sync client replaces the file, [`Self::refresh_if_replaced`]
    /// locks the new inode or reports that another process took it.
    ///
    /// HTTP `noema serve` and stdio MCP use the same lock. The first process to
    /// acquire it runs background work. The others keep serving MCP and do not
    /// take over until they are restarted after the owner exits.
    ///
    /// When this runtime directory is writable, the owner also holds the
    /// 0.24 runtime lock (`$XDG_RUNTIME_DIR/noema/<cortex-id>/background.lock`,
    /// or the system temp directory). That keeps a single owner against a 0.24
    /// process that shares the runtime directory. A 0.24 process using a
    /// different runtime directory cannot see either of this process's locks.
    pub fn try_acquire_background(cortex_dir: &Path, cortex_id: &str) -> Result<Option<Self>> {
        let runtime = std::env::var_os("XDG_RUNTIME_DIR")
            .map(PathBuf::from)
            .unwrap_or_else(std::env::temp_dir);
        Self::try_acquire_background_at(cortex_dir, cortex_id, &runtime)
    }

    fn try_acquire_background_at(
        cortex_dir: &Path,
        cortex_id: &str,
        runtime_dir: &Path,
    ) -> Result<Option<Self>> {
        if cortex_id.is_empty() || cortex_id.contains(['/', '\\', '\0']) {
            bail!("cortex ID required for background lock path")
        }
        if !cortex_dir.is_dir() {
            bail!(
                "cortex directory {} is required for the background lock",
                cortex_dir.display()
            );
        }
        let path = cortex_dir.join(BACKGROUND_LOCK_FILE);
        let Some(cortex_file) = try_lock_file(&path)? else {
            return Ok(None);
        };
        match try_lock_legacy(runtime_dir, cortex_id) {
            Ok(Some(legacy)) => Ok(Some(Self::from_files(vec![cortex_file, legacy], path))),
            Ok(None) => Ok(None),
            Err(error) => {
                eprintln!(
                    "legacy background lock is unavailable ({error:#}); using the cortex lock"
                );
                Ok(Some(Self::from_files(vec![cortex_file], path)))
            }
        }
    }

    fn from_files(files: Vec<File>, path: PathBuf) -> Self {
        #[cfg(unix)]
        let identity = {
            use std::os::unix::fs::MetadataExt;
            let metadata = files[0].metadata().expect("background lock metadata");
            (metadata.dev(), metadata.ino())
        };
        Self {
            files,
            path,
            #[cfg(unix)]
            identity,
        }
    }

    /// True when the lock path still refers to the inode this process holds.
    pub fn still_owns(&self) -> bool {
        #[cfg(unix)]
        {
            use std::os::unix::fs::MetadataExt;
            match fs::symlink_metadata(&self.path) {
                Ok(metadata) if metadata.file_type().is_symlink() => false,
                Ok(metadata) => {
                    metadata.dev() == self.identity.0 && metadata.ino() == self.identity.1
                }
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => false,
                Err(_) => true,
            }
        }
        #[cfg(not(unix))]
        {
            self.path.is_file()
        }
    }

    /// Re-lock the directory entry after a sync client replaces the lock file.
    pub fn refresh_if_replaced(&mut self) -> Result<LockRefresh> {
        if self.still_owns() {
            return Ok(LockRefresh::Held);
        }
        match try_lock_file(&self.path)? {
            Some(file) => {
                #[cfg(unix)]
                {
                    use std::os::unix::fs::MetadataExt;
                    let metadata = file.metadata()?;
                    self.identity = (metadata.dev(), metadata.ino());
                }
                self.files[0] = file;
                Ok(LockRefresh::Reacquired)
            }
            None => Ok(LockRefresh::Lost),
        }
    }
}

fn try_lock_legacy(runtime_dir: &Path, cortex_id: &str) -> Result<Option<File>> {
    let directory = runtime_dir.join("noema").join(cortex_id);
    fs::create_dir_all(&directory)
        .with_context(|| format!("creating background runtime dir {}", directory.display()))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&directory, fs::Permissions::from_mode(0o700))?;
    }
    try_lock_file(&directory.join("background.lock"))
}

fn try_lock_file(path: &Path) -> Result<Option<File>> {
    for _ in 0..5 {
        let mut file = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(path)
            .with_context(|| format!("opening background lock {}", path.display()))?;
        match file.try_lock_exclusive() {
            Ok(()) => {}
            Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => return Ok(None),
            Err(error) => {
                return Err(error)
                    .with_context(|| format!("acquiring background lock {}", path.display()));
            }
        }
        let fd_meta = file.metadata()?;
        let path_meta = match fs::symlink_metadata(path) {
            Ok(metadata) => metadata,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
            Err(error) => {
                return Err(error)
                    .with_context(|| format!("statting background lock {}", path.display()));
            }
        };
        if path_meta.file_type().is_symlink() || !same_file(&fd_meta, &path_meta) {
            let _ = FileExt::unlock(&file);
            continue;
        }
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mut permissions = file.metadata()?.permissions();
            permissions.set_mode(0o600);
            fs::set_permissions(path, permissions)?;
        }
        write_owner_record(&mut file)?;
        let path_meta = fs::symlink_metadata(path)
            .with_context(|| format!("statting background lock {} after write", path.display()))?;
        if path_meta.file_type().is_symlink() || !same_file(&file.metadata()?, &path_meta) {
            let _ = FileExt::unlock(&file);
            continue;
        }
        return Ok(Some(file));
    }
    bail!(
        "background lock {} was replaced or is a symlink",
        path.display()
    )
}

fn write_owner_record(file: &mut File) -> Result<()> {
    file.seek(SeekFrom::Start(0))?;
    file.set_len(0)?;
    writeln!(file, "pid {}", std::process::id())?;
    file.sync_all()?;
    Ok(())
}

fn same_file(left: &fs::Metadata, right: &fs::Metadata) -> bool {
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt;
        left.dev() == right.dev() && left.ino() == right.ino()
    }
    #[cfg(not(unix))]
    {
        let _ = (left, right);
        true
    }
}

impl Drop for CortexLock {
    fn drop(&mut self) {
        for file in &self.files {
            let _ = FileExt::unlock(file);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::{CortexLock, LockRefresh};
    use std::fs;

    #[test]
    fn background_lock_has_a_single_owner_and_can_be_reacquired() {
        let temp = tempfile::tempdir().unwrap();
        let path = temp.path().join("background.lock");

        let first = CortexLock::try_lock_file_for_test(&path).unwrap().unwrap();
        assert!(CortexLock::try_lock_file_for_test(&path).unwrap().is_none());

        drop(first);
        assert!(CortexLock::try_lock_file_for_test(&path).unwrap().is_some());
    }

    impl CortexLock {
        fn try_lock_file_for_test(path: &std::path::Path) -> anyhow::Result<Option<Self>> {
            Ok(super::try_lock_file(path)?
                .map(|file| Self::from_files(vec![file], path.to_path_buf())))
        }
    }

    #[test]
    fn background_owner_follows_the_cortex_across_runtime_directories() {
        let temp = tempfile::tempdir().unwrap();
        let cortex = temp.path().join("cortex");
        fs::create_dir(&cortex).unwrap();
        let runtime_a = temp.path().join("runtime-a");
        let runtime_b = temp.path().join("runtime-b");
        let id = "01ARZ3NDEKTSV4RRFFQ69G5FAV";

        let first = CortexLock::try_acquire_background_at(&cortex, id, &runtime_a)
            .unwrap()
            .unwrap();
        assert!(
            CortexLock::try_acquire_background_at(&cortex, id, &runtime_b)
                .unwrap()
                .is_none()
        );
        assert_eq!(
            fs::read_to_string(cortex.join(".noema-background.lock")).unwrap(),
            format!("pid {}\n", std::process::id())
        );

        drop(first);
        assert!(
            CortexLock::try_acquire_background_at(&cortex, id, &runtime_b)
                .unwrap()
                .is_some()
        );
    }

    #[test]
    fn stale_owner_record_does_not_block_and_replacement_is_detected() {
        let temp = tempfile::tempdir().unwrap();
        let cortex = temp.path().join("cortex");
        fs::create_dir(&cortex).unwrap();
        let runtime = temp.path().join("runtime");
        let id = "01ARZ3NDEKTSV4RRFFQ69G5FAV";
        let path = cortex.join(".noema-background.lock");
        fs::write(&path, "pid 1\n").unwrap();

        let mut owner = CortexLock::try_acquire_background_at(&cortex, id, &runtime)
            .unwrap()
            .unwrap();
        assert!(owner.still_owns());
        assert_eq!(owner.refresh_if_replaced().unwrap(), LockRefresh::Held);
        let record = fs::read_to_string(&path).unwrap();
        assert!(record.starts_with("pid "), "{record}");
        assert_ne!(record, "pid 1\n");

        fs::remove_file(&path).unwrap();
        fs::write(&path, "pid 1\n").unwrap();
        assert!(!owner.still_owns());
        assert_eq!(
            owner.refresh_if_replaced().unwrap(),
            LockRefresh::Reacquired
        );
        assert!(owner.still_owns());
        assert!(
            CortexLock::try_acquire_background_at(&cortex, id, &runtime)
                .unwrap()
                .is_none()
        );

        fs::remove_file(&path).unwrap();
        let other_runtime = temp.path().join("runtime-other");
        let other = CortexLock::try_acquire_background_at(&cortex, id, &other_runtime)
            .unwrap()
            .unwrap();
        assert_eq!(owner.refresh_if_replaced().unwrap(), LockRefresh::Lost);
        assert!(other.still_owns());
        drop(other);
    }

    #[test]
    #[cfg(unix)]
    fn legacy_runtime_lock_defers_to_an_older_process_in_that_runtime() {
        use fs2::FileExt;
        use std::fs::OpenOptions;

        let temp = tempfile::tempdir().unwrap();
        let cortex = temp.path().join("cortex");
        fs::create_dir(&cortex).unwrap();
        let runtime_a = temp.path().join("runtime-a");
        let runtime_b = temp.path().join("runtime-b");
        let id = "01ARZ3NDEKTSV4RRFFQ69G5FAV";
        let legacy_dir = runtime_a.join("noema").join(id);
        fs::create_dir_all(&legacy_dir).unwrap();
        let legacy = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(legacy_dir.join("background.lock"))
            .unwrap();
        legacy.try_lock_exclusive().unwrap();

        assert!(
            CortexLock::try_acquire_background_at(&cortex, id, &runtime_a)
                .unwrap()
                .is_none()
        );
        let elsewhere = CortexLock::try_acquire_background_at(&cortex, id, &runtime_b)
            .unwrap()
            .unwrap();
        drop(elsewhere);
        assert!(
            CortexLock::try_acquire_background_at(&cortex, id, &runtime_a)
                .unwrap()
                .is_none()
        );

        drop(legacy);
        assert!(
            CortexLock::try_acquire_background_at(&cortex, id, &runtime_a)
                .unwrap()
                .is_some()
        );
    }

    #[test]
    #[cfg(unix)]
    fn background_lock_rejects_a_symlink() {
        let temp = tempfile::tempdir().unwrap();
        let cortex = temp.path().join("cortex");
        fs::create_dir(&cortex).unwrap();
        let runtime = temp.path().join("runtime");
        let outside = temp.path().join("outside.lock");
        fs::write(&outside, "pid 1\n").unwrap();
        std::os::unix::fs::symlink(&outside, cortex.join(".noema-background.lock")).unwrap();

        let error = match CortexLock::try_acquire_background_at(
            &cortex,
            "01ARZ3NDEKTSV4RRFFQ69G5FAV",
            &runtime,
        ) {
            Err(error) => error,
            Ok(_) => panic!("symlink lock was acquired"),
        };
        assert!(error.to_string().contains("symlink"), "{error:#}");
    }
}
