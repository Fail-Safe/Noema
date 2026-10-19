use assert_cmd::Command;
use predicates::prelude::*;

fn command(config: &str) -> (tempfile::TempDir, Command) {
    let dir = tempfile::tempdir().unwrap();
    std::fs::create_dir(dir.path().join("noema")).unwrap();
    let mut config: serde_yaml::Value = serde_yaml::from_str(config).unwrap();
    let mapping = config.as_mapping_mut().unwrap();
    let section = mapping
        .entry(serde_yaml::Value::String("typesafe_ai".into()))
        .or_insert_with(|| serde_yaml::Value::Mapping(Default::default()));
    section
        .as_mapping_mut()
        .unwrap()
        .entry(serde_yaml::Value::String("model".into()))
        .or_insert_with(|| serde_yaml::Value::String("jev-1.13.0".into()));
    std::fs::write(
        dir.path().join("noema/config.yaml"),
        serde_yaml::to_string(&config).unwrap(),
    )
    .unwrap();
    let mut command = Command::new(assert_cmd::cargo::cargo_bin!("noema"));
    command
        .env("XDG_CONFIG_HOME", dir.path())
        .env_remove("JEV_API_TOKEN")
        .env_remove("SYSTEM_ONE_API_TOKEN")
        .env_remove("TYPESAFE_API_KEY");
    (dir, command)
}

#[test]
fn absent_credentials_do_not_enable_typesafe_and_status_needs_no_cortex() {
    let (_dir, mut c) = command("{}");
    c.args(["typesafe", "status"])
        .assert()
        .success()
        .stdout(predicate::str::contains("\"enabled\": false"))
        .stdout(predicate::str::contains("unrelated-synthetic-key").not());
}

#[test]
fn explicit_disable_overrides_available_token() {
    let (_dir, mut c) = command("typesafe_ai:\n  enabled: false\n");
    c.env("TYPESAFE_API_KEY", "synthetic-private-token")
        .args(["typesafe", "status"])
        .assert()
        .success()
        .stdout(predicate::str::contains("\"enabled\": false"))
        .stdout(predicate::str::contains("\"credential_available\": true"))
        .stdout(predicate::str::contains("synthetic-private-token").not());
}

#[test]
fn explicit_enable_requires_credential() {
    let (_dir, mut c) = command("typesafe_ai:\n  enabled: true\n");
    c.args(["typesafe", "status"])
        .assert()
        .failure()
        .stderr(predicate::str::contains("credential is unavailable"));
}

#[test]
fn custom_token_variable_and_auto_activation() {
    let (_dir, mut c) =
        command("typesafe_ai:\n  enabled: auto\n  token_env: NOEMA_TEST_TYPESAFE_CREDENTIAL\n");
    c.env("NOEMA_TEST_TYPESAFE_CREDENTIAL", "synthetic-token")
        .args(["typesafe", "status"])
        .assert()
        .success()
        .stdout(predicate::str::contains("\"enabled\": true"))
        .stdout(predicate::str::contains("synthetic-token").not());
}

#[test]
fn canonical_token_and_command_enable_typesafe() {
    let (_dir, mut c) = command("typesafe_ai:\n  enabled: auto\n");
    c.env("TYPESAFE_API_KEY", "canonical-synthetic-token")
        .args(["typesafe", "status"])
        .assert()
        .success()
        .stdout(predicate::str::contains("\"enabled\": true"))
        .stdout(predicate::str::contains(
            "\"credential_env\": \"TYPESAFE_API_KEY\"",
        ))
        .stdout(predicate::str::contains("canonical-synthetic-token").not());
}

#[test]
fn blank_canonical_token_does_not_fall_back_to_legacy() {
    let (_dir, mut c) = command("{}");
    c.env("TYPESAFE_API_KEY", " ")
        .env("JEV_API_TOKEN", "legacy-synthetic-token")
        .args(["typesafe", "status"])
        .assert()
        .success()
        .stdout(predicate::str::contains("\"enabled\": false"));
}

#[test]
fn custom_variable_does_not_fall_back_to_default_tokens() {
    let (_dir, mut c) = command("typesafe_ai:\n  token_env: NOEMA_TEST_MISSING_CREDENTIAL\n");
    c.env_remove("NOEMA_TEST_MISSING_CREDENTIAL")
        .env("TYPESAFE_API_KEY", "canonical-synthetic-token")
        .env("JEV_API_TOKEN", "legacy-synthetic-token")
        .args(["typesafe", "status"])
        .assert()
        .success()
        .stdout(predicate::str::contains("\"enabled\": false"));
}

#[test]
fn canonical_token_takes_precedence_when_both_exist() {
    let (_dir, mut c) = command("{}");
    c.env("TYPESAFE_API_KEY", "canonical-synthetic-token")
        .env("JEV_API_TOKEN", "legacy-synthetic-token")
        .args(["typesafe", "status"])
        .assert()
        .success()
        .stdout(predicate::str::contains("\"enabled\": true"))
        .stdout(predicate::str::contains(
            "\"credential_env\": \"TYPESAFE_API_KEY\"",
        ))
        .stdout(predicate::str::contains("synthetic-token").not());
}

#[test]
fn provider_key_activates_for_explicit_provider_model_and_old_tokens_do_not() {
    let (_dir, mut c) = command("{}");
    c.env("TYPESAFE_API_KEY", "synthetic-provider-key")
        .args(["typesafe", "status"])
        .assert()
        .success()
        .stdout(predicate::str::contains("\"enabled\": true"))
        .stdout(predicate::str::contains("synthetic-provider-key").not());
    let (_dir, mut c) = command("{}");
    c.env("JEV_API_TOKEN", "synthetic-legacy-key")
        .env("SYSTEM_ONE_API_TOKEN", "synthetic-legacy-key")
        .args(["typesafe", "status"])
        .assert()
        .success()
        .stdout(predicate::str::contains("\"enabled\": false"));
}

#[test]
fn only_provider_command_is_supported() {
    for old in ["jev", "system-one"] {
        let (_dir, mut c) = command("{}");
        c.args([old, "status"])
            .assert()
            .failure()
            .stderr(predicate::str::contains("unrecognized subcommand"));
    }
    let cfg: noema::config::Config = serde_yaml::from_str("typesafe_ai: {}\n").unwrap();
    assert!(
        serde_yaml::to_string(&cfg)
            .unwrap()
            .contains("typesafe_ai:")
    );
    for old in ["jev", "system_one"] {
        let cfg: noema::config::Config = serde_yaml::from_str(&format!("{old}: {{}}\n")).unwrap();
        assert!(cfg.typesafe_ai.is_none());
    }
}

#[test]
fn local_clef_explicit_enablement_needs_no_cloud_key() {
    let (_dir, mut command) = command("typesafe_ai:\n  model: clef-flash\n  enabled: true\n");
    command
        .env("TYPESAFE_API_KEY", "synthetic-cloud-key-must-not-be-used")
        .args(["typesafe", "status"])
        .assert()
        .success()
        .stdout(predicate::str::contains("\"enabled\": true"))
        .stdout(predicate::str::contains(
            "\"credential_env\": \"local-clef\"",
        ))
        .stdout(predicate::str::contains("synthetic-cloud-key-must-not-be-used").not());
}
#[test]
fn local_clef_auto_does_not_use_cloud_key() {
    let (_dir, mut command) = command("typesafe_ai:\n  model: clef-flash\n  enabled: auto\n");
    command
        .env("TYPESAFE_API_KEY", "synthetic-cloud-key-must-not-be-used")
        .args(["typesafe", "status"])
        .assert()
        .success()
        .stdout(predicate::str::contains("\"enabled\": false"));
}

#[test]
fn explicit_local_provider_activates_without_cloud_key() {
    let (_dir, mut command) = command(
        "typesafe_ai:\n  provider: local\n  enabled: true\n  model: local-eval\n  endpoint: http://127.0.0.1:18788/v1/systemone\n",
    );
    command
        .env("TYPESAFE_API_KEY", "synthetic-cloud-key-must-not-be-used")
        .args(["typesafe", "status"])
        .assert()
        .success()
        .stdout(predicate::str::contains("\"enabled\": true"))
        .stdout(predicate::str::contains("\"provider\": \"local\""))
        .stdout(predicate::str::contains(
            "\"credential_env\": \"local-clef\"",
        ))
        .stdout(predicate::str::contains("synthetic-cloud-key-must-not-be-used").not());
}

#[test]
fn explicit_hosted_provider_ignores_clef_flash_local_shortcut() {
    let (_dir, mut command) =
        command("typesafe_ai:\n  provider: hosted\n  enabled: true\n  model: clef-flash\n");
    command
        .args(["typesafe", "status"])
        .assert()
        .failure()
        .stderr(predicate::str::contains("credential is unavailable"));
}
