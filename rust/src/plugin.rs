use std::{
    fs::{self, OpenOptions},
    io::Write,
    path::{Component, Path, PathBuf},
};

use anyhow::{Context, Result, bail};
use sha2::{Digest, Sha256};

#[derive(Debug, Clone, Copy)]
pub struct ManagedFile {
    pub path: &'static str,
    pub data: &'static [u8],
}

#[derive(Debug, Clone, Copy)]
pub struct Definition {
    pub name: &'static str,
    pub description: &'static str,
    pub files: &'static [ManagedFile],
}

const HERMES_FILES: &[ManagedFile] = &[
    ManagedFile {
        path: "__init__.py",
        data: include_bytes!("../../plugins/hermes/__init__.py"),
    },
    ManagedFile {
        path: "plugin.yaml",
        data: include_bytes!("../../plugins/hermes/plugin.yaml"),
    },
    ManagedFile {
        path: "transport.py",
        data: include_bytes!("../../plugins/hermes/transport.py"),
    },
];

const OBSIDIAN_FILES: &[ManagedFile] = &[
    ManagedFile {
        path: "main.js",
        data: include_bytes!("../../plugins/obsidian/main.js"),
    },
    ManagedFile {
        path: "manifest.json",
        data: include_bytes!("../../plugins/obsidian/manifest.json"),
    },
    ManagedFile {
        path: "styles.css",
        data: include_bytes!("../../plugins/obsidian/styles.css"),
    },
];

pub const HERMES: Definition = Definition {
    name: "hermes",
    description: "Hermes memory provider",
    files: HERMES_FILES,
};

pub const OBSIDIAN: Definition = Definition {
    name: "obsidian",
    description: "Obsidian vault plugin",
    files: OBSIDIAN_FILES,
};

pub const DEFINITIONS: &[Definition] = &[HERMES, OBSIDIAN];

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum State {
    UpToDate,
    Drift,
    NotInstalled,
}

impl State {
    pub fn label(self) -> &'static str {
        match self {
            Self::UpToDate => "up to date",
            Self::Drift => "drift detected",
            Self::NotInstalled => "not installed",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FileState {
    Match,
    Missing,
    Changed,
    Irregular,
}

impl FileState {
    pub fn label(self) -> &'static str {
        match self {
            Self::Match => "match",
            Self::Missing => "missing",
            Self::Changed => "changed",
            Self::Irregular => "not a regular file",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FileReport {
    pub path: String,
    pub state: FileState,
    pub embedded_hash: String,
    pub installed_hash: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StatusReport {
    pub plugin: String,
    pub target: PathBuf,
    pub state: State,
    pub files: Vec<FileReport>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum InstallAction {
    Installed,
    Replaced,
    Unchanged,
    Refused,
    WouldInstall,
    WouldReplace,
}

impl InstallAction {
    pub fn label(self) -> &'static str {
        match self {
            Self::Installed => "installed",
            Self::Replaced => "replaced",
            Self::Unchanged => "unchanged",
            Self::Refused => "refused",
            Self::WouldInstall => "would install",
            Self::WouldReplace => "would replace",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct InstallFileReport {
    pub path: String,
    pub action: InstallAction,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct InstallReport {
    pub plugin: String,
    pub target: PathBuf,
    pub files: Vec<InstallFileReport>,
}

impl InstallReport {
    pub fn pending(&self) -> bool {
        self.files.iter().any(|file| {
            matches!(
                file.action,
                InstallAction::WouldInstall | InstallAction::WouldReplace
            )
        })
    }

    pub fn refused(&self) -> bool {
        self.files
            .iter()
            .any(|file| file.action == InstallAction::Refused)
    }
}

#[derive(Debug, Clone, Copy, Default)]
pub struct InstallOptions {
    pub check: bool,
    pub force: bool,
}

pub fn inspect(definition: Definition, target: &Path) -> Result<StatusReport> {
    validate_definition(definition)?;
    let target = normalize_target(target)?;
    let metadata = match fs::metadata(&target) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Ok(StatusReport {
                plugin: definition.name.into(),
                target,
                state: State::NotInstalled,
                files: Vec::new(),
            });
        }
        Err(error) => {
            return Err(error)
                .with_context(|| format!("inspecting plugin target {}", target.display()));
        }
    };
    if !metadata.is_dir() {
        bail!("plugin target {} is not a directory", target.display());
    }

    let mut state = State::UpToDate;
    let mut files = Vec::with_capacity(definition.files.len());
    for managed in definition.files {
        let installed_path = target.join(managed.path);
        let embedded_hash = hash_bytes(managed.data);
        let mut report = FileReport {
            path: managed.path.into(),
            state: FileState::Missing,
            embedded_hash,
            installed_hash: String::new(),
        };
        match fs::symlink_metadata(&installed_path) {
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                state = State::Drift;
            }
            Err(error) => {
                return Err(error).with_context(|| {
                    format!("inspecting managed file {}", installed_path.display())
                });
            }
            Ok(metadata) if !metadata.file_type().is_file() => {
                report.state = FileState::Irregular;
                state = State::Drift;
            }
            Ok(_) => {
                let installed = fs::read(&installed_path).with_context(|| {
                    format!("reading managed file {}", installed_path.display())
                })?;
                report.installed_hash = hash_bytes(&installed);
                if report.installed_hash == report.embedded_hash {
                    report.state = FileState::Match;
                } else {
                    report.state = FileState::Changed;
                    state = State::Drift;
                }
            }
        }
        files.push(report);
    }
    Ok(StatusReport {
        plugin: definition.name.into(),
        target,
        state,
        files,
    })
}

pub fn install(
    definition: Definition,
    target: &Path,
    options: InstallOptions,
) -> Result<InstallReport> {
    validate_definition(definition)?;
    let target = normalize_target(target)?;
    match fs::metadata(&target) {
        Ok(metadata) if !metadata.is_dir() => {
            bail!("plugin target {} is not a directory", target.display())
        }
        Ok(_) => {}
        Err(error) if error.kind() == std::io::ErrorKind::NotFound && !options.check => {
            fs::create_dir_all(&target)
                .with_context(|| format!("creating plugin directory {}", target.display()))?;
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
        Err(error) => {
            return Err(error)
                .with_context(|| format!("inspecting plugin target {}", target.display()));
        }
    }

    let mut files = Vec::with_capacity(definition.files.len());
    for managed in definition.files {
        let destination = target.join(managed.path);
        let action = match fs::symlink_metadata(&destination) {
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                if options.check {
                    InstallAction::WouldInstall
                } else {
                    write_atomic(&destination, managed.data).with_context(|| {
                        format!("installing managed file {}", destination.display())
                    })?;
                    InstallAction::Installed
                }
            }
            Err(error) => {
                return Err(error)
                    .with_context(|| format!("inspecting managed file {}", destination.display()));
            }
            Ok(metadata) if metadata.file_type().is_file() => {
                let installed = fs::read(&destination)
                    .with_context(|| format!("reading managed file {}", destination.display()))?;
                if hash_bytes(&installed) == hash_bytes(managed.data) {
                    InstallAction::Unchanged
                } else if !options.force {
                    InstallAction::Refused
                } else if options.check {
                    InstallAction::WouldReplace
                } else {
                    write_atomic(&destination, managed.data).with_context(|| {
                        format!("replacing managed file {}", destination.display())
                    })?;
                    InstallAction::Replaced
                }
            }
            Ok(metadata) if metadata.file_type().is_symlink() => {
                if !options.force {
                    InstallAction::Refused
                } else if options.check {
                    InstallAction::WouldReplace
                } else {
                    write_atomic(&destination, managed.data).with_context(|| {
                        format!("replacing managed symlink {}", destination.display())
                    })?;
                    InstallAction::Replaced
                }
            }
            Ok(_) => InstallAction::Refused,
        };
        files.push(InstallFileReport {
            path: managed.path.into(),
            action,
        });
    }
    Ok(InstallReport {
        plugin: definition.name.into(),
        target,
        files,
    })
}

pub fn file_hash(path: &Path) -> Result<String> {
    Ok(hash_bytes(&fs::read(path)?))
}

fn validate_definition(definition: Definition) -> Result<()> {
    if definition.name.is_empty() {
        bail!("plugin definition has no name");
    }
    if definition.files.is_empty() {
        bail!("plugin {} has no managed files", definition.name);
    }
    let mut previous = None;
    for file in definition.files {
        let path = Path::new(file.path);
        if file.path.is_empty()
            || path.is_absolute()
            || path
                .components()
                .any(|component| !matches!(component, Component::Normal(_)))
        {
            bail!(
                "plugin {} has unsafe managed path {:?}",
                definition.name,
                file.path
            );
        }
        if let Some(previous) = previous {
            if previous == file.path {
                bail!(
                    "plugin {} repeats managed path {:?}",
                    definition.name,
                    file.path
                );
            }
            if previous > file.path {
                bail!("plugin {} managed files must be sorted", definition.name);
            }
        }
        previous = Some(file.path);
    }
    Ok(())
}

fn normalize_target(target: &Path) -> Result<PathBuf> {
    if target.as_os_str().is_empty() {
        bail!("plugin target is empty");
    }
    if !target.is_absolute() {
        bail!("plugin target must be absolute: {}", target.display());
    }
    Ok(target.to_path_buf())
}

fn hash_bytes(data: &[u8]) -> String {
    format!("sha256:{:x}", Sha256::digest(data))
}

fn write_atomic(path: &Path, data: &[u8]) -> Result<()> {
    let directory = path
        .parent()
        .ok_or_else(|| anyhow::anyhow!("managed file has no parent directory"))?;
    let temporary = directory.join(format!(".noema-plugin-{}.tmp", ulid::Ulid::new()));
    let result = (|| -> Result<()> {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temporary)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            file.set_permissions(fs::Permissions::from_mode(0o644))?;
        }
        file.write_all(data)?;
        file.sync_all()?;
        drop(file);
        fs::rename(&temporary, path)?;
        Ok(())
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temporary);
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;

    static TEST_FILES: &[ManagedFile] = &[
        ManagedFile {
            path: "a.txt",
            data: b"embedded-a\n",
        },
        ManagedFile {
            path: "b.txt",
            data: b"embedded-b\n",
        },
    ];
    const TEST: Definition = Definition {
        name: "test",
        description: "test plugin",
        files: TEST_FILES,
    };

    #[test]
    fn inspect_install_check_force_and_extra_files_match_contract() {
        let temp = tempfile::tempdir().unwrap();
        let target = temp.path().join("plugin");
        assert_eq!(inspect(TEST, &target).unwrap().state, State::NotInstalled);

        let check = install(
            TEST,
            &target,
            InstallOptions {
                check: true,
                force: false,
            },
        )
        .unwrap();
        assert!(check.pending());
        assert!(!target.exists());

        let first = install(TEST, &target, InstallOptions::default()).unwrap();
        assert!(
            first
                .files
                .iter()
                .all(|file| file.action == InstallAction::Installed)
        );
        fs::write(target.join("extra.txt"), "operator data\n").unwrap();
        fs::write(target.join("a.txt"), "local override\n").unwrap();
        let drift = inspect(TEST, &target).unwrap();
        assert_eq!(drift.state, State::Drift);
        assert_eq!(drift.files[0].state, FileState::Changed);

        let refused = install(TEST, &target, InstallOptions::default()).unwrap();
        assert!(refused.refused());
        assert_eq!(
            fs::read_to_string(target.join("a.txt")).unwrap(),
            "local override\n"
        );
        let replaced = install(
            TEST,
            &target,
            InstallOptions {
                check: false,
                force: true,
            },
        )
        .unwrap();
        assert_eq!(replaced.files[0].action, InstallAction::Replaced);
        assert_eq!(
            fs::read_to_string(target.join("extra.txt")).unwrap(),
            "operator data\n"
        );
        assert_eq!(inspect(TEST, &target).unwrap().state, State::UpToDate);
    }

    #[cfg(unix)]
    #[test]
    fn force_replaces_symlink_without_following_it() {
        use std::os::unix::fs::symlink;

        let temp = tempfile::tempdir().unwrap();
        let target = temp.path().join("plugin");
        fs::create_dir(&target).unwrap();
        let outside = temp.path().join("outside.txt");
        fs::write(&outside, "outside\n").unwrap();
        symlink(&outside, target.join("a.txt")).unwrap();
        fs::write(target.join("b.txt"), b"embedded-b\n").unwrap();
        let report = install(
            TEST,
            &target,
            InstallOptions {
                check: false,
                force: true,
            },
        )
        .unwrap();
        assert_eq!(report.files[0].action, InstallAction::Replaced);
        assert_eq!(fs::read_to_string(&outside).unwrap(), "outside\n");
        assert_eq!(
            fs::read_to_string(target.join("a.txt")).unwrap(),
            "embedded-a\n"
        );
    }
}
