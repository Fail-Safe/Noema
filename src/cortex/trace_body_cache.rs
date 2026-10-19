use std::{
    collections::VecDeque,
    fs,
    path::{Path, PathBuf},
    sync::Arc,
    time::SystemTime,
};

use anyhow::{Context, Result};

use crate::trace::{self, Trace};

const MAX_ENTRIES: usize = 128;
const MAX_BODY_BYTES: usize = 2 * 1024 * 1024;

#[derive(PartialEq, Eq, serde::Serialize)]
struct FileVersion {
    len: u64,
    modified: SystemTime,
    #[cfg(unix)]
    identity: (u64, u64, i64, i64),
}

pub(super) fn file_version(path: &Path) -> Result<serde_json::Value> {
    Ok(serde_json::to_value(FileVersion::read(path)?)?)
}

impl FileVersion {
    fn read(path: &Path) -> Result<Self> {
        let metadata = fs::metadata(path)
            .with_context(|| format!("reading trace metadata for {}", path.display()))?;
        Ok(Self {
            len: metadata.len(),
            modified: metadata.modified()?,
            #[cfg(unix)]
            identity: {
                use std::os::unix::fs::MetadataExt;
                (
                    metadata.dev(),
                    metadata.ino(),
                    metadata.ctime(),
                    metadata.ctime_nsec(),
                )
            },
        })
    }
}

struct Entry {
    path: PathBuf,
    hash: String,
    version: FileVersion,
    body: Arc<str>,
}

#[derive(Default)]
pub(super) struct TraceBodyCache {
    entries: VecDeque<Entry>,
    bytes: usize,
    #[cfg(test)]
    disabled: bool,
    #[cfg(test)]
    hits: usize,
    #[cfg(test)]
    reads: usize,
}

impl TraceBodyCache {
    pub(super) fn read(&mut self, path: &Path, hash: &str) -> Result<Arc<str>> {
        #[cfg(test)]
        if self.disabled {
            self.reads += 1;
            return Ok(Arc::from(Trace::parse_file(path)?.body));
        }

        let version = FileVersion::read(path)?;
        if let Some(index) = self.entries.iter().position(|entry| entry.path == path) {
            let entry = self.entries.remove(index).unwrap();
            if cfg!(unix) && entry.hash == hash && entry.version == version {
                let body = Arc::clone(&entry.body);
                self.entries.push_back(entry);
                #[cfg(test)]
                {
                    self.hits += 1;
                }
                return Ok(body);
            }
            self.bytes -= entry.body.len();
        }

        #[cfg(test)]
        {
            self.reads += 1;
        }
        let body: Arc<str> = Arc::from(Trace::parse_file(path)?.body);
        if cfg!(unix)
            && body.len() <= MAX_BODY_BYTES
            && trace::content_hash(&body) == hash
            && FileVersion::read(path)? == version
        {
            while self.entries.len() >= MAX_ENTRIES || self.bytes + body.len() > MAX_BODY_BYTES {
                self.bytes -= self.entries.pop_front().unwrap().body.len();
            }
            self.bytes += body.len();
            self.entries.push_back(Entry {
                path: path.to_owned(),
                hash: hash.to_owned(),
                version,
                body: Arc::clone(&body),
            });
        }
        Ok(body)
    }

    #[cfg(test)]
    pub(super) fn set_enabled(&mut self, enabled: bool) {
        self.entries.clear();
        self.bytes = 0;
        self.disabled = !enabled;
        self.hits = 0;
        self.reads = 0;
    }

    #[cfg(test)]
    pub(super) fn counts(&self) -> (usize, usize) {
        (self.hits, self.reads)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn write(path: &Path, body: &str) {
        let trace = Trace::new("Cached evidence", "fact", "test", Vec::new(), body);
        trace.write_preserving_updated(path).unwrap();
    }

    #[test]
    fn cache_detects_external_edits_with_preserved_length_and_mtime() {
        let temp = tempfile::tempdir().unwrap();
        let path = temp.path().join("trace.md");
        write(&path, "original");
        let mut cache = TraceBodyCache::default();
        let hash = trace::content_hash("original");
        assert_eq!(&*cache.read(&path, &hash).unwrap(), "original");
        assert_eq!(&*cache.read(&path, &hash).unwrap(), "original");
        let modified = fs::metadata(&path).unwrap().modified().unwrap();
        write(&path, "modified");
        fs::File::options()
            .write(true)
            .open(&path)
            .unwrap()
            .set_modified(modified)
            .unwrap();
        assert_eq!(&*cache.read(&path, &hash).unwrap(), "modified");
        assert_eq!(&*cache.read(&path, &hash).unwrap(), "modified");
        assert!(cache.entries.is_empty());
        let hash = trace::content_hash("modified");
        cache.read(&path, &hash).unwrap();
        let modified = fs::metadata(&path).unwrap().modified().unwrap();
        let bytes = fs::read_to_string(&path)
            .unwrap()
            .replace("modified", "original");
        fs::write(&path, bytes).unwrap();
        fs::File::options()
            .write(true)
            .open(&path)
            .unwrap()
            .set_modified(modified)
            .unwrap();
        assert_eq!(&*cache.read(&path, &hash).unwrap(), "original");
        assert!(cache.entries.is_empty());
        fs::write(&path, "malformed frontmatter").unwrap();
        assert!(cache.read(&path, &hash).is_err());
        fs::remove_file(&path).unwrap();
        assert!(cache.read(&path, &hash).is_err());
    }

    #[test]
    fn cache_bounds_entries_and_bytes_and_isolates_paths() {
        let temp = tempfile::tempdir().unwrap();
        let mut cache = TraceBodyCache::default();
        for index in 0..150 {
            let path = temp.path().join(format!("small-{index}.md"));
            write(&path, "small");
            cache.read(&path, &trace::content_hash("small")).unwrap();
        }
        #[cfg(unix)]
        assert_eq!(cache.entries.len(), MAX_ENTRIES);
        let body = "x".repeat(24 * 1024);
        let hash = trace::content_hash(&body);
        for index in 0..150 {
            let path = temp.path().join(format!("trace-{index}.md"));
            write(&path, &body);
            cache.read(&path, &hash).unwrap();
        }
        assert!(cache.entries.len() <= MAX_ENTRIES);
        assert!(cache.bytes <= MAX_BODY_BYTES);
        let path = temp.path().join("oversized.md");
        let body = "x".repeat(MAX_BODY_BYTES + 1);
        write(&path, &body);
        cache.read(&path, &trace::content_hash(&body)).unwrap();
        assert!(!cache.entries.iter().any(|entry| entry.path == path));
    }
}
