use std::{
    collections::BTreeSet,
    ffi::OsString,
    fs::{self, File, OpenOptions},
    io,
    path::{Component, Path, PathBuf},
    time::UNIX_EPOCH,
};

use anyhow::{Context, Result, bail};
use flate2::{Compression, read::GzDecoder, write::GzEncoder};

use crate::{
    config::{Config, CortexEntry},
    cortex::{read_manifest, write_manifest},
};

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
    } else {
        bail!("refusing to back up non-regular entry {}", source.display())
    }
    Ok(())
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

pub fn restore(
    config: &mut Config,
    tarball: &Path,
    options: &RestoreOptions,
) -> Result<RestoreResult> {
    restore_with_save(config, tarball, options, Config::save)
}

fn restore_with_save<F>(
    config: &mut Config,
    tarball: &Path,
    options: &RestoreOptions,
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

    fs::create_dir_all(&final_parent)
        .with_context(|| format!("creating restore parent {}", final_parent.display()))?;
    let incoming = unique_sibling(&final_path, "incoming");
    move_directory(&staged_cortex, &incoming).context("staging restored cortex for placement")?;

    let backup = existing_destination.then(|| unique_sibling(&final_path, "backup"));
    if let Some(backup) = &backup
        && let Err(error) = fs::rename(&final_path, backup)
    {
        let _ = fs::remove_dir_all(&incoming);
        return Err(error).context("preserving existing restore destination");
    }
    pause_restore_for_test("destination-preserved");
    if let Err(error) = fs::rename(&incoming, &final_path) {
        if let Some(backup) = &backup {
            let _ = fs::rename(backup, &final_path);
        }
        let _ = fs::remove_dir_all(&incoming);
        return Err(error).context("placing restored cortex");
    }
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
        let failed_restore = unique_sibling(&final_path, "failed");
        let displaced = fs::rename(&final_path, &failed_restore);
        let restored = backup
            .as_ref()
            .map(|backup| fs::rename(backup, &final_path))
            .transpose();
        let _ = fs::remove_dir_all(&failed_restore);
        if let Err(rollback_error) = displaced.and(restored.map(|_| ())) {
            return Err(error.context(format!(
                "saving configuration failed and restoring the previous destination also failed: {rollback_error}"
            )));
        }
        return Err(error).context("saving restored cortex configuration");
    }
    pause_restore_for_test("config-saved");

    let retained_backup = backup.and_then(|backup| match remove_path(&backup) {
        Ok(()) => None,
        Err(_) => Some(backup),
    });
    Ok(RestoreResult {
        name: final_name.clone(),
        path: final_path,
        id: manifest.id,
        is_default: config.default == final_name,
        retained_backup,
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
            |_| Ok(()),
        )
        .unwrap();

        assert_eq!(result.id, original_id);
        assert_eq!(result.name, "renamed");
        assert!(result.is_default);
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
                |_| bail!("injected config failure"),
            )
            .is_err()
        );
        assert!(config.cortexes.is_empty());
        assert_eq!(fs::read(existing.join("keep")).unwrap(), b"operator data");
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
