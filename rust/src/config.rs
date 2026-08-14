use std::{collections::BTreeMap, env, fs, path::PathBuf};

use anyhow::{Context, Result, bail};
use serde::{Deserialize, Serialize};

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
        fs::create_dir_all(path.parent().unwrap())?;
        fs::write(&path, serde_yaml::to_string(self)?)?;
        set_private_permissions(&path)?;
        Ok(())
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
}

#[cfg(unix)]
fn set_private_permissions(path: &std::path::Path) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(0o640))?;
    Ok(())
}

#[cfg(not(unix))]
fn set_private_permissions(_path: &std::path::Path) -> Result<()> {
    Ok(())
}
