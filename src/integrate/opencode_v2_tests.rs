use super::*;

fn request(root: &Path) -> Request {
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
            binary: PathBuf::from("/usr/bin/noema"),
            cortex: "test-cortex".into(),
            transport: Transport::Stdio,
            url: None,
            bearer_token_env: None,
        },
        prefetch: PrefetchInstall::Disabled,
    }
}

#[test]
fn opencode_v2_mcp_migration_and_idempotence() {
    let temp = tempfile::tempdir().unwrap();
    let request = request(temp.path());
    let source = "{\n // retained\n \"mcp\": {\n // sibling\n \"docs\": {\"type\":\"remote\",\"url\":\"https://example.com/mcp\"},\n \"noema\": {\n   // server policy\n   \"type\":\"local\",\"command\":[\"/usr/bin/noema\",\"--cortex\",\"test-cortex\",\"serve\",\"--transport\",\"stdio\"],\"enabled\":true,\"cwd\":\"/tmp\",\"environment\":{\"EXTRA\":\"kept\"},\"timeout\":{\"catalog\":1234}}\n }}\n";
    let (state, next) = opencode_mcp_install(source, &request, OpenCodeVersion::V2, false).unwrap();
    assert_eq!(state, State::WouldReplace);
    let value = parse_jsonc_value(&next).unwrap();
    assert!(value["mcp"].get("noema").is_none());
    assert_eq!(value["mcp"]["servers"]["noema"]["disabled"], false);
    assert_eq!(
        value["mcp"]["servers"]["noema"]["environment"]["EXTRA"],
        "kept"
    );
    assert_eq!(value["mcp"]["servers"]["noema"]["timeout"]["catalog"], 1234);
    assert_eq!(value["mcp"]["docs"]["url"], "https://example.com/mcp");
    assert!(
        next.contains("// retained")
            && next.contains("// sibling")
            && next.contains("// server policy")
    );
    assert_eq!(
        opencode_mcp_install(&next, &request, OpenCodeVersion::V2, false).unwrap(),
        (State::Unchanged, next)
    );
}

#[test]
fn opencode_v2_http_optional_disabled_unchanged() {
    let temp = tempfile::tempdir().unwrap();
    let mut request = request(temp.path());
    request.connection.transport = Transport::Http;
    request.connection.url = Some("https://example.com/mcp".into());
    request.connection.bearer_token_env = Some("MCP_KEY".into());
    let source = r#"{"mcp":{"servers":{"noema":{"type":"remote","url":"https://example.com/mcp","oauth":false,"headers":{"Authorization":"Bearer {env:MCP_KEY}","X-Extra":"kept"}}}}}"#;
    assert_eq!(
        opencode_mcp_install(source, &request, OpenCodeVersion::V2, false).unwrap(),
        (State::Unchanged, source.into())
    );
}

#[test]
fn opencode_v2_preflight_zero_writes() {
    let temp = tempfile::tempdir().unwrap();
    let request = request(temp.path());
    let path = request.target.opencode_config();
    fs::create_dir_all(path.parent().unwrap()).unwrap();
    for source in [
        r#"{"mcp":{"noema":{"type":"local","command":["noema","serve"]},"servers":{"noema":{"type":"local","command":["noema","serve"]}}}}"#,
        r#"{"mcp":{"servers":{"noema":{"type":"local","command":["other","serve"]}}}}"#,
        r#"{"mcp":{"servers":{"noema":{"type":"local","command":["noema","serve"]}}}}"#,
    ] {
        fs::write(&path, source).unwrap();
        assert!(
            opencode_install_version(&request, OpenCodeVersion::V2, false, false)
                .unwrap()
                .refused()
        );
        assert_eq!(fs::read_to_string(&path).unwrap(), source);
        assert!(!request.target.opencode_plugin().exists());
    }
    for source in [
        r#"{"mcp":{"noema":{"type":"local","command":["noema","serve"],"timeout":10000}}}"#,
        r#"{"mcp":{"noema":{"type":"local","command":["noema","serve"],"enabled":true,"disabled":true}}}"#,
        r#"{"mcp":{"noema":{"type":"local","command":["noema","serve"],"unknown":"private"}}}"#,
    ] {
        fs::write(&path, source).unwrap();
        let error = opencode_install_version(&request, OpenCodeVersion::V2, false, true)
            .unwrap_err()
            .to_string();
        assert!(!error.contains("private"));
        assert_eq!(fs::read_to_string(&path).unwrap(), source);
        assert!(!request.target.opencode_plugin().exists());
    }
}

#[test]
fn opencode_v2_clean_check_status_remove() {
    let temp = tempfile::tempdir().unwrap();
    let request = request(temp.path());
    let path = request.target.opencode_config();
    assert_eq!(
        opencode_install_version(&request, OpenCodeVersion::V2, true, false)
            .unwrap()
            .components[0]
            .state,
        State::WouldInstall
    );
    assert!(!path.exists() && !request.target.opencode_plugin().exists());
    assert!(
        opencode_install_version(&request, OpenCodeVersion::V2, false, false)
            .unwrap()
            .healthy()
    );
    let source = fs::read_to_string(&path).unwrap();
    assert_eq!(
        parse_jsonc_value(&source).unwrap()["mcp"]["servers"]["noema"]["disabled"],
        false
    );
    assert_eq!(
        opencode_install_version(&request, OpenCodeVersion::V2, false, false)
            .unwrap()
            .components[0]
            .state,
        State::Unchanged
    );
    fs::remove_file(request.target.opencode_plugin()).unwrap();
    let status = opencode_status_version(&request.target, OpenCodeVersion::V2).unwrap();
    assert_eq!(status.components[0].state, State::Configured);
    assert_eq!(status.components[1].state, State::NotInstalled);
    assert_eq!(
        opencode_remove_version(&request.target, OpenCodeVersion::V2, true, false)
            .unwrap()
            .components[0]
            .state,
        State::WouldRemove
    );
    assert_eq!(fs::read_to_string(&path).unwrap(), source);
    assert_eq!(
        opencode_remove_version(&request.target, OpenCodeVersion::V2, false, false)
            .unwrap()
            .components[0]
            .state,
        State::Removed
    );
    assert!(
        parse_jsonc_value(&fs::read_to_string(path).unwrap()).unwrap()["mcp"]["servers"]
            .get("noema")
            .is_none()
    );
}

#[test]
fn opencode_v2_bootstrap_exact_ownership_migration() {
    let temp = tempfile::tempdir().unwrap();
    let request = request(temp.path());
    let path = request.target.opencode_plugin();
    fs::create_dir_all(path.parent().unwrap()).unwrap();
    for legacy in [
        LEGACY_OPENCODE_PLUGIN_V1.to_owned(),
        opencode_plugin_source(),
    ] {
        fs::write(&path, &legacy).unwrap();
        assert_eq!(
            inspect_opencode_plugin(&path, OpenCodeVersion::V2).unwrap(),
            State::Drift
        );
        assert_eq!(
            install_opencode_plugin(&path, OpenCodeVersion::V2, true, false).unwrap(),
            State::WouldReplace
        );
        assert_eq!(fs::read_to_string(&path).unwrap(), legacy);
        assert_eq!(
            install_opencode_plugin(&path, OpenCodeVersion::V2, false, false).unwrap(),
            State::Replaced
        );
        assert_eq!(
            install_opencode_plugin(&path, OpenCodeVersion::V2, false, false).unwrap(),
            State::Unchanged
        );
        assert_eq!(
            remove_opencode_plugin(&path, false, false).unwrap(),
            State::Removed
        );
    }
    let customized = opencode_plugin_source().replace(
        "output.system.push(bootstrap)",
        "output.system.push('custom policy')",
    );
    fs::write(&path, &customized).unwrap();
    assert!(
        opencode_install_version(&request, OpenCodeVersion::V2, false, false)
            .unwrap()
            .refused()
    );
    assert_eq!(fs::read_to_string(&path).unwrap(), customized);
    assert!(!request.target.opencode_config().exists());
    assert_eq!(
        remove_opencode_plugin(&path, false, false).unwrap(),
        State::Drift
    );
}

#[test]
fn opencode_v2_force_migration_and_dual_location_removal() {
    let temp = tempfile::tempdir().unwrap();
    let request = request(temp.path());
    let path = request.target.opencode_config();
    fs::create_dir_all(path.parent().unwrap()).unwrap();
    let source = r#"{"mcp":{"noema":{"type":"remote","url":"https://example.com/mcp","enabled":true},"servers":{"docs":{"type":"local","command":["docs"]}},"timeout":{"startup":1234}}}"#;
    fs::write(&path, source).unwrap();
    assert_eq!(
        opencode_install_version(&request, OpenCodeVersion::V2, false, false)
            .unwrap()
            .components[0]
            .state,
        State::Drift
    );
    assert_eq!(fs::read_to_string(&path).unwrap(), source);
    assert_eq!(
        opencode_install_version(&request, OpenCodeVersion::V2, false, true)
            .unwrap()
            .components[0]
            .state,
        State::Replaced
    );
    let migrated = fs::read_to_string(&path).unwrap();
    let value = parse_jsonc_value(&migrated).unwrap();
    assert_eq!(
        value["mcp"]["servers"]["noema"]["command"][0],
        "/usr/bin/noema"
    );
    assert_eq!(value["mcp"]["servers"]["docs"]["command"][0], "docs");
    assert_eq!(value["mcp"]["timeout"]["startup"], 1234);
    let duplicate = insert_json_path_member(
        &migrated,
        &["mcp"],
        "noema",
        &json!({"type":"remote","url":"https://example.com/mcp"}),
    )
    .unwrap();
    fs::write(&path, &duplicate).unwrap();
    assert_eq!(
        opencode_status_version(&request.target, OpenCodeVersion::V2)
            .unwrap()
            .components[0]
            .state,
        State::Conflict
    );
    assert_eq!(
        opencode_remove_version(&request.target, OpenCodeVersion::V2, false, false)
            .unwrap()
            .components[0]
            .state,
        State::Removed
    );
    let value = parse_jsonc_value(&fs::read_to_string(&path).unwrap()).unwrap();
    assert!(value["mcp"].get("noema").is_none() && value["mcp"]["servers"].get("noema").is_none());
    assert_eq!(value["mcp"]["servers"]["docs"]["command"][0], "docs");
    let unknown = r#"{"mcp":{"noema":{"type":"local","command":["unknown"]},"servers":{"noema":{"type":"local","command":["noema","serve"]}}}}"#;
    fs::write(&path, unknown).unwrap();
    assert!(
        opencode_remove_version(&request.target, OpenCodeVersion::V2, false, false)
            .unwrap()
            .refused()
    );
    assert_eq!(fs::read_to_string(&path).unwrap(), unknown);
}

#[test]
fn opencode_v2_reuses_existing_json_config() {
    let temp = tempfile::tempdir().unwrap();
    let request = request(temp.path());
    let jsonc = request.target.opencode_config();
    let json = jsonc.with_extension("json");
    fs::create_dir_all(json.parent().unwrap()).unwrap();
    fs::write(&json, "{}\n").unwrap();
    assert_eq!(request.target.opencode_config(), json);
    assert!(
        opencode_install_version(&request, OpenCodeVersion::V2, false, false)
            .unwrap()
            .healthy()
    );
    assert!(!jsonc.exists());
    assert_eq!(
        opencode_status_version(&request.target, OpenCodeVersion::V2)
            .unwrap()
            .components[0]
            .state,
        State::Configured
    );
    assert_eq!(
        opencode_remove_version(&request.target, OpenCodeVersion::V2, false, false)
            .unwrap()
            .components[0]
            .state,
        State::Removed
    );
    assert!(!jsonc.exists());
}

#[test]
fn opencode_directory_bootstrap_conflict_preserves_all_components() {
    for version in [OpenCodeVersion::V1, OpenCodeVersion::V2] {
        for flat in [false, true] {
            let temp = tempfile::tempdir().unwrap();
            let request = request(temp.path());
            let config = request.target.opencode_config();
            let plugin = request.target.opencode_plugin();
            let directory = plugin.with_extension("");
            fs::create_dir_all(&directory).unwrap();
            let custom = "export default { id: 'custom-bootstrap', setup() {} }\n";
            let package =
                "{\"type\":\"module\",\"dependencies\":{\"@opencode/plugin\":\"2.1.4\"}}\n";
            fs::write(directory.join("index.ts"), custom).unwrap();
            fs::write(directory.join("package.json"), package).unwrap();
            let managed = opencode_plugin_source_for(version);
            if flat {
                fs::write(&plugin, &managed).unwrap();
            }
            assert_eq!(
                opencode_status_version(&request.target, version)
                    .unwrap()
                    .components[1]
                    .state,
                State::Conflict
            );
            // A directory conflict must prevent even an otherwise clean MCP install.
            for check in [false, true] {
                for force in [false, true] {
                    let report = opencode_install_version(&request, version, check, force).unwrap();
                    assert_eq!(report.components[1].state, State::Conflict);
                    assert!(report.refused());
                    assert!(!config.exists());
                }
            }
            let source = insert_json_path_member(
                "{\n // retain existing MCP configuration\n \"mcp\": {}\n}\n",
                version.mcp_path(),
                "noema",
                &opencode_mcp_value(&request, version).unwrap(),
            )
            .unwrap();
            fs::write(&config, &source).unwrap();
            for check in [false, true] {
                for force in [false, true] {
                    assert!(
                        opencode_install_version(&request, version, check, force)
                            .unwrap()
                            .refused()
                    );
                    assert!(
                        opencode_remove_version(&request.target, version, check, force)
                            .unwrap()
                            .refused()
                    );
                    assert_eq!(fs::read_to_string(&config).unwrap(), source);
                }
            }
            assert_eq!(
                fs::read_to_string(directory.join("index.ts")).unwrap(),
                custom
            );
            assert_eq!(
                fs::read_to_string(directory.join("package.json")).unwrap(),
                package
            );
            if flat {
                assert_eq!(fs::read_to_string(plugin).unwrap(), managed);
            } else {
                assert!(!plugin.exists());
            }
        }
    }
}

#[cfg(unix)]
#[test]
fn opencode_symlinked_directory_bootstrap_is_an_ownership_conflict() {
    let temp = tempfile::tempdir().unwrap();
    let request = request(temp.path());
    let plugin = request.target.opencode_plugin();
    let custom = temp.path().join("custom-plugin");
    fs::create_dir_all(&custom).unwrap();
    fs::create_dir_all(plugin.parent().unwrap()).unwrap();
    fs::write(custom.join("index.ts"), "custom bootstrap\n").unwrap();
    std::os::unix::fs::symlink(&custom, plugin.with_extension("")).unwrap();
    assert!(
        opencode_install_version(&request, OpenCodeVersion::V2, false, true)
            .unwrap()
            .refused()
    );
    assert!(!plugin.exists());
    assert!(!request.target.opencode_config().exists());
    assert_eq!(
        fs::read_to_string(custom.join("index.ts")).unwrap(),
        "custom bootstrap\n"
    );
}
