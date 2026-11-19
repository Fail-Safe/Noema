use std::{
    fs,
    io::Write,
    net::TcpListener,
    path::Path,
    process::{Child, Command, Stdio},
    thread,
    time::{Duration, Instant},
};

fn init(config: &Path, root: &Path) {
    let output = Command::new(env!("CARGO_BIN_EXE_noema"))
        .env("XDG_CONFIG_HOME", config)
        .args(["init", "--name", "shared", "--path"])
        .arg(root)
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
}

struct Server {
    child: Child,
    _stdin: Option<std::process::ChildStdin>,
    log: std::path::PathBuf,
}

impl Server {
    fn start(
        config: &Path,
        root: &Path,
        runtime: &Path,
        log: &Path,
        http_port: Option<u16>,
    ) -> Self {
        let mut command = Command::new(env!("CARGO_BIN_EXE_noema"));
        command
            .env("XDG_CONFIG_HOME", config)
            .env("XDG_RUNTIME_DIR", runtime)
            .env("XDG_STATE_HOME", root.join("state"))
            .args(["--cortex", "shared", "serve", "--no-watch", "--log-file"])
            .arg(log);
        let stdin = if let Some(port) = http_port {
            command.args([
                "--transport",
                "http",
                "--host",
                "127.0.0.1",
                "--port",
                &port.to_string(),
            ]);
            None
        } else {
            command.args(["--transport", "stdio"]);
            Some(Stdio::piped())
        };
        command.stdin(stdin.unwrap_or(Stdio::null()));
        command.stdout(Stdio::null()).stderr(Stdio::null());
        let mut child = command.spawn().unwrap();
        let stdin = child.stdin.take().map(|mut pipe| {
            let _ = pipe.write_all(b"");
            pipe
        });
        Self {
            child,
            _stdin: stdin,
            log: log.to_path_buf(),
        }
    }

    fn log(&self) -> String {
        fs::read_to_string(&self.log).unwrap_or_default()
    }
}

fn wait_until(label: &str, mut ready: impl FnMut() -> bool) {
    let deadline = Instant::now() + Duration::from_secs(20);
    while Instant::now() < deadline {
        if ready() {
            return;
        }
        thread::sleep(Duration::from_millis(50));
    }
    panic!("{label} timed out");
}

#[test]
fn one_background_owner_per_cortex_across_runtime_dirs_and_after_kill() {
    let temp = tempfile::tempdir().unwrap();
    let config = temp.path().join("config");
    let root = temp.path().join("cortexes");
    init(&config, &root);
    let cortex = root.join("shared");
    let lock = cortex.join(".noema-background.lock");

    let http_port = TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port();
    let mut http = Server::start(
        &config,
        &root,
        &temp.path().join("runtime-http"),
        &temp.path().join("http.log"),
        Some(http_port),
    );
    wait_until("HTTP server owns the cortex lock", || {
        fs::read_to_string(&lock)
            .ok()
            .is_some_and(|record| record == format!("pid {}\n", http.child.id()))
            && http.log().contains("Noema MCP listening")
    });
    assert!(
        !http
            .log()
            .contains("another process owns cortex background work"),
        "{}",
        http.log()
    );

    let mut stdio = Server::start(
        &config,
        &root,
        &temp.path().join("runtime-stdio"),
        &temp.path().join("stdio.log"),
        None,
    );
    wait_until("stdio client serves MCP only", || {
        stdio
            .log()
            .contains("another process owns cortex background work")
    });
    assert!(stdio.child.try_wait().unwrap().is_none());
    assert_eq!(
        fs::read_to_string(&lock).unwrap(),
        format!("pid {}\n", http.child.id())
    );

    http.child.kill().unwrap();
    http.child.wait().unwrap();

    let replacement = Server::start(
        &config,
        &root,
        &temp.path().join("runtime-replacement"),
        &temp.path().join("replacement.log"),
        None,
    );
    wait_until(
        "next process acquires the lock after the owner is killed",
        || {
            fs::read_to_string(&lock)
                .ok()
                .is_some_and(|record| record == format!("pid {}\n", replacement.child.id()))
        },
    );
    thread::sleep(Duration::from_millis(200));
    assert!(
        !replacement
            .log()
            .contains("another process owns cortex background work"),
        "{}",
        replacement.log()
    );
    assert!(
        stdio
            .log()
            .contains("another process owns cortex background work"),
        "an already-running loser does not take over"
    );

    let later = Server::start(
        &config,
        &root,
        &temp.path().join("runtime-later"),
        &temp.path().join("later.log"),
        None,
    );
    wait_until("later client loses to the replacement owner", || {
        later
            .log()
            .contains("another process owns cortex background work")
    });

    for mut server in [stdio, replacement, later] {
        let _ = server.child.kill();
        let _ = server.child.wait();
    }
}
