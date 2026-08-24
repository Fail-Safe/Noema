use std::{fs, path::Path, process::Command};

fn noema(home: &Path, config: &Path) -> Command {
    let mut command = Command::new(env!("CARGO_BIN_EXE_noema"));
    command.env("HOME", home).env("XDG_CONFIG_HOME", config);
    command
}

#[test]
fn codex_integration_cli_supports_check_install_status_and_remove() {
    let temp = tempfile::tempdir().unwrap();
    let home = temp.path().join("home");
    let config = temp.path().join("config");
    let cortexes = temp.path().join("cortexes");
    fs::create_dir_all(&home).unwrap();
    fs::create_dir_all(&config).unwrap();
    fs::create_dir_all(&cortexes).unwrap();

    let init = noema(&home, &config)
        .args([
            "init",
            "--name",
            "shared",
            "--path",
            cortexes.to_str().unwrap(),
        ])
        .output()
        .unwrap();
    assert!(
        init.status.success(),
        "{}",
        String::from_utf8_lossy(&init.stderr)
    );

    let preview = noema(&home, &config)
        .args([
            "--cortex",
            "shared",
            "integrate",
            "codex",
            "install",
            "--scope",
            "user",
            "--check",
        ])
        .output()
        .unwrap();
    assert!(!preview.status.success());
    assert!(String::from_utf8_lossy(&preview.stdout).contains("would install"));
    assert!(!home.join(".codex/config.toml").exists());

    let install = noema(&home, &config)
        .args([
            "--cortex",
            "shared",
            "integrate",
            "codex",
            "install",
            "--scope",
            "user",
        ])
        .output()
        .unwrap();
    assert!(
        install.status.success(),
        "{}",
        String::from_utf8_lossy(&install.stderr)
    );
    let installed = fs::read_to_string(home.join(".codex/config.toml")).unwrap();
    assert!(installed.contains("[mcp_servers.noema]"));
    assert!(installed.contains("[[hooks.SessionStart]]"));
    assert!(installed.contains("--cortex"));
    assert!(installed.contains("shared"));

    fs::write(
        home.join(".codex/config.toml"),
        installed.replace("timeout = 5", "timeout = 9"),
    )
    .unwrap();
    let refused = noema(&home, &config)
        .args([
            "--cortex",
            "shared",
            "integrate",
            "codex",
            "install",
            "--scope",
            "user",
        ])
        .output()
        .unwrap();
    assert!(!refused.status.success());
    assert!(String::from_utf8_lossy(&refused.stderr).contains("managed components have drifted"));

    let repair = noema(&home, &config)
        .args([
            "--cortex",
            "shared",
            "integrate",
            "codex",
            "install",
            "--scope",
            "user",
            "--force",
        ])
        .output()
        .unwrap();
    assert!(
        repair.status.success(),
        "{}",
        String::from_utf8_lossy(&repair.stderr)
    );

    let status = noema(&home, &config)
        .args([
            "--cortex",
            "shared",
            "integrate",
            "codex",
            "status",
            "--scope",
            "user",
            "--check",
        ])
        .output()
        .unwrap();
    assert!(
        status.status.success(),
        "{}",
        String::from_utf8_lossy(&status.stderr)
    );
    assert!(String::from_utf8_lossy(&status.stdout).contains("configured"));

    let remove = noema(&home, &config)
        .args([
            "--cortex",
            "shared",
            "integrate",
            "codex",
            "remove",
            "--scope",
            "user",
        ])
        .output()
        .unwrap();
    assert!(
        remove.status.success(),
        "{}",
        String::from_utf8_lossy(&remove.stderr)
    );
    assert!(
        !fs::read_to_string(home.join(".codex/config.toml"))
            .unwrap()
            .contains("mcp_servers.noema")
    );
}

#[test]
fn codex_integration_cli_adopts_compatible_http_configuration() {
    let temp = tempfile::tempdir().unwrap();
    let home = temp.path().join("home");
    let config = temp.path().join("config");
    let cortexes = temp.path().join("cortexes");
    fs::create_dir_all(home.join(".codex")).unwrap();
    fs::create_dir_all(&config).unwrap();
    fs::create_dir_all(&cortexes).unwrap();

    let init = noema(&home, &config)
        .args([
            "init",
            "--name",
            "shared",
            "--path",
            cortexes.to_str().unwrap(),
        ])
        .output()
        .unwrap();
    assert!(
        init.status.success(),
        "{}",
        String::from_utf8_lossy(&init.stderr)
    );

    let path = home.join(".codex/config.toml");
    fs::write(
        &path,
        r#"model = "gpt-5"

[mcp_servers.noema]
url = "https://memory.example.com/mcp"
default_tools_approval_mode = "approve"
bearer_token_env_var = "NOEMA_MCP_KEY"

[mcp_servers.docs]
url = "https://docs.example.com/mcp"
"#,
    )
    .unwrap();

    let install = noema(&home, &config)
        .args([
            "--cortex",
            "shared",
            "integrate",
            "codex",
            "install",
            "--scope",
            "user",
            "--transport",
            "http",
            "--url",
            "https://memory.example.com/mcp",
            "--bearer-token-env",
            "NOEMA_MCP_KEY",
        ])
        .output()
        .unwrap();
    assert!(
        install.status.success(),
        "{}",
        String::from_utf8_lossy(&install.stderr)
    );
    assert!(String::from_utf8_lossy(&install.stdout).contains("replaced"));
    let installed = fs::read_to_string(&path).unwrap();
    assert!(installed.contains("default_tools_approval_mode = \"approve\""));
    assert!(installed.contains("# >>> noema integrate codex v1"));
    assert!(installed.contains("[[hooks.SessionStart]]"));
    assert!(installed.contains("[mcp_servers.docs]"));

    let status = noema(&home, &config)
        .args(["integrate", "codex", "status", "--scope", "user", "--check"])
        .output()
        .unwrap();
    assert!(
        status.status.success(),
        "{}",
        String::from_utf8_lossy(&status.stderr)
    );
    let stdout = String::from_utf8_lossy(&status.stdout);
    assert!(stdout.contains("configured"));
    assert!(stdout.contains("mcp + bootstrap (http)"));
}

#[test]
fn integration_status_and_remove_do_not_require_a_cortex() {
    let temp = tempfile::tempdir().unwrap();
    let home = temp.path().join("home");
    let config = temp.path().join("config");
    fs::create_dir_all(&home).unwrap();
    fs::create_dir_all(&config).unwrap();

    let status = noema(&home, &config)
        .args(["integrate", "codex", "status", "--scope", "user"])
        .output()
        .unwrap();
    assert!(
        status.status.success(),
        "{}",
        String::from_utf8_lossy(&status.stderr)
    );
    assert!(String::from_utf8_lossy(&status.stdout).contains("not installed"));

    let all_status = noema(&home, &config)
        .args(["integrate", "status", "--scope", "user"])
        .output()
        .unwrap();
    assert!(
        all_status.status.success(),
        "{}",
        String::from_utf8_lossy(&all_status.stderr)
    );
    let stdout = String::from_utf8_lossy(&all_status.stdout);
    assert!(stdout.contains("codex (user)"));
    assert!(stdout.contains("claude-code (user)"));
    assert!(stdout.contains("opencode (user)"));

    let remove = noema(&home, &config)
        .args(["integrate", "codex", "remove", "--scope", "user", "--check"])
        .output()
        .unwrap();
    assert!(
        remove.status.success(),
        "{}",
        String::from_utf8_lossy(&remove.stderr)
    );
}
