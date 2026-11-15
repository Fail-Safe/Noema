use super::*;
use crate::tag_review::TagReviewConfig;

impl Cortex {
    /// Read fresh settings, so a CLI change is visible to an already-open TUI.
    pub fn tag_review_settings(&self) -> Result<TagReviewConfig> {
        Ok(read_manifest(&self.dir)?.tag_review.unwrap_or_default())
    }

    /// Change only tag_review in the YAML mapping, preserving other (including
    /// unknown) settings and the exact Markdown suffix. Never resolve API keys.
    pub fn save_tag_review_settings(
        &self,
        settings: &TagReviewConfig,
        expected: Option<&TagReviewConfig>,
    ) -> Result<()> {
        settings.validate()?;
        let lock = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(self.db_dir.join("tag-review-settings.lock"))?;
        lock.try_lock_exclusive()
            .context("Tag Review settings are being changed by another client")?;
        let path = self.dir.join("cortex.md");
        let metadata = fs::symlink_metadata(&path)?;
        if !metadata.file_type().is_file() {
            bail!("Tag Review settings require cortex.md to be a regular file");
        }
        let bytes = fs::read(&path)?;
        let text = std::str::from_utf8(&bytes).context("cortex.md is not UTF-8")?;
        let (yaml, suffix) = if let Some(rest) = text.strip_prefix("---\n") {
            let end = rest
                .find("\n---\n")
                .context("unterminated cortex manifest frontmatter")?;
            (&rest[..end], &rest[end + 5..])
        } else {
            (text, "")
        };
        let mut document: serde_yaml::Value = serde_yaml::from_str(yaml)
            .map_err(|_| anyhow::anyhow!("invalid cortex manifest YAML"))?;
        let mapping = document
            .as_mapping_mut()
            .context("cortex manifest must be a YAML mapping")?;
        let key = serde_yaml::Value::String("tag_review".into());
        if let Some(expected) = expected {
            let current: TagReviewConfig = mapping
                .get(&key)
                .cloned()
                .map(serde_yaml::from_value)
                .transpose()
                .map_err(|_| anyhow::anyhow!("invalid Tag Review settings"))?
                .unwrap_or_default();
            if &current != expected {
                bail!("Tag Review settings changed in another client; reopen settings");
            }
        }
        mapping.insert(key, serde_yaml::to_value(settings)?);
        let yaml = serde_yaml::to_string(&document)?;
        let yaml = yaml.strip_prefix("---\n").unwrap_or(&yaml);
        let replacement = format!("---\n{yaml}---\n{suffix}");
        if fs::read(&path)? != bytes {
            bail!("cortex manifest changed while saving Tag Review settings; retry");
        }
        #[cfg(unix)]
        let mode = {
            use std::os::unix::fs::PermissionsExt;
            metadata.permissions().mode() & 0o777
        };
        #[cfg(not(unix))]
        let mode = 0o640;
        trace::write_bytes_atomic_with_mode(&path, replacement.as_bytes(), mode)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::tag_review::{ModelMode, resolve_model};

    #[test]
    fn settings_round_trip_preserves_unrelated_yaml_body_and_private_permissions() {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("test", temp.path()).unwrap();
        let cx = Cortex::open("test", temp.path().join("test")).unwrap();
        let path = cx.dir.join("cortex.md");
        let source = "---\nname: test\ncreated: '2026-01-01'\nversion: 2\nconsolidation:\n  llm_enabled: true\n  local_llm_endpoint: http://127.0.0.1:8000/v1\n  model_name: fixture-9b\n  future_field: preserve-me\nfuture_section:\n  nested: [a, b]\n---\n\n# Untouched body\n\nKeep exact whitespace.  \n\n";
        fs::write(&path, source).unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).unwrap();
        }
        let original = cx.tag_review_settings().unwrap();
        assert_eq!(original.mode, ModelMode::Shared);
        let mut settings = TagReviewConfig {
            mode: ModelMode::Separate,
            model_name: "fixture-35b".into(),
            endpoint: "http://127.0.0.1:8001/v1".into(),
            api_key_env: String::new(),
        };
        cx.save_tag_review_settings(&settings, Some(&original))
            .unwrap();
        let saved = fs::read_to_string(&path).unwrap();
        let before: serde_yaml::Value = serde_yaml::from_str(
            source
                .strip_prefix("---\n")
                .unwrap()
                .split("\n---\n")
                .next()
                .unwrap(),
        )
        .unwrap();
        let after: serde_yaml::Value = serde_yaml::from_str(
            saved
                .strip_prefix("---\n")
                .unwrap()
                .split("\n---\n")
                .next()
                .unwrap(),
        )
        .unwrap();
        assert_eq!(before["consolidation"], after["consolidation"]);
        assert_eq!(before["future_section"], after["future_section"]);
        assert!(saved.ends_with("\n\n# Untouched body\n\nKeep exact whitespace.  \n\n"));
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            assert_eq!(
                fs::metadata(&path).unwrap().permissions().mode() & 0o777,
                0o600
            );
        }
        let reopened = Cortex::open("test", &cx.dir).unwrap();
        assert_eq!(
            resolve_model(&reopened.manifest).unwrap().model_name,
            "fixture-35b"
        );
        settings.mode = ModelMode::Shared;
        cx.save_tag_review_settings(&settings, None).unwrap();
        assert_eq!(
            resolve_model(&read_manifest(&cx.dir).unwrap())
                .unwrap()
                .model_name,
            "fixture-9b"
        );
        assert_eq!(cx.tag_review_settings().unwrap().model_name, "fixture-35b");
    }

    #[test]
    fn invalid_or_stale_settings_leave_the_manifest_unchanged() {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("test", temp.path()).unwrap();
        let cx = Cortex::open("test", temp.path().join("test")).unwrap();
        let path = cx.dir.join("cortex.md");
        let original = fs::read(&path).unwrap();
        let incomplete = TagReviewConfig {
            mode: crate::tag_review::ModelMode::Separate,
            ..Default::default()
        };
        assert!(cx.save_tag_review_settings(&incomplete, None).is_err());
        assert_eq!(fs::read(&path).unwrap(), original);
        let settings = TagReviewConfig {
            model_name: "saved-draft".into(),
            ..Default::default()
        };
        cx.save_tag_review_settings(&settings, Some(&TagReviewConfig::default()))
            .unwrap();
        let saved = fs::read(&path).unwrap();
        assert!(
            cx.save_tag_review_settings(
                &TagReviewConfig::default(),
                Some(&TagReviewConfig::default())
            )
            .is_err()
        );
        assert_eq!(fs::read(&path).unwrap(), saved);
    }
}
