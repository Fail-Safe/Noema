use anyhow::{Result, bail};
use serde::{Deserialize, Serialize};

use crate::cortex::Manifest;

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ModelMode {
    #[default]
    Shared,
    Separate,
}

impl ModelMode {
    pub fn label(self) -> &'static str {
        match self {
            Self::Shared => "shared",
            Self::Separate => "separate",
        }
    }
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct TagReviewConfig {
    pub mode: ModelMode,
    /// Saved separate-model settings survive toggling back to shared mode.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub model_name: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub endpoint: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub api_key_env: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResolvedModel {
    pub mode: ModelMode,
    pub model_name: String,
    pub endpoint: String,
    pub api_key_env: String,
}

impl ResolvedModel {
    pub(crate) fn validate(&self) -> Result<()> {
        if self.model_name.trim().is_empty() || self.endpoint.trim().is_empty() {
            bail!("semantic Tag Review needs a model name and endpoint; open settings (s)");
        }
        validate_fields(&self.model_name, &self.endpoint, &self.api_key_env)
    }
}

fn validate_fields(model: &str, endpoint: &str, api_key_env: &str) -> Result<()> {
    if model.len() > 256 || model.chars().any(char::is_control) {
        bail!("Tag Review model name must be at most 256 bytes with no control characters");
    }
    if !api_key_env.is_empty()
        && (api_key_env.len() > 256
            || !api_key_env.bytes().enumerate().all(|(i, c)| {
                c == b'_' || c.is_ascii_alphabetic() || (i > 0 && c.is_ascii_digit())
            }))
    {
        bail!("Tag Review credential must be an environment variable name, not a key value");
    }
    if endpoint.is_empty() {
        return Ok(());
    }
    if endpoint.len() > 2048 || endpoint.chars().any(char::is_whitespace) {
        bail!("invalid Tag Review model endpoint");
    }
    let url = reqwest::Url::parse(endpoint)
        .map_err(|_| anyhow::anyhow!("invalid Tag Review model endpoint"))?;
    if !matches!(url.scheme(), "http" | "https")
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        bail!("model endpoint must be HTTP(S), without credentials, query, or fragment");
    }
    if !api_key_env.is_empty() && url.scheme() != "https" {
        bail!("Tag Review requires HTTPS when a model API key is configured");
    }
    Ok(())
}

impl TagReviewConfig {
    pub fn validate(&self) -> Result<()> {
        validate_fields(&self.model_name, &self.endpoint, &self.api_key_env)?;
        if self.mode == ModelMode::Separate {
            self.separate_model().validate()?;
        }
        Ok(())
    }

    fn separate_model(&self) -> ResolvedModel {
        ResolvedModel {
            mode: ModelMode::Separate,
            model_name: self.model_name.clone(),
            endpoint: self.endpoint.clone(),
            api_key_env: self.api_key_env.clone(),
        }
    }
}

/// Shared mode inherits the entire consolidation destination. Separate mode
/// never silently falls back to a different model, endpoint, or credential.
pub fn resolve_model(manifest: &Manifest) -> Result<ResolvedModel> {
    let settings = manifest.tag_review.clone().unwrap_or_default();
    let resolved = match settings.mode {
        ModelMode::Separate => settings.separate_model(),
        ModelMode::Shared => {
            let config = manifest.consolidation_config()?.unwrap_or_default();
            if !config.llm_enabled {
                bail!(
                    "shared Tag Review model needs consolidation.llm_enabled; configure consolidation or choose separate mode (s)"
                );
            }
            ResolvedModel {
                mode: ModelMode::Shared,
                model_name: config.model_name,
                endpoint: config.local_llm_endpoint,
                api_key_env: config.api_key_env,
            }
        }
    };
    resolved.validate()?;
    Ok(resolved)
}

/// Safe status: model and mode only, never credentials or full endpoint URLs.
pub fn model_status(manifest: &Manifest) -> String {
    match resolve_model(manifest) {
        Ok(model) => format!("{} · {}", model.mode.label(), model.model_name),
        Err(_) => format!(
            "{} · not configured (s: settings)",
            manifest
                .tag_review
                .as_ref()
                .map(|s| s.mode)
                .unwrap_or_default()
                .label()
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn manifest() -> Manifest {
        Manifest {
            consolidation: Some(serde_yaml::from_str("llm_enabled: true\nlocal_llm_endpoint: https://consolidation.example.com/v1\nmodel_name: fixture-9b\napi_key_env: SHARED_API_KEY\n").unwrap()),
            ..Default::default()
        }
    }

    fn separate() -> TagReviewConfig {
        TagReviewConfig {
            mode: ModelMode::Separate,
            model_name: "fixture-35b".into(),
            endpoint: "http://127.0.0.1:8001/v1".into(),
            api_key_env: String::new(),
        }
    }

    #[test]
    fn missing_settings_default_to_the_whole_shared_destination() {
        let resolved = resolve_model(&manifest()).unwrap();
        assert_eq!(resolved.mode, ModelMode::Shared);
        assert_eq!(resolved.model_name, "fixture-9b");
        assert_eq!(resolved.endpoint, "https://consolidation.example.com/v1");
        assert_eq!(resolved.api_key_env, "SHARED_API_KEY");
    }

    #[test]
    fn separate_model_never_inherits_shared_credentials_or_enablement() {
        let mut manifest = manifest();
        manifest.tag_review = Some(separate());
        let resolved = resolve_model(&manifest).unwrap();
        assert_eq!(resolved.model_name, "fixture-35b");
        assert_eq!(resolved.endpoint, "http://127.0.0.1:8001/v1");
        assert!(resolved.api_key_env.is_empty());
        manifest.consolidation = None;
        assert_eq!(resolve_model(&manifest).unwrap(), resolved);
        manifest.tag_review.as_mut().unwrap().mode = ModelMode::Shared;
        assert!(resolve_model(&manifest).is_err());
    }

    #[test]
    fn shared_toggle_ignores_but_retains_separate_settings() {
        let mut manifest = manifest();
        let mut settings = separate();
        settings.mode = ModelMode::Shared;
        manifest.tag_review = Some(settings);
        assert_eq!(resolve_model(&manifest).unwrap().model_name, "fixture-9b");
        manifest.tag_review.as_mut().unwrap().mode = ModelMode::Separate;
        assert_eq!(resolve_model(&manifest).unwrap().model_name, "fixture-35b");
    }

    #[test]
    fn incomplete_separate_mode_fails_instead_of_falling_back() {
        for field in ["model", "endpoint"] {
            let mut manifest = manifest();
            let mut settings = separate();
            if field == "model" {
                settings.model_name.clear();
            } else {
                settings.endpoint.clear();
            }
            manifest.tag_review = Some(settings);
            assert!(resolve_model(&manifest).is_err());
        }
    }

    #[test]
    fn invalid_destinations_and_literal_credentials_are_rejected_without_echoing() {
        for endpoint in [
            "https://user:synthetic-password@example.com/v1",
            "https://example.com/v1?key=synthetic-secret",
            "file:///fixture",
            "http://example.com/v1\n",
        ] {
            let mut settings = separate();
            settings.endpoint = endpoint.into();
            let error = settings.validate().unwrap_err().to_string();
            assert!(!error.contains("synthetic-"));
        }
        let mut settings = separate();
        settings.api_key_env = "synthetic-key-not-a-variable".into();
        assert!(
            !settings
                .validate()
                .unwrap_err()
                .to_string()
                .contains("synthetic-key")
        );
        settings.api_key_env = "REVIEW_API_KEY".into();
        assert!(settings.validate().is_err(), "credentialed HTTP must fail");
        settings.endpoint = "https://review.example.com/v1".into();
        settings.validate().unwrap();
    }
}
