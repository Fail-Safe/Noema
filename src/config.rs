use std::{
    collections::BTreeMap,
    env, fs,
    fs::{File, OpenOptions},
    io::Write,
    path::{Path, PathBuf},
};

use anyhow::{Context, Result, bail};
use fs2::FileExt;
use serde::{Deserialize, Serialize};

use crate::trace::sync_directory;

const CONFIG_LOCK_NAME: &str = ".config.lock";
const CONFIG_TEMP_NAME: &str = ".config.yaml.tmp";

struct ConfigWriteLock {
    file: File,
}

impl ConfigWriteLock {
    fn acquire(directory: &Path) -> Result<Self> {
        let path = directory.join(CONFIG_LOCK_NAME);
        let file = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(&path)
            .with_context(|| format!("opening config lock {}", path.display()))?;
        set_owner_only_permissions(&path)?;
        FileExt::lock_exclusive(&file)
            .with_context(|| format!("locking config {}", path.display()))?;
        Ok(Self { file })
    }
}

impl Drop for ConfigWriteLock {
    fn drop(&mut self) {
        let _ = FileExt::unlock(&self.file);
    }
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Config {
    #[serde(default)]
    pub default: String,
    #[serde(default)]
    pub cortexes: BTreeMap<String, CortexEntry>,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub trash_days: u32,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ui: Option<UiConfig>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CortexEntry {
    pub path: PathBuf,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub id: String,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct UiConfig {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub theme: String,
}

fn is_zero(value: &u32) -> bool {
    *value == 0
}

pub fn path() -> Result<PathBuf> {
    if let Some(base) = env::var_os("XDG_CONFIG_HOME") {
        return Ok(PathBuf::from(base).join("noema/config.yaml"));
    }
    let home = env::var_os("HOME").ok_or_else(|| anyhow::anyhow!("HOME is not set"))?;
    #[cfg(target_os = "macos")]
    return Ok(PathBuf::from(home).join("Library/Application Support/noema/config.yaml"));
    #[cfg(not(target_os = "macos"))]
    Ok(PathBuf::from(home).join(".config/noema/config.yaml"))
}

impl Config {
    pub fn load() -> Result<Self> {
        let path = path()?;
        match fs::read(&path) {
            Ok(data) => serde_yaml::from_slice(&data)
                .with_context(|| format!("parsing config {}", path.display())),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(Self::default()),
            Err(error) => Err(error).with_context(|| format!("reading config {}", path.display())),
        }
    }

    pub fn save(&self) -> Result<()> {
        self.validate_paths()?;
        let path = path()?;
        self.save_to_path(&path)
    }

    pub fn theme(&self) -> &str {
        self.ui
            .as_ref()
            .map(|ui| ui.theme.as_str())
            .filter(|theme| !theme.is_empty())
            .unwrap_or("auto")
    }

    fn validate_paths(&self) -> Result<()> {
        let mut seen = BTreeMap::new();
        for (name, entry) in &self.cortexes {
            let Ok(path) = entry.path.canonicalize() else {
                continue;
            };
            if let Some(other) = seen.insert(path.clone(), name) {
                bail!(
                    "refusing to save config: cortex entries {:?} and {:?} both point at {}",
                    name,
                    other,
                    path.display()
                );
            }
        }
        Ok(())
    }

    pub(crate) fn save_to_path(&self, path: &Path) -> Result<()> {
        let directory = path
            .parent()
            .ok_or_else(|| anyhow::anyhow!("config path has no parent directory"))?;
        let directory_existed = directory.exists();
        fs::create_dir_all(directory)
            .with_context(|| format!("creating config directory {}", directory.display()))?;
        if !directory_existed {
            set_config_directory_permissions(directory)?;
            if let Some(parent) = directory.parent() {
                sync_directory(parent)?;
            }
        }
        let _lock = ConfigWriteLock::acquire(directory)?;
        let encoded = serde_yaml::to_string(self).context("encoding config")?;
        write_config_atomic(path, encoded.as_bytes())
    }
}

fn write_config_atomic(path: &Path, data: &[u8]) -> Result<()> {
    let directory = path.parent().unwrap();
    if path.exists() {
        drop(
            OpenOptions::new()
                .write(true)
                .open(path)
                .with_context(|| format!("opening config {} for replacement", path.display()))?,
        );
    }
    let temporary = directory.join(CONFIG_TEMP_NAME);
    if let Ok(metadata) = fs::symlink_metadata(&temporary) {
        if !metadata.file_type().is_file() {
            bail!("refusing to replace non-file config temporary artifact")
        }
        fs::remove_file(&temporary).context("removing stale config temporary file")?;
    }
    let result = (|| -> Result<()> {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temporary)
            .context("creating config temporary file")?;
        set_owner_only_permissions(&temporary)?;
        file.write_all(data)
            .context("writing config temporary file")?;
        file.sync_all().context("syncing config temporary file")?;
        set_private_permissions(&temporary)?;
        file.sync_all()
            .context("syncing config temporary permissions")?;
        drop(file);
        pause_config_for_test("before-rename");
        fs::rename(&temporary, path).context("replacing config atomically")?;
        sync_directory(directory).context("syncing config directory")?;
        Ok(())
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temporary);
    }
    result
}

#[cfg(unix)]
fn set_private_permissions(path: &std::path::Path) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(0o640))?;
    Ok(())
}

#[cfg(unix)]
fn set_owner_only_permissions(path: &Path) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(0o600))?;
    Ok(())
}

#[cfg(not(unix))]
fn set_owner_only_permissions(_path: &Path) -> Result<()> {
    Ok(())
}

#[cfg(unix)]
fn set_config_directory_permissions(path: &Path) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(0o750))?;
    Ok(())
}

#[cfg(not(unix))]
fn set_config_directory_permissions(_path: &Path) -> Result<()> {
    Ok(())
}

#[cfg(not(unix))]
fn set_private_permissions(_path: &std::path::Path) -> Result<()> {
    Ok(())
}

#[cfg(debug_assertions)]
fn pause_config_for_test(point: &str) {
    if std::env::var("NOEMA_RUST_TEST_CONFIG_PAUSE_POINT").as_deref() != Ok(point) {
        return;
    }
    let marker = std::env::var_os("NOEMA_RUST_TEST_CONFIG_PAUSE_MARKER")
        .map(PathBuf::from)
        .expect("config test pause marker is required");
    fs::write(marker, b"ready\n").expect("writing config test pause marker");
    loop {
        std::thread::park_timeout(std::time::Duration::from_secs(60));
    }
}

#[cfg(not(debug_assertions))]
fn pause_config_for_test(_point: &str) {}

#[cfg(test)]
mod tests {
    use super::*;

    fn config(default: &str) -> Config {
        Config {
            default: default.into(),
            ..Config::default()
        }
    }

    #[test]
    fn atomic_save_replaces_complete_yaml_and_removes_temporary_file() {
        let temp = tempfile::tempdir().unwrap();
        let path = temp.path().join("config.yaml");
        fs::write(&path, serde_yaml::to_string(&config("old")).unwrap()).unwrap();

        config("new").save_to_path(&path).unwrap();

        let loaded: Config = serde_yaml::from_slice(&fs::read(&path).unwrap()).unwrap();
        assert_eq!(loaded.default, "new");
        assert!(!temp.path().join(CONFIG_TEMP_NAME).exists());
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            assert_eq!(
                fs::metadata(path).unwrap().permissions().mode() & 0o777,
                0o640
            );
        }
    }

    #[cfg(unix)]
    #[test]
    fn read_only_config_refuses_atomic_replacement_without_changing_bytes() {
        use std::os::unix::fs::PermissionsExt;

        let temp = tempfile::tempdir().unwrap();
        let path = temp.path().join("config.yaml");
        let original = serde_yaml::to_string(&config("old")).unwrap().into_bytes();
        fs::write(&path, &original).unwrap();
        fs::set_permissions(&path, fs::Permissions::from_mode(0o440)).unwrap();

        assert!(config("new").save_to_path(&path).is_err());
        assert_eq!(fs::read(&path).unwrap(), original);
        assert!(!temp.path().join(CONFIG_TEMP_NAME).exists());
    }

    #[test]
    fn non_file_temporary_artifact_fails_closed() {
        let temp = tempfile::tempdir().unwrap();
        let path = temp.path().join("config.yaml");
        let original = serde_yaml::to_string(&config("old")).unwrap().into_bytes();
        fs::write(&path, &original).unwrap();
        fs::create_dir(temp.path().join(CONFIG_TEMP_NAME)).unwrap();

        let error = config("new").save_to_path(&path).unwrap_err().to_string();
        assert!(error.contains("non-file config temporary artifact"));
        assert_eq!(fs::read(&path).unwrap(), original);
        assert!(temp.path().join(CONFIG_TEMP_NAME).is_dir());
    }
}
