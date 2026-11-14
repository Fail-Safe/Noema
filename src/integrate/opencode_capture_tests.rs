use super::*;

fn capture_request(root: &Path) -> Request {
    Request {
        target: Target {
            client: Client::OpenCode,
            scope: Scope::User,
            project_root: root.join("project"),
            home: root.join("home"),
            xdg_config_home: Some(root.join("config")),
            ohmypi_agent_dir: None,
        },
        connection: ConnectionSpec {
            cortex: "capture-fixture".into(),
            binary: PathBuf::from("/fixture/noema"),
            transport: Transport::Stdio,
            url: None,
            bearer_token_env: None,
        },
        prefetch: PrefetchInstall::Disabled,
    }
}

#[test]
fn opencode_capture_v2_override_replaces_servers_without_credentials() {
    let temp = tempfile::tempdir().unwrap();
    let request = capture_request(temp.path());
    let config = temp.path().join("selected.jsonc");
    for server in [
        json!({
            "type": "remote",
            "url": "https://private.example.com/mcp",
            "headers": {"Authorization": "Bearer fixture-private-token"},
            "oauth": false
        }),
        json!({
            "type": "local",
            "command": ["/private/noema", "--cortex", "private-cortex", "serve"],
            "environment": {"PRIVATE_TOKEN": "fixture-private-token"},
            "cwd": "/private/workspace"
        }),
    ] {
        let source = serde_json::to_string(&json!({
            "mcp": {"servers": {"noema": server, "other": {"type": "remote", "url": "https://other.example.com"}}}
        }))
        .unwrap();
        fs::write(&config, &source).unwrap();
        let output = opencode_capture_override(&request, &config, OpenCodeVersion::V2).unwrap();
        let value: Value = serde_json::from_str(&output).unwrap();
        let servers = &value["mcp"]["servers"];
        let disabled = &servers["noema"];
        assert_eq!(disabled["disabled"], true);
        assert!(disabled.get("enabled").is_none());
        assert_eq!(disabled.as_object().unwrap().len(), 3);
        if server["type"] == "remote" {
            assert_eq!(disabled["url"], "http://127.0.0.1/mcp");
        } else {
            assert_eq!(disabled["command"], json!(["noema", "serve"]));
        }
        assert_eq!(
            servers["noema_capture"],
            json!({
                "type": "local",
                "command": ["/fixture/noema", "--cortex", "capture-fixture", "serve", "--transport", "stdio"],
                "disabled": false,
                "environment": {"NOEMA_MCP_TOOL_PROFILE": "continuity-capture"}
            })
        );
        assert_eq!(servers.as_object().unwrap().len(), 2);
        assert!(value["mcp"].get("noema").is_none());
        assert!(!output.contains("private"));
        assert!(!output.contains("Authorization"));
        assert!(!output.contains("other.example.com"));
        assert_eq!(fs::read_to_string(&config).unwrap(), source);
    }
}

#[test]
fn opencode_capture_v1_override_retains_legacy_schema() {
    let temp = tempfile::tempdir().unwrap();
    let request = capture_request(temp.path());
    let config = temp.path().join("opencode.jsonc");
    let source = r#"{"mcp":{"noema":{"type":"remote","url":"https://private.example.com/mcp","enabled":true}}}"#;
    fs::write(&config, source).unwrap();
    let output = opencode_capture_override(&request, &config, OpenCodeVersion::V1).unwrap();
    assert_eq!(
        serde_json::from_str::<Value>(&output).unwrap(),
        json!({"mcp": {
            "noema": {"type": "remote", "url": "http://127.0.0.1/mcp", "enabled": false},
            "noema_capture": {
                "type": "local",
                "command": ["/fixture/noema", "--cortex", "capture-fixture", "serve", "--transport", "stdio"],
                "enabled": true,
                "environment": {"NOEMA_MCP_TOOL_PROFILE": "continuity-capture"}
            }
        }})
    );
    assert_eq!(fs::read_to_string(config).unwrap(), source);
}

#[cfg(unix)]
mod process {
    use super::*;
    use std::os::unix::{ffi::OsStringExt, fs::PermissionsExt};

    const CHILD_TEST: &str =
        "integrate::opencode_capture_tests::process::opencode_capture_process_entry";

    fn arguments() -> Vec<OsString> {
        vec![
            "run".into(),
            "a prompt with spaces".into(),
            "".into(),
            "--flag=value".into(),
            OsString::from_vec(vec![b'x', 0xff]),
        ]
    }

    // Run capture inside a fresh test process: environment selection is part of
    // the public behavior and must not race other tests through set_var.
    #[test]
    fn opencode_capture_process_entry() {
        let Some(root) = std::env::var_os("NOEMA_CAPTURE_TEST_ROOT") else {
            return;
        };
        let root = PathBuf::from(root);
        let request = capture_request(&root);
        let custom = root.join("custom-client");
        let default = std::env::var_os("NOEMA_CAPTURE_TEST_DEFAULT").is_some();
        let result = capture(
            &request,
            if default {
                None
            } else {
                Some(custom.as_path())
            },
            &arguments(),
        );
        match std::env::var("NOEMA_CAPTURE_TEST_ERROR").ok() {
            Some(expected) => assert!(
                result.unwrap_err().to_string().contains(&expected),
                "capture must report {expected}"
            ),
            None => result.unwrap(),
        }
    }

    fn write_client(path: &Path, version: &str) {
        fs::write(
            path,
            format!(
                "#!/bin/sh\nif [ \"$1\" = --version ]; then\n  printf 'probe\\n' >> \"$NOEMA_CAPTURE_TEST_ROOT/probes\"\n  printf '%s\\n' '{version}'\n  exit 0\nfi\nprintf '%s' \"$OPENCODE_CONFIG_CONTENT\" > \"$NOEMA_CAPTURE_TEST_ROOT/override.json\"\nprintf '%s\\0' \"$@\" > \"$NOEMA_CAPTURE_TEST_ROOT/args\"\nprintf '%s' \"$OPENCODE_CONFIG\" > \"$NOEMA_CAPTURE_TEST_ROOT/config-path\"\nexit \"${{NOEMA_CAPTURE_TEST_EXIT:-0}}\"\n"
            ),
        )
        .unwrap();
        fs::set_permissions(path, fs::Permissions::from_mode(0o700)).unwrap();
    }

    struct Fixture {
        temp: tempfile::TempDir,
        selected: PathBuf,
        source: String,
        default_source: String,
    }

    impl Fixture {
        fn new(version: &str, v2: bool) -> Self {
            let temp = tempfile::tempdir().unwrap();
            let root = temp.path();
            let request = capture_request(root);
            let config = request.target.opencode_config();
            fs::create_dir_all(config.parent().unwrap()).unwrap();
            // This target must never be inspected while OPENCODE_CONFIG selects
            // a different file, even for the initial version/layout decision.
            let default_source = "{ intentionally invalid default config\n".to_owned();
            fs::write(config, &default_source).unwrap();
            let selected = root.join("selected.jsonc");
            let server = json!({
                "type": "remote",
                "url": "https://private.example.com/mcp",
                "headers": {"Authorization": "Bearer fixture-private-token"},
                "oauth": false
            });
            let mcp = if v2 {
                json!({"servers": {"noema": server}})
            } else {
                json!({"noema": server})
            };
            let source = format!("// selected fixture\n{}\n", json!({"mcp": mcp}));
            fs::write(&selected, &source).unwrap();
            fs::create_dir_all(root.join("bin")).unwrap();
            write_client(&root.join("bin/opencode"), "opencode v1.9.0");
            write_client(&root.join("custom-client"), version);
            Self {
                temp,
                selected,
                source,
                default_source,
            }
        }

        fn command(&self) -> Command {
            let root = self.temp.path();
            let mut command = Command::new(std::env::current_exe().unwrap());
            command
                .args(["--exact", CHILD_TEST, "--nocapture"])
                .env("NOEMA_CAPTURE_TEST_ROOT", root)
                .env("HOME", root.join("home"))
                .env("XDG_CONFIG_HOME", root.join("config"))
                .env("XDG_STATE_HOME", root.join("state"))
                .env("PATH", root.join("bin"))
                .env("OPENCODE_CONFIG", &self.selected)
                .env_remove("OPENCODE_CONFIG_CONTENT")
                .env_remove("NOEMA_CAPTURE_TEST_DEFAULT")
                .env_remove("NOEMA_CAPTURE_TEST_ERROR")
                .env_remove("NOEMA_CAPTURE_TEST_EXIT");
            command
        }

        fn run(&self, command: &mut Command) {
            let output = command.output().unwrap();
            assert!(
                output.status.success(),
                "child test failed: {}\n{}",
                String::from_utf8_lossy(&output.stdout),
                String::from_utf8_lossy(&output.stderr)
            );
            assert!(String::from_utf8_lossy(&output.stdout).contains("1 passed"));
            assert_eq!(fs::read_to_string(&self.selected).unwrap(), self.source);
            let config = capture_request(self.temp.path()).target.opencode_config();
            assert_eq!(fs::read_to_string(config).unwrap(), self.default_source);
        }

        fn assert_launch(&self, v2: bool) {
            let root = self.temp.path();
            assert_eq!(fs::read_to_string(root.join("probes")).unwrap(), "probe\n");
            let mut expected_args = Vec::new();
            for arg in arguments() {
                expected_args.extend(arg.into_vec());
                expected_args.push(0);
            }
            assert_eq!(fs::read(root.join("args")).unwrap(), expected_args);
            assert_eq!(
                fs::read(root.join("config-path")).unwrap(),
                self.selected.as_os_str().as_encoded_bytes()
            );
            let output = fs::read_to_string(root.join("override.json")).unwrap();
            assert!(!output.contains("private"));
            assert!(!output.contains("Authorization"));
            let value: Value = serde_json::from_str(&output).unwrap();
            let servers = if v2 {
                &value["mcp"]["servers"]
            } else {
                &value["mcp"]
            };
            assert_eq!(servers["noema_capture"]["type"], "local");
            assert_eq!(
                servers["noema_capture"]["environment"]["NOEMA_MCP_TOOL_PROFILE"],
                "continuity-capture"
            );
            if v2 {
                assert_eq!(servers["noema"]["disabled"], true);
                assert_eq!(servers["noema_capture"]["disabled"], false);
                assert!(servers["noema"].get("enabled").is_none());
            } else {
                assert_eq!(servers["noema"]["enabled"], false);
                assert_eq!(servers["noema_capture"]["enabled"], true);
            }
        }
    }

    #[test]
    fn opencode_capture_custom_binary_and_selected_config_forward_arguments() {
        let fixture = Fixture::new("opencode v2.1.4", true);
        fixture.run(&mut fixture.command());
        fixture.assert_launch(true);
    }

    #[test]
    fn opencode_capture_default_binary_preserves_v1_behavior() {
        let fixture = Fixture::new("opencode v3.0.0", false);
        fixture.run(fixture.command().env("NOEMA_CAPTURE_TEST_DEFAULT", "1"));
        fixture.assert_launch(false);
    }

    #[test]
    fn opencode_capture_child_failure_is_reported_after_forwarding() {
        let fixture = Fixture::new("v2.1.4", true);
        fixture.run(
            fixture
                .command()
                .env("NOEMA_CAPTURE_TEST_EXIT", "23")
                .env("NOEMA_CAPTURE_TEST_ERROR", "exited with exit status: 23"),
        );
        fixture.assert_launch(true);
    }

    #[test]
    fn opencode_capture_refuses_existing_override_without_launching() {
        for content in ["", "{}"] {
            let fixture = Fixture::new("opencode v2.1.4", true);
            fixture.run(
                fixture
                    .command()
                    .env("OPENCODE_CONFIG_CONTENT", content)
                    .env(
                        "NOEMA_CAPTURE_TEST_ERROR",
                        "existing OPENCODE_CONFIG_CONTENT",
                    ),
            );
            assert!(!fixture.temp.path().join("probes").exists());
            assert!(!fixture.temp.path().join("override.json").exists());
        }
    }

    #[test]
    fn opencode_capture_custom_version_controls_layout_and_major_refusal() {
        for (version, v2, error) in [
            ("opencode v1.9.0", true, "v1 cannot use a v2"),
            (
                "opencode v3.0.0",
                false,
                "unsupported OpenCode major version 3",
            ),
        ] {
            let fixture = Fixture::new(version, v2);
            fixture.run(fixture.command().env("NOEMA_CAPTURE_TEST_ERROR", error));
            assert_eq!(
                fs::read_to_string(fixture.temp.path().join("probes")).unwrap(),
                "probe\n"
            );
            assert!(!fixture.temp.path().join("override.json").exists());
        }
    }
}
