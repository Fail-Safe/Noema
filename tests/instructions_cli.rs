use assert_cmd::Command;
use std::fs;

#[test]
fn instructions_do_not_require_or_modify_local_configuration() {
    let home = tempfile::tempdir().unwrap();
    let config_dir = home.path().join("config");
    let config_path = config_dir.join("noema/config.yaml");
    let invalid_config = "decision: [unfinished";

    for configured in [false, true] {
        if configured {
            fs::create_dir_all(config_path.parent().unwrap()).unwrap();
            fs::write(&config_path, invalid_config).unwrap();
        }
        for topic in [None, Some("quick-start"), Some("federation")] {
            let mut command = Command::new(assert_cmd::cargo::cargo_bin!("noema"));
            command
                .current_dir(home.path())
                .env("HOME", home.path())
                .env("XDG_CONFIG_HOME", &config_dir)
                .env("NOEMA_CORTEX", "not-registered")
                .arg("instructions");
            if let Some(topic) = topic {
                command.arg(topic);
            }
            command.assert().success();
            assert!(!home.path().join(".noema").exists());
            if configured {
                assert_eq!(fs::read_to_string(&config_path).unwrap(), invalid_config);
                assert_eq!(
                    fs::read_dir(config_path.parent().unwrap()).unwrap().count(),
                    1
                );
            } else {
                assert!(!config_dir.exists());
                assert_eq!(fs::read_dir(home.path()).unwrap().count(), 0);
            }
        }
    }
}
