use std::{
    collections::BTreeMap,
    fs::{self, OpenOptions},
    io::Write,
    path::Path,
    sync::LazyLock,
};

use anyhow::{Context, Result, bail};
use chrono::{SecondsFormat, Utc};
use regex::Regex;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

pub const MAX_SLUG_LEN: usize = 100;
pub const VALID_TYPES: &[&str] = &[
    "fact",
    "decision",
    "preference",
    "context",
    "skill",
    "intent",
    "observation",
    "note",
    "divergence",
];
pub const VALID_TIERS: &[&str] = &["short", "mid", "long"];

static VALID_ID: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^[0-9]{8}-[a-z0-9][a-z0-9-]{0,99}$").unwrap());

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct Frontmatter {
    pub id: String,
    pub title: String,
    #[serde(rename = "type")]
    pub trace_type: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub tier: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub author: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tags: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub derived_from: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub origin: String,
    pub created: String,
    pub updated: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub content_hash: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub source_hash: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub source_locked: bool,
    #[serde(default, flatten)]
    pub extra: BTreeMap<String, serde_yaml::Value>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Trace {
    pub frontmatter: Frontmatter,
    pub body: String,
}

impl Trace {
    pub fn new(
        title: impl Into<String>,
        trace_type: impl Into<String>,
        author: impl Into<String>,
        tags: Vec<String>,
        body: impl Into<String>,
    ) -> Self {
        let title = title.into();
        let now = now_rfc3339();
        Self {
            frontmatter: Frontmatter {
                id: new_id(&title),
                title,
                trace_type: trace_type.into(),
                tier: String::new(),
                author: author.into(),
                tags,
                derived_from: Vec::new(),
                origin: String::new(),
                created: now.clone(),
                updated: now,
                content_hash: String::new(),
                source_hash: String::new(),
                source_locked: false,
                extra: BTreeMap::new(),
            },
            body: body.into(),
        }
    }

    pub fn parse(data: &[u8]) -> Result<Self> {
        let source = std::str::from_utf8(data).context("trace is not UTF-8")?;
        let rest = source
            .strip_prefix("---\n")
            .ok_or_else(|| anyhow::anyhow!("missing frontmatter delimiter"))?;
        let end = rest
            .find("\n---\n")
            .ok_or_else(|| anyhow::anyhow!("unterminated frontmatter"))?;
        let frontmatter = serde_yaml::from_str(&rest[..end]).context("parsing frontmatter")?;
        let body = rest[end + 5..]
            .strip_prefix('\n')
            .unwrap_or(&rest[end + 5..]);
        Ok(Self {
            frontmatter,
            body: body.to_owned(),
        })
    }

    pub fn parse_file(path: &Path) -> Result<Self> {
        Self::parse(&fs::read(path).with_context(|| format!("reading {}", path.display()))?)
    }

    pub fn validate(&self) -> Result<()> {
        let f = &self.frontmatter;
        if !is_valid_id(&f.id) {
            bail!(
                "invalid trace frontmatter: id {:?} does not match YYYYMMDD-slug format",
                f.id
            );
        }
        if f.title.is_empty() {
            bail!("invalid trace frontmatter: title is required");
        }
        if !VALID_TYPES.contains(&f.trace_type.as_str()) {
            bail!(
                "invalid trace frontmatter: unrecognized type {:?}; expected one of: {}",
                f.trace_type,
                VALID_TYPES.join(", ")
            );
        }
        if f.created.is_empty() || f.updated.is_empty() {
            bail!("invalid trace frontmatter: created and updated timestamps are required");
        }
        if !f.tier.is_empty() && !VALID_TIERS.contains(&f.tier.as_str()) {
            bail!("invalid trace frontmatter: unrecognized tier {:?}", f.tier);
        }
        Ok(())
    }

    pub fn encoded(&self) -> Result<Vec<u8>> {
        let yaml = serde_yaml::to_string(&self.frontmatter).context("encoding frontmatter")?;
        let yaml = yaml.strip_prefix("---\n").unwrap_or(&yaml);
        Ok(format!("---\n{yaml}---\n\n{}", self.body).into_bytes())
    }

    pub fn write(&mut self, path: &Path) -> Result<()> {
        self.frontmatter.updated = now_rfc3339();
        self.write_preserving_updated(path)
    }

    pub fn write_preserving_updated(&self, path: &Path) -> Result<()> {
        write_bytes_atomic(path, &self.encoded()?)
            .with_context(|| format!("writing {}", path.display()))
    }

    pub(crate) fn write_preserving_updated_compatible(&self, path: &Path) -> Result<()> {
        write_bytes_compatible(path, &self.encoded()?)
            .with_context(|| format!("writing {}", path.display()))
    }

    pub fn effective_tier(&self) -> &str {
        if self.frontmatter.tier.is_empty() {
            "short"
        } else {
            &self.frontmatter.tier
        }
    }
}

pub(crate) fn write_bytes_atomic(path: &Path, data: &[u8]) -> Result<()> {
    write_bytes_atomic_with_mode(path, data, 0o640)
}

pub(crate) fn write_bytes_atomic_with_mode(path: &Path, data: &[u8], mode: u32) -> Result<()> {
    let directory = path
        .parent()
        .ok_or_else(|| anyhow::anyhow!("trace path has no parent directory"))?;
    if path.exists() {
        drop(OpenOptions::new().write(true).open(path)?);
    }
    let temporary = directory.join(format!(".noema-trace-{}.tmp", ulid::Ulid::new()));
    let result = (|| -> Result<()> {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temporary)?;
        set_permissions(&temporary, mode)?;
        file.write_all(data)?;
        file.sync_all()?;
        drop(file);
        fs::rename(&temporary, path)?;
        sync_directory(directory)?;
        Ok(())
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temporary);
    }
    result
}

fn write_bytes_compatible(path: &Path, data: &[u8]) -> Result<()> {
    let existed = path.exists();
    let mut file = OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .open(path)?;
    if !existed {
        set_permissions(path, 0o640)?;
    }
    file.write_all(data)?;
    Ok(())
}

#[cfg(unix)]
pub(crate) fn sync_directory(path: &Path) -> Result<()> {
    fs::File::open(path)?.sync_all()?;
    Ok(())
}

#[cfg(not(unix))]
pub(crate) fn sync_directory(_path: &Path) -> Result<()> {
    Ok(())
}

pub fn now_rfc3339() -> String {
    Utc::now().to_rfc3339_opts(SecondsFormat::Secs, true)
}

pub fn content_hash(body: &str) -> String {
    format!("sha256:{:x}", Sha256::digest(body.as_bytes()))
}

pub fn is_valid_id(id: &str) -> bool {
    VALID_ID.is_match(id)
}

pub fn new_id(title: &str) -> String {
    let date = Utc::now().format("%Y%m%d");
    let mut value = strip_leading_date_prefix(&slug(title)).to_owned();
    if value.chars().count() > MAX_SLUG_LEN {
        value = value.chars().take(MAX_SLUG_LEN).collect::<String>();
        while value.ends_with('-') {
            value.pop();
        }
    }
    format!("{date}-{value}")
}

fn slug(input: &str) -> String {
    let mut out = String::new();
    let mut previous_hyphen = true;
    for ch in input.to_lowercase().chars() {
        if ch.is_alphanumeric() {
            out.push(ch);
            previous_hyphen = false;
        } else if !previous_hyphen {
            out.push('-');
            previous_hyphen = true;
        }
    }
    out.trim_end_matches('-').to_owned()
}

fn strip_leading_date_prefix(value: &str) -> &str {
    let bytes = value.as_bytes();
    if bytes.len() >= 9 && bytes[8] == b'-' && bytes[..8].iter().all(u8::is_ascii_digit) {
        return &value[9..];
    }
    if bytes.len() >= 11
        && bytes[4] == b'-'
        && bytes[7] == b'-'
        && bytes[10] == b'-'
        && bytes[..4].iter().all(u8::is_ascii_digit)
        && bytes[5..7].iter().all(u8::is_ascii_digit)
        && bytes[8..10].iter().all(u8::is_ascii_digit)
    {
        return &value[11..];
    }
    value
}

#[cfg(unix)]
fn set_permissions(path: &Path, mode: u32) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(mode))?;
    Ok(())
}

#[cfg(not(unix))]
fn set_permissions(_path: &Path, _mode: u32) -> Result<()> {
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn id_matches_public_rules() {
        let id = new_id("2026-04-02 Why We Chose Local Storage!");
        assert!(id.ends_with("-why-we-chose-local-storage"));
        assert!(is_valid_id(&id));
    }

    #[test]
    fn parse_and_encode_round_trip() {
        let mut trace = Trace::new("Choice", "decision", "tester", vec!["rust".into()], "body");
        trace.frontmatter.content_hash = content_hash(&trace.body);
        let parsed = Trace::parse(&trace.encoded().unwrap()).unwrap();
        assert_eq!(parsed, trace);
    }

    #[test]
    fn atomic_write_replaces_existing_file_without_leaving_temporary_files() {
        let temp = tempfile::tempdir().unwrap();
        let path = temp.path().join("trace.md");
        fs::write(&path, b"old bytes").unwrap();
        let mut trace = Trace::new("Atomic", "fact", "", vec![], "new body");
        trace.write(&path).unwrap();

        assert_eq!(Trace::parse_file(&path).unwrap(), trace);
        assert!(fs::read_dir(temp.path()).unwrap().all(|entry| {
            !entry
                .unwrap()
                .file_name()
                .to_string_lossy()
                .starts_with(".noema-trace-")
        }));
    }

    #[test]
    fn failed_atomic_replace_cleans_up_temporary_file() {
        let temp = tempfile::tempdir().unwrap();
        let path = temp.path().join("destination");
        fs::create_dir(&path).unwrap();
        fs::write(path.join("keep"), b"keep").unwrap();
        let mut trace = Trace::new("Atomic failure", "fact", "", vec![], "body");

        assert!(trace.write(&path).is_err());
        assert_eq!(fs::read(path.join("keep")).unwrap(), b"keep");
        assert!(fs::read_dir(temp.path()).unwrap().all(|entry| {
            !entry
                .unwrap()
                .file_name()
                .to_string_lossy()
                .starts_with(".noema-trace-")
        }));
    }
}
