use std::{
    fs,
    io::Write,
    path::Path,
    process::{Command, Stdio},
};

#[cfg(unix)]
use std::os::unix::fs::PermissionsExt;

fn noema(home: &Path, config: &Path) -> Command {
    let mut command = Command::new(env!("CARGO_BIN_EXE_noema"));
    command.env("HOME", home).env("XDG_CONFIG_HOME", config);
    command
}

#[cfg(unix)]
fn fake_client(path: &Path) {
    fs::write(
        path,
        r#"#!/bin/sh
if [ "$1" = "--version" ]; then
  printf '%s\n' '1.0.0'
  exit 0
fi
{
  printf 'profile=%s\n' "$NOEMA_MCP_TOOL_PROFILE"
  printf 'inline=%s\n' "$OPENCODE_CONFIG_CONTENT"
  for argument in "$@"; do
    printf 'arg=%s\n' "$argument"
  done
} > "$NOEMA_CAPTURE_TEST_OUTPUT"
"#,
    )
    .unwrap();
    let mut permissions = fs::metadata(path).unwrap().permissions();
    permissions.set_mode(0o755);
    fs::set_permissions(path, permissions).unwrap();
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
    assert!(!home.join(".codex/hooks.json").exists());

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

#[cfg(unix)]
#[test]
fn capture_sessions_are_ephemeral_and_forward_client_arguments() {
    let temp = tempfile::tempdir().unwrap();
    let home = temp.path().join("home");
    let config = temp.path().join("config");
    let cortexes = temp.path().join("cortexes");
    let bin = temp.path().join("bin");
    fs::create_dir_all(&home).unwrap();
    fs::create_dir_all(&config).unwrap();
    fs::create_dir_all(&cortexes).unwrap();
    fs::create_dir_all(&bin).unwrap();
    fake_client(&bin.join("codex"));
    fake_client(&bin.join("opencode"));
    let path = std::env::join_paths(std::iter::once(bin.clone()).chain(std::env::split_paths(
        &std::env::var_os("PATH").unwrap_or_default(),
    )))
    .unwrap();

    assert!(
        noema(&home, &config)
            .args([
                "init",
                "--name",
                "shared",
                "--path",
                cortexes.to_str().unwrap(),
            ])
            .env("PATH", &path)
            .output()
            .unwrap()
            .status
            .success()
    );
    for client in ["codex", "opencode"] {
        let mut install_command = noema(&home, &config);
        install_command.args([
            "--cortex",
            "shared",
            "integrate",
            client,
            "install",
            "--scope",
            "user",
        ]);
        if client == "opencode" {
            install_command.args([
                "--transport",
                "http",
                "--url",
                "https://memory.example.com/mcp",
            ]);
        }
        let install = install_command.env("PATH", &path).output().unwrap();
        assert!(
            install.status.success(),
            "{client}: {}",
            String::from_utf8_lossy(&install.stderr)
        );

        let capture_config_env = if client == "codex" {
            let custom_root = temp.path().join("codex-runtime");
            fs::create_dir_all(&custom_root).unwrap();
            fs::copy(
                home.join(".codex/config.toml"),
                custom_root.join("config.toml"),
            )
            .unwrap();
            let capture_config = custom_root.join("config.toml");
            let source = fs::read_to_string(&capture_config).unwrap();
            fs::write(
                &capture_config,
                source.replace(
                    "# <<< noema integrate codex v1",
                    "[projects.\"/tmp/capture-workspace\"]\ntrust_level = \"trusted\"\n# <<< noema integrate codex v1",
                ),
            )
            .unwrap();
            fs::write(home.join(".codex/config.toml"), "model = \"gpt-5\"\n").unwrap();
            ("CODEX_HOME", custom_root)
        } else {
            let custom_config = temp.path().join("opencode-runtime.jsonc");
            fs::copy(config.join("opencode/opencode.jsonc"), &custom_config).unwrap();
            fs::write(config.join("opencode/opencode.jsonc"), "{}\n").unwrap();
            ("OPENCODE_CONFIG", custom_config)
        };
        let capture_output = temp.path().join(format!("{client}-capture.txt"));
        let client_action = if client == "codex" { "exec" } else { "run" };
        let mut capture_command = noema(&home, &config);
        capture_command.args([
            "--cortex",
            "shared",
            "integrate",
            client,
            "capture",
            "--scope",
            "user",
            "--client-binary",
            bin.join(client).to_str().unwrap(),
            "--",
            client_action,
            "remember this",
        ]);
        capture_command.env(capture_config_env.0, capture_config_env.1);
        let capture = capture_command
            .env("NOEMA_MCP_TOOL_PROFILE", "full")
            .env("PATH", &path)
            .env("NOEMA_CAPTURE_TEST_OUTPUT", &capture_output)
            .output()
            .unwrap();
        assert!(
            capture.status.success(),
            "{client}: {}",
            String::from_utf8_lossy(&capture.stderr)
        );
        let launched = fs::read_to_string(capture_output).unwrap();
        assert!(launched.contains("profile=full\n"));
        assert!(launched.contains(&format!("arg={client_action}")));
        assert!(launched.contains("arg=remember this"));
        if client == "codex" {
            assert!(launched.contains("arg=--config"));
            assert!(launched.contains(
                "arg=mcp_servers.noema.env.NOEMA_MCP_TOOL_PROFILE=\"continuity-capture\""
            ));
            assert!(!launched.contains("arg=mcp_servers.noema.enabled=false"));
            assert!(!launched.contains("arg=mcp_servers.noema_capture.command="));
            assert!(launched.contains(
                "arg=mcp_servers.noema.tools.get_instructions.approval_mode=\"approve\""
            ));
            assert!(
                launched.contains(
                    "arg=mcp_servers.noema.tools.create_traces.approval_mode=\"approve\""
                )
            );
            assert!(launched.contains("arg=mcp_servers.noema.env.XDG_CONFIG_HOME="));
            assert!(launched.find("arg=exec").unwrap() < launched.find("arg=--config").unwrap());
        } else {
            assert!(!launched.contains("arg=--config"));
            assert!(launched.contains("\"noema\""));
            assert!(launched.contains("\"enabled\":false"));
            assert!(launched.contains("\"noema_capture\""));
            assert!(launched.contains("\"NOEMA_MCP_TOOL_PROFILE\":\"continuity-capture\""));
        }
    }
}

#[test]
fn codex_prefetch_integration_is_opt_in_and_preserves_unrelated_hooks() {
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
    assert!(init.status.success());

    let hook_path = home.join(".codex/hooks.json");
    let original = "{\n  \"hooks\": {\n    \"Stop\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"keep-stop\"}]}]\n  },\n  \"theme\": \"dark\"\n}\n";
    fs::write(&hook_path, original).unwrap();

    let preview = noema(&home, &config)
        .args([
            "--cortex",
            "shared",
            "integrate",
            "codex",
            "install",
            "--scope",
            "user",
            "--prefetch",
            "--check",
        ])
        .output()
        .unwrap();
    assert!(!preview.status.success());
    assert!(String::from_utf8_lossy(&preview.stdout).contains("prompt prefetch"));
    assert_eq!(fs::read_to_string(&hook_path).unwrap(), original);
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
            "--prefetch",
        ])
        .output()
        .unwrap();
    assert!(
        install.status.success(),
        "{}",
        String::from_utf8_lossy(&install.stderr)
    );
    let installed = fs::read_to_string(&hook_path).unwrap();
    assert!(installed.contains("keep-stop"));
    assert!(installed.contains("\"theme\": \"dark\""));
    assert!(installed.contains("noema-managed-codex-prefetch-v1"));
    assert!(installed.contains("noema-managed-codex-preferences-v1"));
    assert!(installed.contains("--max-preferences 0"));
    assert!(installed.contains("--max-results 0"));
    assert!(installed.contains("--max-preferences 24"));
    assert!(installed.contains("--fail-open"));

    #[cfg(unix)]
    {
        let hooks: serde_json::Value = serde_json::from_str(&installed).unwrap();
        let hook_command = hooks["hooks"]["UserPromptSubmit"][0]["hooks"][0]["command"]
            .as_str()
            .unwrap();
        let mut child = Command::new("sh")
            .args(["-c", hook_command])
            .env("HOME", &home)
            .env("XDG_CONFIG_HOME", &config)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        child
            .stdin
            .take()
            .unwrap()
            .write_all(br#"{"prompt":"What memory is relevant?"}"#)
            .unwrap();
        let output = child.wait_with_output().unwrap();
        assert!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
        );
        let envelope: serde_json::Value = serde_json::from_slice(&output.stdout).unwrap();
        assert_eq!(
            envelope["hookSpecificOutput"]["hookEventName"],
            "UserPromptSubmit"
        );
    }

    let status = noema(&home, &config)
        .args(["integrate", "codex", "status", "--scope", "user", "--check"])
        .output()
        .unwrap();
    assert!(
        status.status.success(),
        "{}",
        String::from_utf8_lossy(&status.stderr)
    );
    assert!(String::from_utf8_lossy(&status.stdout).contains("prompt prefetch"));
    assert!(String::from_utf8_lossy(&status.stdout).contains("startup preferences"));

    fs::write(
        &hook_path,
        installed.replace("\"timeout\": 15", "\"timeout\": 9"),
    )
    .unwrap();
    let drift = noema(&home, &config)
        .args(["integrate", "codex", "status", "--scope", "user", "--check"])
        .output()
        .unwrap();
    assert!(!drift.status.success());
    assert!(String::from_utf8_lossy(&drift.stdout).contains("drift"));

    let repair = noema(&home, &config)
        .args([
            "--cortex",
            "shared",
            "integrate",
            "codex",
            "install",
            "--scope",
            "user",
            "--prefetch",
            "--force",
        ])
        .output()
        .unwrap();
    assert!(repair.status.success());

    let remove = noema(&home, &config)
        .args(["integrate", "codex", "remove", "--scope", "user"])
        .output()
        .unwrap();
    assert!(remove.status.success());
    let removed = fs::read_to_string(&hook_path).unwrap();
    assert!(removed.contains("keep-stop"));
    assert!(removed.contains("\"theme\": \"dark\""));
    assert!(!removed.contains("noema-managed-codex-prefetch-v1"));
    assert!(!removed.contains("noema-managed-codex-preferences-v1"));
}

#[test]
fn codex_prefetch_only_cli_leaves_existing_mcp_bytes_unchanged() {
    let temp = tempfile::tempdir().unwrap();
    let home = temp.path().join("home");
    let config = temp.path().join("config");
    let cortexes = temp.path().join("cortexes");
    fs::create_dir_all(home.join(".codex")).unwrap();
    fs::create_dir_all(&config).unwrap();
    fs::create_dir_all(&cortexes).unwrap();
    assert!(
        noema(&home, &config)
            .args([
                "init",
                "--name",
                "shared",
                "--path",
                cortexes.to_str().unwrap(),
            ])
            .output()
            .unwrap()
            .status
            .success()
    );

    let config_path = home.join(".codex/config.toml");
    let original = b"[mcp_servers.noema]\nenabled = true\nurl = \"https://memory.example.com/mcp\"\n\n[mcp_servers.noema.http_headers]\nAuthorization = \"Bearer fixture-secret\"\n";
    fs::write(&config_path, original).unwrap();
    let install = noema(&home, &config)
        .args([
            "--cortex",
            "shared",
            "integrate",
            "codex",
            "install",
            "--scope",
            "user",
            "--prefetch-only",
        ])
        .output()
        .unwrap();
    assert!(
        install.status.success(),
        "{}",
        String::from_utf8_lossy(&install.stderr)
    );
    assert_eq!(fs::read(&config_path).unwrap(), original);
    assert!(
        fs::read_to_string(home.join(".codex/hooks.json"))
            .unwrap()
            .contains("noema-managed-codex-prefetch-v1")
    );
    assert!(
        fs::read_to_string(home.join(".codex/hooks.json"))
            .unwrap()
            .contains("noema-managed-codex-preferences-v1")
    );
}

#[test]
fn codex_continuity_cli_installs_a_selectable_profile_without_rewriting_mcp() {
    let temp = tempfile::tempdir().unwrap();
    let home = temp.path().join("home");
    let config = temp.path().join("config");
    let cortexes = temp.path().join("cortexes");
    fs::create_dir_all(home.join(".codex")).unwrap();
    fs::create_dir_all(&config).unwrap();
    fs::create_dir_all(&cortexes).unwrap();
    assert!(
        noema(&home, &config)
            .args([
                "init",
                "--name",
                "shared",
                "--path",
                cortexes.to_str().unwrap(),
            ])
            .output()
            .unwrap()
            .status
            .success()
    );

    let config_path = home.join(".codex/config.toml");
    let original = b"[mcp_servers.noema]\nenabled = true\nurl = \"https://memory.example.com/mcp\"\n\n[mcp_servers.noema.http_headers]\nAuthorization = \"Bearer fixture-secret\"\n";
    fs::write(&config_path, original).unwrap();
    let install = noema(&home, &config)
        .args([
            "--cortex",
            "shared",
            "integrate",
            "codex",
            "install",
            "--scope",
            "user",
            "--continuity",
        ])
        .output()
        .unwrap();
    assert!(
        install.status.success(),
        "{}",
        String::from_utf8_lossy(&install.stderr)
    );
    assert_eq!(fs::read(&config_path).unwrap(), original);
    let stdout = String::from_utf8_lossy(&install.stdout);
    assert!(stdout.contains("base mcp preserved (http)"));
    assert!(stdout.contains("continuity profile"));
    let profile = fs::read_to_string(home.join(".codex/noema-continuity.config.toml")).unwrap();
    assert!(profile.contains("noema-managed-codex-continuity-v1"));
    assert!(profile.contains("enabled = false"));

    let status = noema(&home, &config)
        .args(["integrate", "codex", "status", "--scope", "user", "--check"])
        .output()
        .unwrap();
    assert!(
        status.status.success(),
        "{}",
        String::from_utf8_lossy(&status.stderr)
    );
    assert!(String::from_utf8_lossy(&status.stdout).contains("base mcp preserved (http)"));

    let remove = noema(&home, &config)
        .args(["integrate", "codex", "remove", "--scope", "user"])
        .output()
        .unwrap();
    assert!(
        remove.status.success(),
        "{}",
        String::from_utf8_lossy(&remove.stderr)
    );
    assert_eq!(fs::read(&config_path).unwrap(), original);
    assert!(!home.join(".codex/noema-continuity.config.toml").exists());
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
fn cursor_user_integration_cli_supports_check_install_status_and_remove() {
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
    assert!(init.status.success());

    let preview = noema(&home, &config)
        .args([
            "--cortex",
            "shared",
            "integrate",
            "cursor",
            "install",
            "--scope",
            "user",
            "--check",
        ])
        .output()
        .unwrap();
    assert!(!preview.status.success());
    assert!(String::from_utf8_lossy(&preview.stdout).contains("would install"));
    assert!(!home.join(".cursor/mcp.json").exists());
    assert!(!home.join(".cursor/hooks.json").exists());

    let install = noema(&home, &config)
        .args([
            "--cortex",
            "shared",
            "integrate",
            "cursor",
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
    let mcp: serde_json::Value =
        serde_json::from_str(&fs::read_to_string(home.join(".cursor/mcp.json")).unwrap()).unwrap();
    let hooks: serde_json::Value =
        serde_json::from_str(&fs::read_to_string(home.join(".cursor/hooks.json")).unwrap())
            .unwrap();
    assert!(mcp["mcpServers"]["noema"].is_object());
    assert_eq!(hooks["version"], 1);
    assert_eq!(
        hooks["hooks"]["sessionStart"][0]["command"],
        "./hooks/noema-bootstrap.sh"
    );
    let script = home.join(".cursor/hooks/noema-bootstrap.sh");
    let script_body = fs::read_to_string(&script).unwrap();
    assert!(script_body.contains("additional_context"));
    assert!(script_body.contains("noema-managed-cursor-bootstrap-v2"));
    assert!(script_body.contains("user-preference"));
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        assert_ne!(
            fs::metadata(&script).unwrap().permissions().mode() & 0o100,
            0
        );
    }

    let status = noema(&home, &config)
        .args([
            "integrate",
            "cursor",
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
    assert!(String::from_utf8_lossy(&status.stdout).contains("bootstrap script"));

    let remove = noema(&home, &config)
        .args(["integrate", "cursor", "remove", "--scope", "user"])
        .output()
        .unwrap();
    assert!(
        remove.status.success(),
        "{}",
        String::from_utf8_lossy(&remove.stderr)
    );
    assert!(!script.exists());
}

#[test]
fn cursor_project_integration_cli_generates_cloud_bootstrap_rule() {
    let temp = tempfile::tempdir().unwrap();
    let home = temp.path().join("home");
    let config = temp.path().join("config");
    let cortexes = temp.path().join("cortexes");
    let project = temp.path().join("project");
    fs::create_dir_all(&home).unwrap();
    fs::create_dir_all(&config).unwrap();
    fs::create_dir_all(&cortexes).unwrap();
    fs::create_dir_all(project.join(".git")).unwrap();

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
    assert!(init.status.success());

    let install = noema(&home, &config)
        .current_dir(&project)
        .args([
            "--cortex",
            "shared",
            "integrate",
            "cursor",
            "install",
            "--scope",
            "project",
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
    let mcp = fs::read_to_string(project.join(".cursor/mcp.json")).unwrap();
    assert!(mcp.contains("Bearer ${env:NOEMA_MCP_KEY}"));
    let rule = fs::read_to_string(project.join(".cursor/rules/noema.mdc")).unwrap();
    assert!(rule.contains("alwaysApply: true"));
    assert!(rule.contains("get_instructions"));
    assert!(!project.join(".cursor/hooks.json").exists());

    let status = noema(&home, &config)
        .current_dir(&project)
        .args([
            "integrate",
            "cursor",
            "status",
            "--scope",
            "project",
            "--check",
        ])
        .output()
        .unwrap();
    assert!(
        status.status.success(),
        "{}",
        String::from_utf8_lossy(&status.stderr)
    );
    assert!(String::from_utf8_lossy(&status.stdout).contains("bootstrap rule"));
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
    assert!(stdout.contains("cursor (user)"));
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
