use noema::{
    cortex::{Cortex, SearchConfig, write_manifest},
    embedding::HttpEmbedder,
    trace::Trace,
};
use std::{
    io::Read,
    net::TcpListener,
    path::Path,
    process::{Command, Output},
    thread,
    time::{Duration, Instant},
};

fn endpoint(listener: &TcpListener) -> String {
    format!(
        "https://synthetic-user:synthetic-password@{}/v1?token=synthetic-query#synthetic-fragment",
        listener.local_addr().unwrap()
    )
}

fn refuse_tls(listener: TcpListener) -> thread::JoinHandle<()> {
    listener.set_nonblocking(true).unwrap();
    thread::spawn(move || {
        let deadline = Instant::now() + Duration::from_secs(10);
        loop {
            match listener.accept() {
                Ok((mut stream, _)) => {
                    stream.set_nonblocking(false).unwrap();
                    stream
                        .set_read_timeout(Some(Duration::from_secs(10)))
                        .unwrap();
                    let mut first = [0];
                    stream.read_exact(&mut first).unwrap();
                    assert_eq!(first[0], 22, "expected a TLS handshake, not plaintext HTTP");
                    return;
                }
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                    assert!(Instant::now() < deadline, "TLS request did not connect");
                    thread::sleep(Duration::from_millis(10));
                }
                Err(error) => panic!("accept failed: {error}"),
            }
        }
    })
}

fn assert_no_url_secrets(output: &str) {
    for secret in [
        "synthetic-user",
        "synthetic-password",
        "synthetic-query",
        "synthetic-fragment",
    ] {
        assert!(
            !output.contains(secret),
            "credential-bearing endpoint leaked in diagnostics"
        );
    }
}

fn cli(root: &Path, args: &[&str]) -> Output {
    Command::new(env!("CARGO_BIN_EXE_noema"))
        .env("HOME", root.join("home"))
        .env("XDG_CONFIG_HOME", root.join("config"))
        .args(args)
        .output()
        .unwrap()
}

#[test]
fn embedding_cli_diagnostics_do_not_expose_endpoint_credentials() {
    let temp = tempfile::tempdir().unwrap();
    let root = temp.path();
    assert!(
        cli(
            root,
            &[
                "init",
                "--name",
                "diagnostic-smoke",
                "--path",
                root.to_str().unwrap()
            ]
        )
        .status
        .success()
    );
    let directory = root.join("diagnostic-smoke");
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let mut cortex = Cortex::open("diagnostic-smoke", &directory).unwrap();
    cortex.manifest.search = Some(SearchConfig {
        semantic_enabled: true,
        embedding_endpoint: endpoint(&listener),
        embedding_model: "fixture-model".into(),
        ..Default::default()
    });
    write_manifest(&directory, &cortex.manifest).unwrap();
    let mut trace = Trace::new("Diagnostic fixture", "note", "", vec![], "Synthetic input");
    cortex.add(&mut trace).unwrap();
    drop(cortex);
    let status = cli(
        root,
        &["--cortex", "diagnostic-smoke", "embeddings", "status"],
    );
    assert!(status.status.success());
    assert_no_url_secrets(&String::from_utf8_lossy(&status.stdout));
    assert_no_url_secrets(&String::from_utf8_lossy(&status.stderr));
    let server = refuse_tls(listener);
    let backfill = cli(
        root,
        &["--cortex", "diagnostic-smoke", "embeddings", "backfill"],
    );
    server.join().unwrap();
    assert!(
        !backfill.status.success(),
        "provider TLS failure must remain visible"
    );
    assert_no_url_secrets(&String::from_utf8_lossy(&backfill.stdout));
    assert_no_url_secrets(&String::from_utf8_lossy(&backfill.stderr));
}

#[tokio::test]
async fn embedding_request_errors_omit_credential_bearing_urls() {
    for tokenizer in [false, true] {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let client = HttpEmbedder::new(&endpoint(&listener), "").unwrap();
        let server = refuse_tls(listener);
        let error = if tokenizer {
            client.token_count("Synthetic input", "").await.map(|_| ())
        } else {
            client
                .embed("fixture-model", &["Synthetic input".into()])
                .await
                .map(|_| ())
        }
        .unwrap_err();
        server.join().unwrap();
        assert_no_url_secrets(&format!("{error:#}"));
    }
}
