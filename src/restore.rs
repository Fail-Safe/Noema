use std::{
    collections::BTreeSet,
    ffi::{OsStr, OsString},
    fs::{self, File, OpenOptions},
    io::{self, Read, Write},
    path::{Component, Path, PathBuf},
    time::UNIX_EPOCH,
};

use anyhow::{Context, Result, bail};
use flate2::{Compression, read::GzDecoder, write::GzEncoder};
use fs2::FileExt;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::{
    config::{self, Config, CortexEntry},
    cortex::{read_manifest, write_manifest},
};

const RESTORE_TRANSACTION_VERSION: u32 = 1;
const RESTORE_TRANSACTION_DIRECTORY: &str = "restore-transactions";

#[derive(Debug, Clone, Default)]
pub struct RestoreOptions {
    pub name: Option<String>,
    pub parent: Option<PathBuf>,
    pub force: bool,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RestoreResult {
    pub name: String,
    pub path: PathBuf,
    pub id: String,
    pub is_default: bool,
    pub retained_backup: Option<PathBuf>,
    pub retained_transaction: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RestorePhase {
    Prepared,
    IncomingReady,
    DestinationPreserved,
    RestorePlaced,
    ConfigSaved,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RestoreTransactionState {
    Resumable,
    RollbackOnly,
    CommittedCleanup,
    Ambiguous,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RestoreTransactionSummary {
    pub id: String,
    pub name: String,
    pub phase: RestorePhase,
    pub state: RestoreTransactionState,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct RestoreTransactionReport {
    pub transactions: Vec<RestoreTransactionSummary>,
    pub malformed: usize,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RestoreRecoveryAction {
    Resume,
    Rollback,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct RestoreTransaction {
    version: u32,
    id: String,
    name: String,
    cortex_id: String,
    final_parent: PathBuf,
    had_destination: bool,
    restored_hash: String,
    previous_hash: Option<String>,
    previous_default: String,
    phase: RestorePhase,
}

pub fn backup(source: &Path, output: &Path, force: bool) -> Result<u64> {
    let source_metadata = fs::metadata(source)
        .with_context(|| format!("reading cortex directory {}", source.display()))?;
    if !source_metadata.is_dir() {
        bail!("cortex path {} is not a directory", source.display())
    }
    match fs::symlink_metadata(output) {
        Ok(_) if !force => {
            bail!(
                "output file {} already exists (use --force to replace it)",
                output.display()
            )
        }
        Ok(metadata) if !metadata.is_file() => {
            bail!("backup output {} is not a regular file", output.display())
        }
        Ok(_) => {}
        Err(error) if error.kind() == io::ErrorKind::NotFound => {}
        Err(error) => return Err(error).context("inspecting backup output"),
    }
    let parent = output
        .parent()
        .filter(|path| !path.as_os_str().is_empty())
        .unwrap_or_else(|| Path::new("."));
    let source_root = source.canonicalize()?;
    let output_name = output
        .file_name()
        .ok_or_else(|| anyhow::anyhow!("backup output has no file name"))?;
    let output_path = parent
        .canonicalize()
        .with_context(|| format!("resolving backup output parent {}", parent.display()))?
        .join(output_name);
    if output_path.starts_with(&source_root) {
        bail!("backup output must be outside the cortex directory")
    }
    let temporary = parent.join(format!(".noema-backup-{}.tmp", ulid::Ulid::new()));
    let write_result = (|| -> Result<()> {
        let file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temporary)?;
        set_file_permissions(&temporary, 0o640)?;
        let encoder = GzEncoder::new(file, Compression::default());
        let mut builder = tar::Builder::new(encoder);
        let root_name = source
            .file_name()
            .ok_or_else(|| anyhow::anyhow!("cortex directory has no final path component"))?;
        append_backup_entry(&mut builder, source, Path::new(root_name))?;
        let encoder = builder.into_inner()?;
        let file = encoder.finish()?;
        file.sync_all()?;
        Ok(())
    })();
    if let Err(error) = write_result {
        let _ = fs::remove_file(&temporary);
        return Err(error).context("writing backup archive");
    }

    let previous = output
        .exists()
        .then(|| unique_sibling(output, "previous-backup"));
    if let Some(previous) = &previous {
        fs::rename(output, previous).context("preserving previous backup output")?;
    }
    if let Err(error) = fs::rename(&temporary, output) {
        if let Some(previous) = &previous {
            let _ = fs::rename(previous, output);
        }
        let _ = fs::remove_file(&temporary);
        return Err(error).context("placing backup archive");
    }
    if let Some(previous) = previous {
        let _ = fs::remove_file(previous);
    }
    sync_directory(parent)?;
    Ok(fs::metadata(output)?.len())
}

fn append_backup_entry(
    builder: &mut tar::Builder<GzEncoder<File>>,
    source: &Path,
    archive_path: &Path,
) -> Result<()> {
    let metadata = fs::symlink_metadata(source)?;
    let mut header = tar::Header::new_gnu();
    header.set_uid(0);
    header.set_gid(0);
    header.set_username("")?;
    header.set_groupname("")?;
    header.set_mtime(
        metadata
            .modified()
            .ok()
            .and_then(|value| value.duration_since(UNIX_EPOCH).ok())
            .map_or(0, |duration| duration.as_secs()),
    );
    if metadata.is_dir() {
        header.set_entry_type(tar::EntryType::Directory);
        header.set_size(0);
        header.set_mode(metadata_mode(&metadata, 0o750));
        builder.append_data(&mut header, archive_path, io::empty())?;
        let mut entries = fs::read_dir(source)?.collect::<io::Result<Vec<_>>>()?;
        entries.sort_by_key(fs::DirEntry::file_name);
        for entry in entries {
            append_backup_entry(
                builder,
                &entry.path(),
                &archive_path.join(entry.file_name()),
            )?;
        }
    } else if metadata.is_file() {
        header.set_entry_type(tar::EntryType::Regular);
        header.set_size(metadata.len());
        header.set_mode(metadata_mode(&metadata, 0o640));
        let mut file = File::open(source)?;
        builder.append_data(&mut header, archive_path, &mut file)?;
    } else if metadata.file_type().is_symlink()
        && is_legacy_trash_alias(archive_path, &fs::read_link(source)?)
    {
        // Go backups included this Obsidian-facing convenience alias, while Go
        // restore ignored it. The managed trash contents are already archived
        // under trash/traces, so omit the alias without accepting general links.
    } else {
        bail!("refusing to back up non-regular entry {}", source.display())
    }
    Ok(())
}

fn is_legacy_trash_alias(path: &Path, target: &Path) -> bool {
    is_legacy_trash_alias_path(path) && target == Path::new("trash/traces")
}

fn is_legacy_trash_alias_path(path: &Path) -> bool {
    let mut components = path.components();
    matches!(components.next(), Some(Component::Normal(_)))
        && matches!(
            components.next(),
            Some(Component::Normal(value)) if value == OsStr::new(".trash")
        )
        && components.next().is_none()
}

struct ScratchDirectory {
    path: PathBuf,
}

impl ScratchDirectory {
    fn create() -> Result<Self> {
        let parent = std::env::temp_dir();
        for _ in 0..4 {
            let path = parent.join(format!("noema-restore-{}", ulid::Ulid::new()));
            match fs::create_dir(&path) {
                Ok(()) => {
                    set_directory_permissions(&path, 0o700)?;
                    return Ok(Self { path });
                }
                Err(error) if error.kind() == io::ErrorKind::AlreadyExists => continue,
                Err(error) => return Err(error).context("creating restore staging directory"),
            }
        }
        bail!("could not allocate a unique restore staging directory")
    }
}

impl Drop for ScratchDirectory {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.path);
    }
}

fn restore_transaction_directory() -> Result<PathBuf> {
    let config_path = config::path()?;
    let parent = config_path
        .parent()
        .ok_or_else(|| anyhow::anyhow!("Noema config path has no parent directory"))?;
    Ok(parent.join(RESTORE_TRANSACTION_DIRECTORY))
}

fn restore_transaction_path(directory: &Path, id: &str) -> Result<PathBuf> {
    ulid::Ulid::from_string(id).context("invalid restore transaction ID")?;
    Ok(directory.join(format!("{id}.json")))
}

fn lock_restore_target(directory: &Path, final_path: &Path) -> Result<File> {
    fs::create_dir_all(directory)?;
    set_directory_permissions(directory, 0o700)?;
    let digest = Sha256::digest(final_path.as_os_str().as_encoded_bytes());
    let path = directory.join(format!("target-{digest:x}.lock"));
    let file = OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(&path)?;
    set_file_permissions(&path, 0o600)?;
    match file.try_lock_exclusive() {
        Ok(()) => Ok(file),
        Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
            bail!("a restore for this destination is already in progress")
        }
        Err(error) => Err(error).context("locking restore destination"),
    }
}

fn save_restore_transaction(directory: &Path, transaction: &RestoreTransaction) -> Result<()> {
    validate_restore_transaction(transaction)?;
    fs::create_dir_all(directory)?;
    set_directory_permissions(directory, 0o700)?;
    let path = restore_transaction_path(directory, &transaction.id)?;
    write_restore_transaction_atomic(&path, &serde_json::to_vec(transaction)?)
        .context("writing restore transaction")
}

fn write_restore_transaction_atomic(path: &Path, bytes: &[u8]) -> Result<()> {
    let directory = path
        .parent()
        .ok_or_else(|| anyhow::anyhow!("restore transaction has no parent directory"))?;
    if path.exists() {
        drop(OpenOptions::new().write(true).open(path)?);
    }
    let temporary = directory.join(format!(".noema-restore-journal-{}.tmp", ulid::Ulid::new()));
    let result = (|| -> Result<()> {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temporary)?;
        set_file_permissions(&temporary, 0o600)?;
        file.write_all(bytes)?;
        file.sync_all()?;
        drop(file);
        fs::rename(&temporary, path)?;
        sync_directory(directory)
    })();
    if result.is_err() {
        let _ = fs::remove_file(temporary);
    }
    result
}

fn remove_restore_transaction(directory: &Path, id: &str) -> Result<()> {
    let path = restore_transaction_path(directory, id)?;
    match fs::remove_file(&path) {
        Ok(()) => sync_directory(path.parent().unwrap()),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error).context("removing restore transaction"),
    }
}

fn validate_restore_transaction(transaction: &RestoreTransaction) -> Result<()> {
    if transaction.version != RESTORE_TRANSACTION_VERSION {
        bail!(
            "unsupported restore transaction version {}",
            transaction.version
        )
    }
    ulid::Ulid::from_string(&transaction.id).context("invalid restore transaction ID")?;
    validate_name(&transaction.name).context("invalid restore transaction cortex name")?;
    if !transaction.cortex_id.is_empty() {
        ulid::Ulid::from_string(&transaction.cortex_id)
            .context("invalid restore transaction cortex ID")?;
    }
    if !transaction.final_parent.is_absolute() {
        bail!("restore transaction parent is not absolute")
    }
    let valid_hash = |value: &str| {
        value.strip_prefix("sha256:").is_some_and(|digest| {
            digest.len() == 64 && digest.bytes().all(|byte| byte.is_ascii_hexdigit())
        })
    };
    if !valid_hash(&transaction.restored_hash)
        || transaction
            .previous_hash
            .as_deref()
            .is_some_and(|value| !valid_hash(value))
        || transaction.had_destination != transaction.previous_hash.is_some()
    {
        bail!("restore transaction has invalid content hashes")
    }
    Ok(())
}

fn transaction_artifact_path(transaction: &RestoreTransaction, purpose: &str) -> PathBuf {
    transaction
        .final_parent
        .join(format!(".noema-restore-{purpose}-{}", transaction.id))
}

fn tree_hash(root: &Path) -> Result<String> {
    let mut hasher = Sha256::new();
    hash_tree_entry(root, root, &mut hasher)?;
    Ok(format!("sha256:{:x}", hasher.finalize()))
}

fn hash_tree_entry(root: &Path, path: &Path, hasher: &mut Sha256) -> Result<()> {
    let relative = path.strip_prefix(root)?;
    let encoded = relative.as_os_str().as_encoded_bytes();
    hasher.update((encoded.len() as u64).to_be_bytes());
    hasher.update(encoded);
    let metadata = fs::symlink_metadata(path)?;
    hasher.update(metadata_mode(&metadata, 0).to_be_bytes());
    if metadata.is_dir() {
        hasher.update(b"directory");
        let mut entries = fs::read_dir(path)?.collect::<io::Result<Vec<_>>>()?;
        entries.sort_by_key(fs::DirEntry::file_name);
        for entry in entries {
            hash_tree_entry(root, &entry.path(), hasher)?;
        }
    } else if metadata.is_file() {
        hasher.update(b"file");
        hasher.update(metadata.len().to_be_bytes());
        let mut file = File::open(path)?;
        let mut buffer = [0_u8; 64 * 1024];
        loop {
            let count = file.read(&mut buffer)?;
            if count == 0 {
                break;
            }
            hasher.update(&buffer[..count]);
        }
    } else {
        bail!(
            "restore transaction cannot protect non-regular entry {}",
            path.display()
        )
    }
    Ok(())
}

fn load_restore_transaction(directory: &Path, path: &Path) -> Result<RestoreTransaction> {
    let bytes = fs::read(path).context("reading restore transaction")?;
    let transaction = serde_json::from_slice::<RestoreTransaction>(&bytes)
        .map_err(|_| anyhow::anyhow!("malformed restore transaction"))?;
    validate_restore_transaction(&transaction)
        .map_err(|_| anyhow::anyhow!("malformed restore transaction"))?;
    let expected = restore_transaction_path(directory, &transaction.id)?;
    if path != expected {
        bail!("restore transaction filename does not match its ID")
    }
    Ok(transaction)
}

fn ensure_no_conflicting_restore_transaction(
    directory: &Path,
    name: &str,
    parent: &Path,
) -> Result<()> {
    let entries = match fs::read_dir(directory) {
        Ok(entries) => entries,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(error).context("reading restore transactions"),
    };
    for entry in entries {
        let entry = entry?;
        if entry.path().extension().and_then(|value| value.to_str()) != Some("json") {
            continue;
        }
        let transaction = load_restore_transaction(directory, &entry.path())?;
        if transaction.name == name && transaction.final_parent == parent {
            bail!(
                "restore transaction {} is already pending for cortex {:?}; inspect or recover it before restoring again",
                transaction.id,
                name
            )
        }
    }
    Ok(())
}

fn path_exists(path: &Path) -> Result<bool> {
    match fs::symlink_metadata(path) {
        Ok(_) => Ok(true),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(false),
        Err(error) => Err(error.into()),
    }
}

fn path_matches_hash(path: &Path, expected: &str) -> bool {
    tree_hash(path).is_ok_and(|value| value == expected)
}

fn config_entry_matches(config: &Config, transaction: &RestoreTransaction) -> Result<bool> {
    let Some(entry) = config.cortexes.get(&transaction.name) else {
        return Ok(false);
    };
    if entry.id != transaction.cortex_id
        || !paths_match(
            &entry.path,
            &transaction.final_parent.join(&transaction.name),
        )
    {
        bail!("registered cortex conflicts with restore transaction")
    }
    Ok(true)
}

fn classify_restore_transaction(
    config: &Config,
    transaction: &RestoreTransaction,
) -> RestoreTransactionState {
    let final_path = transaction.final_parent.join(&transaction.name);
    let incoming = transaction_artifact_path(transaction, "incoming");
    let backup = transaction_artifact_path(transaction, "backup");
    let config_matches = match config_entry_matches(config, transaction) {
        Ok(value) => value,
        Err(_) => return RestoreTransactionState::Ambiguous,
    };
    let final_is_restored = path_matches_hash(&final_path, &transaction.restored_hash);
    let incoming_is_restored = path_matches_hash(&incoming, &transaction.restored_hash);
    let backup_is_previous = transaction
        .previous_hash
        .as_deref()
        .is_some_and(|hash| path_matches_hash(&backup, hash));

    if config_matches {
        return if final_is_restored {
            RestoreTransactionState::CommittedCleanup
        } else {
            RestoreTransactionState::Ambiguous
        };
    }
    if final_is_restored || incoming_is_restored {
        return RestoreTransactionState::Resumable;
    }
    if backup_is_previous
        || (transaction.had_destination
            && transaction
                .previous_hash
                .as_deref()
                .is_some_and(|hash| path_matches_hash(&final_path, hash)))
        || (!transaction.had_destination
            && matches!(transaction.phase, RestorePhase::Prepared)
            && !final_path.exists()
            && !incoming.exists())
    {
        return RestoreTransactionState::RollbackOnly;
    }
    RestoreTransactionState::Ambiguous
}

pub fn inspect_restore_transactions(config: &Config) -> Result<RestoreTransactionReport> {
    let directory = restore_transaction_directory()?;
    let entries = match fs::read_dir(&directory) {
        Ok(entries) => entries,
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            return Ok(RestoreTransactionReport::default());
        }
        Err(error) => return Err(error).context("reading restore transactions"),
    };
    let mut paths = entries
        .collect::<io::Result<Vec<_>>>()?
        .into_iter()
        .map(|entry| entry.path())
        .filter(|path| path.extension().and_then(|value| value.to_str()) == Some("json"))
        .collect::<Vec<_>>();
    paths.sort();
    let mut report = RestoreTransactionReport::default();
    for path in paths {
        match load_restore_transaction(&directory, &path) {
            Ok(transaction) => report.transactions.push(RestoreTransactionSummary {
                id: transaction.id.clone(),
                name: transaction.name.clone(),
                phase: transaction.phase,
                state: classify_restore_transaction(config, &transaction),
            }),
            Err(_) => report.malformed += 1,
        }
    }
    Ok(report)
}

fn require_hash(path: &Path, expected: &str, label: &str) -> Result<()> {
    if !path_matches_hash(path, expected) {
        bail!("{label} does not match the restore transaction hash")
    }
    Ok(())
}

fn remove_hashed_path(path: &Path, expected: &str, label: &str) -> Result<()> {
    if !path_exists(path)? {
        return Ok(());
    }
    require_hash(path, expected, label)?;
    remove_path(path).with_context(|| format!("removing {label}"))
}

pub fn recover_restore_transaction(
    config: &mut Config,
    id: &str,
    action: RestoreRecoveryAction,
) -> Result<()> {
    let directory = restore_transaction_directory()?;
    let path = restore_transaction_path(&directory, id)?;
    let lock_path = directory.join(format!("{id}.lock"));
    let lock_file = OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(&lock_path)?;
    lock_file
        .try_lock_exclusive()
        .context("locking restore transaction")?;
    let mut transaction = load_restore_transaction(&directory, &path)?;
    let _target_lock = lock_restore_target(
        &directory,
        &transaction.final_parent.join(&transaction.name),
    )?;
    match action {
        RestoreRecoveryAction::Resume => {
            resume_restore_transaction(config, &directory, &mut transaction)?
        }
        RestoreRecoveryAction::Rollback => {
            rollback_restore_transaction(config, &directory, &transaction)?
        }
    }
    drop(lock_file);
    let _ = fs::remove_file(lock_path);
    Ok(())
}

fn resume_restore_transaction(
    config: &mut Config,
    directory: &Path,
    transaction: &mut RestoreTransaction,
) -> Result<()> {
    let final_path = transaction.final_parent.join(&transaction.name);
    let incoming = transaction_artifact_path(transaction, "incoming");
    let backup = transaction_artifact_path(transaction, "backup");
    let config_matches = config_entry_matches(config, transaction)?;

    if !config_matches {
        let incoming_ready = path_matches_hash(&incoming, &transaction.restored_hash);
        let final_ready = path_matches_hash(&final_path, &transaction.restored_hash);
        if incoming_ready {
            if path_exists(&final_path)? && !final_ready {
                let previous_hash = transaction.previous_hash.as_deref().ok_or_else(|| {
                    anyhow::anyhow!("unexpected destination blocks restore resume")
                })?;
                require_hash(&final_path, previous_hash, "existing destination")?;
                if path_exists(&backup)? {
                    bail!("both destination and preserved backup exist during restore resume")
                }
                fs::rename(&final_path, &backup)?;
            }
            if !path_exists(&final_path)? {
                fs::rename(&incoming, &final_path)?;
            }
        } else if !final_ready {
            bail!("restore transaction has no intact restored tree to resume")
        }
        require_hash(
            &final_path,
            &transaction.restored_hash,
            "restored destination",
        )?;
        transaction.phase = RestorePhase::RestorePlaced;
        save_restore_transaction(directory, transaction)?;
        config.cortexes.insert(
            transaction.name.clone(),
            CortexEntry {
                path: final_path.clone(),
                id: transaction.cortex_id.clone(),
            },
        );
        if config.default.is_empty() {
            config.default.clone_from(&transaction.name);
        }
        config.save()?;
        transaction.phase = RestorePhase::ConfigSaved;
        save_restore_transaction(directory, transaction)?;
    } else {
        require_hash(
            &final_path,
            &transaction.restored_hash,
            "restored destination",
        )?;
    }

    if let Some(previous_hash) = transaction.previous_hash.as_deref() {
        remove_hashed_path(&backup, previous_hash, "preserved destination")?;
    }
    remove_hashed_path(
        &incoming,
        &transaction.restored_hash,
        "incoming restored tree",
    )?;
    remove_restore_transaction(directory, &transaction.id)
}

fn rollback_restore_transaction(
    config: &mut Config,
    directory: &Path,
    transaction: &RestoreTransaction,
) -> Result<()> {
    let final_path = transaction.final_parent.join(&transaction.name);
    let incoming = transaction_artifact_path(transaction, "incoming");
    let backup = transaction_artifact_path(transaction, "backup");
    let discarded = transaction_artifact_path(transaction, "rollback");
    let config_matches = config_entry_matches(config, transaction)?;

    if transaction.had_destination {
        let previous_hash = transaction.previous_hash.as_deref().unwrap();
        if path_exists(&backup)? {
            require_hash(&backup, previous_hash, "preserved destination")?;
            if path_exists(&final_path)? {
                require_hash(
                    &final_path,
                    &transaction.restored_hash,
                    "restored destination",
                )?;
                if path_exists(&discarded)? {
                    bail!("restore rollback discard path already exists")
                }
                fs::rename(&final_path, &discarded)?;
            }
            fs::rename(&backup, &final_path)?;
        } else {
            require_hash(&final_path, previous_hash, "original destination")?;
        }
    } else if path_exists(&final_path)? {
        require_hash(
            &final_path,
            &transaction.restored_hash,
            "restored destination",
        )?;
        fs::rename(&final_path, &discarded)?;
    }

    remove_hashed_path(
        &incoming,
        &transaction.restored_hash,
        "incoming restored tree",
    )?;
    if config_matches {
        config.cortexes.remove(&transaction.name);
        if config.default == transaction.name {
            config.default = if transaction.previous_default.is_empty()
                || config.cortexes.contains_key(&transaction.previous_default)
            {
                transaction.previous_default.clone()
            } else {
                String::new()
            };
        }
        config.save()?;
    }
    remove_hashed_path(
        &discarded,
        &transaction.restored_hash,
        "rolled-back restored tree",
    )?;
    remove_restore_transaction(directory, &transaction.id)
}

pub fn restore(
    config: &mut Config,
    tarball: &Path,
    options: &RestoreOptions,
) -> Result<RestoreResult> {
    let transaction_directory = restore_transaction_directory()?;
    restore_with_save(
        config,
        tarball,
        options,
        &transaction_directory,
        Config::save,
    )
}

fn restore_with_save<F>(
    config: &mut Config,
    tarball: &Path,
    options: &RestoreOptions,
    transaction_directory: &Path,
    save: F,
) -> Result<RestoreResult>
where
    F: FnOnce(&Config) -> Result<()>,
{
    fs::metadata(tarball)
        .with_context(|| format!("reading restore archive {}", tarball.display()))?;
    let scratch = ScratchDirectory::create()?;
    let staged_cortex = extract_archive(tarball, &scratch.path)
        .with_context(|| format!("extracting restore archive {}", tarball.display()))?;
    let mut manifest = read_manifest(&staged_cortex).context("reading cortex.md from archive")?;
    validate_name(&manifest.name).context("invalid cortex name in archive")?;
    if !manifest.id.is_empty() {
        ulid::Ulid::from_string(&manifest.id).context("invalid cortex ID in archive")?;
    }

    let final_name = options.name.as_deref().unwrap_or(&manifest.name).to_owned();
    validate_name(&final_name).context("invalid restored cortex name")?;
    if let Some(existing) = config.cortexes.get(&final_name) {
        bail!(
            "cortex name {final_name:?} is already registered at {}",
            existing.path.display()
        )
    }
    if !manifest.id.is_empty()
        && let Some((existing_name, existing)) = config
            .cortexes
            .iter()
            .find(|(_, entry)| entry.id == manifest.id)
    {
        bail!(
            "cortex ID {} is already registered as {:?} at {}; restore under a new name and reset its cortex ID before running both copies",
            manifest.id,
            existing_name,
            existing.path.display()
        )
    }

    if final_name != manifest.name {
        manifest.name.clone_from(&final_name);
        write_manifest(&staged_cortex, &manifest).context("rewriting restored cortex name")?;
    }

    let final_parent = match &options.parent {
        Some(parent) => absolute_path(parent)?,
        None => {
            let home =
                std::env::var_os("HOME").ok_or_else(|| anyhow::anyhow!("HOME is not set"))?;
            PathBuf::from(home).join(".noema")
        }
    };
    let final_path = final_parent.join(&final_name);
    for (registered_name, entry) in &config.cortexes {
        if paths_match(&entry.path, &final_path) {
            bail!(
                "restore destination {} is already registered as {:?}",
                final_path.display(),
                registered_name
            )
        }
    }
    let existing_destination = match fs::symlink_metadata(&final_path) {
        Ok(_) if !options.force => {
            bail!(
                "destination {} already exists (use --force to replace it)",
                final_path.display()
            )
        }
        Ok(_) => true,
        Err(error) if error.kind() == io::ErrorKind::NotFound => false,
        Err(error) => return Err(error).context("inspecting restore destination"),
    };
    let restored_hash = tree_hash(&staged_cortex).context("hashing staged restored cortex")?;
    let previous_hash = existing_destination
        .then(|| tree_hash(&final_path).context("hashing existing restore destination"))
        .transpose()?;

    fs::create_dir_all(&final_parent)
        .with_context(|| format!("creating restore parent {}", final_parent.display()))?;
    let _target_lock = lock_restore_target(transaction_directory, &final_path)?;
    ensure_no_conflicting_restore_transaction(transaction_directory, &final_name, &final_parent)?;
    let mut transaction = RestoreTransaction {
        version: RESTORE_TRANSACTION_VERSION,
        id: ulid::Ulid::new().to_string(),
        name: final_name.clone(),
        cortex_id: manifest.id.clone(),
        final_parent: final_parent.clone(),
        had_destination: existing_destination,
        restored_hash,
        previous_hash,
        previous_default: config.default.clone(),
        phase: RestorePhase::Prepared,
    };
    save_restore_transaction(transaction_directory, &transaction)?;
    let incoming = transaction_artifact_path(&transaction, "incoming");
    let backup = existing_destination.then(|| transaction_artifact_path(&transaction, "backup"));
    if let Err(error) = move_directory(&staged_cortex, &incoming) {
        let _ = remove_restore_transaction(transaction_directory, &transaction.id);
        return Err(error).context("staging restored cortex for placement");
    }
    transaction.phase = RestorePhase::IncomingReady;
    save_restore_transaction(transaction_directory, &transaction)?;

    if let Some(backup) = &backup
        && let Err(error) = fs::rename(&final_path, backup)
    {
        let _ = fs::remove_dir_all(&incoming);
        let _ = remove_restore_transaction(transaction_directory, &transaction.id);
        return Err(error).context("preserving existing restore destination");
    }
    transaction.phase = RestorePhase::DestinationPreserved;
    save_restore_transaction(transaction_directory, &transaction)?;
    pause_restore_for_test("destination-preserved");
    if let Err(error) = fs::rename(&incoming, &final_path) {
        let rollback = backup
            .as_ref()
            .map(|backup| fs::rename(backup, &final_path))
            .transpose();
        if let Err(rollback_error) = rollback {
            return Err(anyhow::Error::new(error).context(format!(
                "placing restored cortex failed and restoring the previous destination also failed: {rollback_error}"
            )));
        }
        let _ = fs::remove_dir_all(&incoming);
        let _ = remove_restore_transaction(transaction_directory, &transaction.id);
        return Err(error).context("placing restored cortex");
    }
    transaction.phase = RestorePhase::RestorePlaced;
    save_restore_transaction(transaction_directory, &transaction)?;
    pause_restore_for_test("restore-placed");

    let previous_config = config.clone();
    config.cortexes.insert(
        final_name.clone(),
        CortexEntry {
            path: final_path.clone(),
            id: manifest.id.clone(),
        },
    );
    if config.default.is_empty() {
        config.default.clone_from(&final_name);
    }
    if let Err(error) = save(config) {
        *config = previous_config;
        let failed_restore = transaction_artifact_path(&transaction, "failed");
        let rollback = (|| -> Result<()> {
            fs::rename(&final_path, &failed_restore)?;
            if let Some(backup) = &backup {
                fs::rename(backup, &final_path)?;
            }
            remove_path(&failed_restore)?;
            remove_restore_transaction(transaction_directory, &transaction.id)?;
            Ok(())
        })();
        if let Err(rollback_error) = rollback {
            return Err(error.context(format!(
                "saving configuration failed and restoring the previous destination also failed: {rollback_error}"
            )));
        }
        return Err(error).context("saving restored cortex configuration");
    }
    transaction.phase = RestorePhase::ConfigSaved;
    save_restore_transaction(transaction_directory, &transaction)?;
    pause_restore_for_test("config-saved");

    let retained_backup = backup.and_then(|backup| match remove_path(&backup) {
        Ok(()) => None,
        Err(_) => Some(backup),
    });
    let retained_transaction = if retained_backup.is_none()
        && remove_restore_transaction(transaction_directory, &transaction.id).is_ok()
    {
        None
    } else {
        Some(transaction.id.clone())
    };
    Ok(RestoreResult {
        name: final_name.clone(),
        path: final_path,
        id: manifest.id,
        is_default: config.default == final_name,
        retained_backup,
        retained_transaction,
    })
}

fn extract_archive(tarball: &Path, destination: &Path) -> Result<PathBuf> {
    let file = File::open(tarball)?;
    let decoder = GzDecoder::new(file);
    let mut archive = tar::Archive::new(decoder);
    let mut top_levels = BTreeSet::<OsString>::new();
    let mut seen_paths = BTreeSet::<PathBuf>::new();
    let mut directory_modes = Vec::<(PathBuf, u32)>::new();

    for entry in archive.entries().context("reading tar archive")? {
        let mut entry = entry.context("reading tar entry")?;
        let relative = entry
            .path()
            .context("decoding tar entry path")?
            .into_owned();
        validate_archive_path(&relative)?;
        if !seen_paths.insert(relative.clone()) {
            bail!("tar entry {} appears more than once", relative.display())
        }
        let top = relative
            .components()
            .next()
            .and_then(|component| match component {
                Component::Normal(value) => Some(value.to_os_string()),
                _ => None,
            })
            .ok_or_else(|| anyhow::anyhow!("tar entry has no top-level directory"))?;
        top_levels.insert(top);
        let target = destination.join(&relative);
        let entry_type = entry.header().entry_type();
        let mode = entry.header().mode().unwrap_or(0o640) & 0o777;
        if entry_type.is_dir() {
            fs::create_dir_all(&target)
                .with_context(|| format!("creating archive directory {}", relative.display()))?;
            directory_modes.push((target, mode));
        } else if entry_type.is_file() {
            let parent = target
                .parent()
                .ok_or_else(|| anyhow::anyhow!("archive file has no parent"))?;
            fs::create_dir_all(parent)?;
            let mut output = OpenOptions::new()
                .write(true)
                .create_new(true)
                .open(&target)
                .with_context(|| format!("creating archive file {}", relative.display()))?;
            io::copy(&mut entry, &mut output)
                .with_context(|| format!("extracting archive file {}", relative.display()))?;
            output.sync_all()?;
            set_file_permissions(&target, mode)?;
        } else if entry_type.is_symlink()
            && is_legacy_trash_alias_path(&relative)
            && entry
                .link_name()
                .context("reading tar link target")?
                .as_deref()
                .is_none_or(|link| link.as_os_str().is_empty() || link == Path::new("trash/traces"))
        {
            // Go wrote the legacy Obsidian-facing .trash alias into archives
            // and ignored it on restore. Preserve that behavior without
            // creating a link or accepting any other archive link type.
        } else {
            bail!(
                "tar entry {} has unsupported type {:?}",
                relative.display(),
                entry_type
            )
        }
    }
    if top_levels.len() != 1 {
        bail!("tarball must contain exactly one top-level cortex directory")
    }
    directory_modes.sort_by_key(|(path, _)| std::cmp::Reverse(path.components().count()));
    for (path, mode) in directory_modes {
        set_directory_permissions(&path, mode)?;
    }
    let top = top_levels.into_iter().next().unwrap();
    let root = destination.join(top);
    if !fs::metadata(&root)?.is_dir() {
        bail!("tarball top-level entry is not a directory")
    }
    Ok(root)
}

fn validate_archive_path(path: &Path) -> Result<()> {
    if path.as_os_str().is_empty()
        || !path
            .components()
            .all(|component| matches!(component, Component::Normal(_)))
    {
        bail!("tar entry {:?} has an unsafe path", path)
    }
    Ok(())
}

fn validate_name(name: &str) -> Result<()> {
    let path = Path::new(name);
    let mut components = path.components();
    if name.is_empty()
        || !matches!(components.next(), Some(Component::Normal(_)))
        || components.next().is_some()
    {
        bail!("cortex name must be one path-safe component")
    }
    Ok(())
}

fn absolute_path(path: &Path) -> Result<PathBuf> {
    std::path::absolute(path).with_context(|| format!("resolving {}", path.display()))
}

fn paths_match(left: &Path, right: &Path) -> bool {
    let left = left
        .canonicalize()
        .or_else(|_| absolute_path(left))
        .unwrap_or_else(|_| left.to_path_buf());
    let right = right
        .canonicalize()
        .or_else(|_| absolute_path(right))
        .unwrap_or_else(|_| right.to_path_buf());
    left == right
}

fn unique_sibling(path: &Path, purpose: &str) -> PathBuf {
    path.parent()
        .unwrap()
        .join(format!(".noema-restore-{purpose}-{}", ulid::Ulid::new()))
}

fn move_directory(source: &Path, target: &Path) -> Result<()> {
    match fs::rename(source, target) {
        Ok(()) => Ok(()),
        Err(_) => {
            if let Err(error) = copy_directory(source, target) {
                let _ = fs::remove_dir_all(target);
                return Err(error);
            }
            fs::remove_dir_all(source)?;
            Ok(())
        }
    }
}

fn remove_path(path: &Path) -> io::Result<()> {
    let metadata = fs::symlink_metadata(path)?;
    if metadata.is_dir() && !metadata.file_type().is_symlink() {
        fs::remove_dir_all(path)
    } else {
        fs::remove_file(path)
    }
}

fn copy_directory(source: &Path, target: &Path) -> Result<()> {
    let metadata = fs::symlink_metadata(source)?;
    if !metadata.is_dir() {
        bail!("restore source is not a directory")
    }
    fs::create_dir(target)?;
    for entry in fs::read_dir(source)? {
        let entry = entry?;
        let source_path = entry.path();
        let target_path = target.join(entry.file_name());
        let metadata = fs::symlink_metadata(&source_path)?;
        if metadata.is_dir() {
            copy_directory(&source_path, &target_path)?;
        } else if metadata.is_file() {
            fs::copy(&source_path, &target_path)?;
            fs::set_permissions(&target_path, metadata.permissions())?;
        } else {
            bail!("restore staging tree contains a non-regular entry")
        }
    }
    fs::set_permissions(target, metadata.permissions())?;
    Ok(())
}

#[cfg(unix)]
fn metadata_mode(metadata: &fs::Metadata, _fallback: u32) -> u32 {
    use std::os::unix::fs::PermissionsExt;
    metadata.permissions().mode() & 0o777
}

#[cfg(not(unix))]
fn metadata_mode(_metadata: &fs::Metadata, fallback: u32) -> u32 {
    fallback
}

#[cfg(unix)]
fn sync_directory(path: &Path) -> Result<()> {
    File::open(path)?.sync_all()?;
    Ok(())
}

#[cfg(not(unix))]
fn sync_directory(_path: &Path) -> Result<()> {
    Ok(())
}

#[cfg(unix)]
fn set_file_permissions(path: &Path, mode: u32) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(mode))?;
    Ok(())
}

#[cfg(not(unix))]
fn set_file_permissions(_path: &Path, _mode: u32) -> Result<()> {
    Ok(())
}

fn set_directory_permissions(path: &Path, mode: u32) -> Result<()> {
    set_file_permissions(path, mode)
}

#[cfg(debug_assertions)]
fn pause_restore_for_test(point: &str) {
    let Ok(requested) = std::env::var("NOEMA_RUST_TEST_RESTORE_PAUSE_POINT") else {
        return;
    };
    if requested != point {
        return;
    }
    let Some(marker) = std::env::var_os("NOEMA_RUST_TEST_RESTORE_PAUSE_MARKER") else {
        return;
    };
    if fs::write(marker, format!("{point}\n")).is_err() {
        return;
    }
    loop {
        std::thread::park();
    }
}

#[cfg(not(debug_assertions))]
fn pause_restore_for_test(_point: &str) {}

#[cfg(test)]
mod tests {
    use flate2::{Compression, write::GzEncoder};
    use tar::{Builder, EntryType, Header};

    use super::*;
    use crate::{cortex::Cortex, trace::Trace};

    fn append_bytes(
        builder: &mut Builder<GzEncoder<File>>,
        path: &str,
        bytes: &[u8],
    ) -> Result<()> {
        let mut header = Header::new_gnu();
        header.set_size(bytes.len() as u64);
        header.set_mode(0o640);
        header.set_entry_type(EntryType::Regular);
        header.set_cksum();
        builder.append_data(&mut header, path, bytes)?;
        Ok(())
    }

    fn archive_directory(source: &Path, output: &Path) -> Result<()> {
        backup(source, output, false).map(|_| ())
    }

    fn finish_archive(builder: Builder<GzEncoder<File>>) -> Result<()> {
        let encoder = builder.into_inner()?;
        encoder.finish()?;
        Ok(())
    }

    #[test]
    fn round_trip_preserves_identity_trace_and_relabels_manifest() {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("original", temp.path()).unwrap();
        let source = temp.path().join("original");
        let cortex = Cortex::open("original", &source).unwrap();
        let mut trace = Trace::new("Backup me", "fact", "", vec![], "preserved body");
        cortex.add(&mut trace).unwrap();
        let trace_id = trace.frontmatter.id.clone();
        let original_id = cortex.id.clone();
        drop(cortex);
        let archive = temp.path().join("backup.tar.gz");
        archive_directory(&source, &archive).unwrap();

        let destination = temp.path().join("restored");
        let mut config = Config::default();
        let result = restore_with_save(
            &mut config,
            &archive,
            &RestoreOptions {
                name: Some("renamed".into()),
                parent: Some(destination.clone()),
                force: false,
            },
            &temp.path().join("transactions"),
            |_| Ok(()),
        )
        .unwrap();

        assert_eq!(result.id, original_id);
        assert_eq!(result.name, "renamed");
        assert!(result.is_default);
        assert!(result.retained_backup.is_none());
        assert!(result.retained_transaction.is_none());
        let manifest = read_manifest(&result.path).unwrap();
        assert_eq!(manifest.name, "renamed");
        let restored = Cortex::open("renamed", &result.path).unwrap();
        assert_eq!(
            restored.get_trace(&trace_id).unwrap().1.body,
            "preserved body"
        );
    }

    #[test]
    fn rejects_path_traversal_before_writing_outside_staging() {
        let temp = tempfile::tempdir().unwrap();
        let archive = temp.path().join("unsafe.tar.gz");
        let file = File::create(&archive).unwrap();
        let encoder = GzEncoder::new(file, Compression::default());
        let mut builder = Builder::new(encoder);
        let mut header = Header::new_gnu();
        let bytes = b"outside";
        header.set_size(bytes.len() as u64);
        header.set_mode(0o640);
        header.set_entry_type(EntryType::Regular);
        header.as_mut_bytes()[..13].copy_from_slice(b"../escape.txt");
        header.set_cksum();
        builder.append(&header, &bytes[..]).unwrap();
        finish_archive(builder).unwrap();

        let staging = temp.path().join("staging");
        fs::create_dir(&staging).unwrap();
        assert!(extract_archive(&archive, &staging).is_err());
        assert!(!temp.path().join("escape.txt").exists());
    }

    #[test]
    fn rejects_links_and_multiple_top_level_directories() {
        let temp = tempfile::tempdir().unwrap();
        let link_archive = temp.path().join("link.tar.gz");
        let file = File::create(&link_archive).unwrap();
        let encoder = GzEncoder::new(file, Compression::default());
        let mut builder = Builder::new(encoder);
        let mut header = Header::new_gnu();
        header.set_size(0);
        header.set_mode(0o777);
        header.set_entry_type(EntryType::Symlink);
        header.set_link_name("../../outside").unwrap();
        header.set_cksum();
        builder
            .append_data(&mut header, "root/link", io::empty())
            .unwrap();
        finish_archive(builder).unwrap();
        let staging = temp.path().join("link-staging");
        fs::create_dir(&staging).unwrap();
        assert!(extract_archive(&link_archive, &staging).is_err());

        let multi_archive = temp.path().join("multi.tar.gz");
        let file = File::create(&multi_archive).unwrap();
        let encoder = GzEncoder::new(file, Compression::default());
        let mut builder = Builder::new(encoder);
        append_bytes(&mut builder, "one/a", b"a").unwrap();
        append_bytes(&mut builder, "two/b", b"b").unwrap();
        finish_archive(builder).unwrap();
        let staging = temp.path().join("multi-staging");
        fs::create_dir(&staging).unwrap();
        assert!(extract_archive(&multi_archive, &staging).is_err());
    }

    #[cfg(unix)]
    #[test]
    fn backup_omits_only_the_legacy_trash_alias() {
        use std::os::unix::fs::symlink;

        let temp = tempfile::tempdir().unwrap();
        Cortex::create("original", temp.path()).unwrap();
        let source = temp.path().join("original");
        symlink("trash/traces/", source.join(".trash")).unwrap();
        let archive = temp.path().join("backup.tar.gz");
        archive_directory(&source, &archive).unwrap();

        let file = File::open(&archive).unwrap();
        let decoder = GzDecoder::new(file);
        let mut archive = tar::Archive::new(decoder);
        let paths = archive
            .entries()
            .unwrap()
            .map(|entry| entry.unwrap().path().unwrap().into_owned())
            .collect::<Vec<_>>();
        assert!(!paths.iter().any(|path| path.ends_with(".trash")));
        assert!(paths.iter().any(|path| path.ends_with("trash/traces")));

        symlink("trash/traces/", source.join("other-link")).unwrap();
        assert!(archive_directory(&source, &temp.path().join("rejected.tar.gz")).is_err());
    }

    #[test]
    fn restore_ignores_exact_legacy_trash_alias() {
        let temp = tempfile::tempdir().unwrap();
        let archive = temp.path().join("legacy.tar.gz");
        let file = File::create(&archive).unwrap();
        let encoder = GzEncoder::new(file, Compression::default());
        let mut builder = Builder::new(encoder);
        for path in ["root", "root/trash", "root/trash/traces"] {
            let mut header = Header::new_gnu();
            header.set_size(0);
            header.set_mode(0o750);
            header.set_entry_type(EntryType::Directory);
            header.set_cksum();
            builder.append_data(&mut header, path, io::empty()).unwrap();
        }
        let mut header = Header::new_gnu();
        header.set_size(0);
        header.set_mode(0o777);
        header.set_entry_type(EntryType::Symlink);
        header.set_cksum();
        builder
            .append_data(&mut header, "root/.trash", io::empty())
            .unwrap();
        finish_archive(builder).unwrap();

        let staging = temp.path().join("staging");
        fs::create_dir(&staging).unwrap();
        let restored = extract_archive(&archive, &staging).unwrap();
        assert!(restored.join("trash/traces").is_dir());
        assert!(!restored.join(".trash").exists());
    }

    #[test]
    fn rejects_duplicate_archive_paths() {
        let temp = tempfile::tempdir().unwrap();
        let archive = temp.path().join("duplicate.tar.gz");
        let file = File::create(&archive).unwrap();
        let encoder = GzEncoder::new(file, Compression::default());
        let mut builder = Builder::new(encoder);
        for mode in [0o750, 0o000] {
            let mut header = Header::new_gnu();
            header.set_size(0);
            header.set_mode(mode);
            header.set_entry_type(EntryType::Directory);
            header.set_cksum();
            builder
                .append_data(&mut header, "root", io::empty())
                .unwrap();
        }
        finish_archive(builder).unwrap();

        let staging = temp.path().join("staging");
        fs::create_dir(&staging).unwrap();
        assert!(extract_archive(&archive, &staging).is_err());
    }

    #[test]
    fn collision_checks_leave_destination_and_config_unchanged() {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("original", temp.path()).unwrap();
        let source = temp.path().join("original");
        let manifest = read_manifest(&source).unwrap();
        let archive = temp.path().join("backup.tar.gz");
        archive_directory(&source, &archive).unwrap();
        let destination = temp.path().join("destination");
        fs::create_dir(&destination).unwrap();
        fs::write(destination.join("keep"), b"operator data").unwrap();
        let mut config = Config::default();
        config.cortexes.insert(
            "registered".into(),
            CortexEntry {
                path: temp.path().join("registered"),
                id: manifest.id,
            },
        );
        let original = config.clone();

        assert!(
            restore_with_save(
                &mut config,
                &archive,
                &RestoreOptions {
                    name: Some("clone".into()),
                    parent: Some(destination.clone()),
                    force: true,
                },
                &temp.path().join("transactions"),
                |_| Ok(()),
            )
            .is_err()
        );
        assert_eq!(config.cortexes.len(), original.cortexes.len());
        let current = config.cortexes.get("registered").unwrap();
        let previous = original.cortexes.get("registered").unwrap();
        assert_eq!(current.path, previous.path);
        assert_eq!(current.id, previous.id);
        assert_eq!(
            fs::read(destination.join("keep")).unwrap(),
            b"operator data"
        );
    }

    #[test]
    fn failed_config_save_restores_force_replaced_destination() {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("original", temp.path()).unwrap();
        let source = temp.path().join("original");
        let archive = temp.path().join("backup.tar.gz");
        archive_directory(&source, &archive).unwrap();
        let parent = temp.path().join("destination");
        let existing = parent.join("original");
        fs::create_dir_all(&existing).unwrap();
        fs::write(existing.join("keep"), b"operator data").unwrap();
        let mut config = Config::default();

        assert!(
            restore_with_save(
                &mut config,
                &archive,
                &RestoreOptions {
                    parent: Some(parent),
                    force: true,
                    ..Default::default()
                },
                &temp.path().join("transactions"),
                |_| bail!("injected config failure"),
            )
            .is_err()
        );
        assert!(config.cortexes.is_empty());
        assert_eq!(fs::read(existing.join("keep")).unwrap(), b"operator data");
        let transactions = temp.path().join("transactions");
        assert!(
            !transactions.exists()
                || fs::read_dir(transactions).unwrap().all(|entry| {
                    entry
                        .unwrap()
                        .path()
                        .extension()
                        .and_then(|value| value.to_str())
                        != Some("json")
                })
        );
    }

    #[test]
    fn rejects_unsafe_manifest_and_override_names() {
        assert!(validate_name("").is_err());
        assert!(validate_name("../outside").is_err());
        assert!(validate_name("nested/name").is_err());
        assert!(validate_name("valid-name").is_ok());
    }

    #[test]
    fn archive_helper_writes_expected_bytes() {
        let temp = tempfile::tempdir().unwrap();
        let archive = temp.path().join("simple.tar.gz");
        let file = File::create(&archive).unwrap();
        let encoder = GzEncoder::new(file, Compression::default());
        let mut builder = Builder::new(encoder);
        append_bytes(&mut builder, "root/value", b"value").unwrap();
        finish_archive(builder).unwrap();
        let staging = temp.path().join("staging");
        fs::create_dir(&staging).unwrap();
        let root = extract_archive(&archive, &staging).unwrap();
        assert_eq!(fs::read(root.join("value")).unwrap(), b"value");
    }

    #[test]
    fn backup_refuses_self_inclusion_and_replaces_output_only_with_force() {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("source", temp.path()).unwrap();
        let source = temp.path().join("source");
        let inside = source.join("inside.tar.gz");
        assert!(backup(&source, &inside, false).is_err());
        assert!(!inside.exists());
        assert!(fs::read_dir(&source).unwrap().all(|entry| {
            !entry
                .unwrap()
                .file_name()
                .to_string_lossy()
                .starts_with(".noema-backup-")
        }));

        let output = temp.path().join("backup.tar.gz");
        fs::write(&output, b"previous backup").unwrap();
        assert!(backup(&source, &output, false).is_err());
        assert_eq!(fs::read(&output).unwrap(), b"previous backup");
        backup(&source, &output, true).unwrap();
        assert_ne!(fs::read(&output).unwrap(), b"previous backup");
        assert!(fs::read_dir(temp.path()).unwrap().all(|entry| {
            !entry
                .unwrap()
                .file_name()
                .to_string_lossy()
                .starts_with(".noema-restore-previous-backup-")
        }));
    }

    #[test]
    fn backup_strips_owner_metadata_and_uses_one_root() {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("source", temp.path()).unwrap();
        let source = temp.path().join("source");
        let output = temp.path().join("backup.tar.gz");
        backup(&source, &output, false).unwrap();

        let decoder = GzDecoder::new(File::open(output).unwrap());
        let mut archive = tar::Archive::new(decoder);
        for entry in archive.entries().unwrap() {
            let entry = entry.unwrap();
            let path = entry.path().unwrap();
            assert_eq!(
                path.components().next(),
                Some(Component::Normal("source".as_ref()))
            );
            assert_eq!(entry.header().uid().unwrap(), 0);
            assert_eq!(entry.header().gid().unwrap(), 0);
            assert_eq!(entry.header().username().unwrap(), Some(""));
            assert_eq!(entry.header().groupname().unwrap(), Some(""));
        }
    }
}
