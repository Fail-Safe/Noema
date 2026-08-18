#!/usr/bin/env python3
"""Compare Go and Rust TLS certificate lifecycle behavior."""

from __future__ import annotations

import argparse
from datetime import datetime, timedelta, timezone
from pathlib import Path
import shutil
import subprocess
import tempfile

from federation_ring import Node, environment, free_port, stop, wait_for_port
from federation_tls import generate_certificate, initialize_node, insert_access


def serve_arguments(node: Node, *extra: str) -> list[str]:
    arguments = [
        str(node.binary),
        "--cortex",
        node.name,
        "serve",
        "--transport",
        "http",
        "--host",
        "127.0.0.1",
        "--port",
        str(node.port),
        *extra,
    ]
    if node.rust:
        arguments.append("--no-watch")
    return arguments


def configure_node(
    node: Node,
    env: dict[str, str],
    parent: Path,
    access_key: Path,
    certificate: Path,
    private_key: Path,
) -> None:
    initialize_node(node, env, parent)
    insert_access(
        parent / node.name / "cortex.md",
        access_key,
        certificate,
        private_key,
    )


def refused(
    node: Node,
    env: dict[str, str],
    secret: str,
    *extra: str,
) -> str:
    result = subprocess.run(
        serve_arguments(node, *extra),
        env=env,
        check=False,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=10,
    )
    assert result.returncode != 0, f"{node.name} unexpectedly started"
    assert secret not in result.stderr
    assert "PRIVATE KEY" not in result.stderr
    return result.stderr


def starts(
    node: Node,
    env: dict[str, str],
    log_path: Path,
    secret: str,
    *extra: str,
) -> str:
    with log_path.open("w") as log:
        node.process = subprocess.Popen(
            serve_arguments(node, *extra),
            env=env,
            text=True,
            stdout=subprocess.DEVNULL,
            stderr=log,
        )
    wait_for_port(node)
    assert stop(node), f"{node.name} did not stop gracefully"
    log = log_path.read_text()
    assert secret not in log
    assert "PRIVATE KEY" not in log
    return log


def exercise(go: Path, rust: Path, root: Path) -> None:
    env = environment(root)
    parent = root / "cortexes"
    secret = "lifecycle-test-access-key"
    access_key = root / "access.key"
    access_key.write_text(f"{secret}\n")
    access_key.chmod(0o600)

    now = datetime.now(timezone.utc)
    fixtures: dict[str, tuple[Path, Path, Path]] = {
        "expired": generate_certificate(
            root / "expired",
            not_before=now - timedelta(days=30),
            not_after=now - timedelta(days=2),
        ),
        "future": generate_certificate(
            root / "future",
            not_before=now + timedelta(days=2),
            not_after=now + timedelta(days=30),
        ),
        "near": generate_certificate(
            root / "near",
            not_before=now - timedelta(days=1),
            not_after=now + timedelta(days=6),
        ),
        "fresh": generate_certificate(
            root / "fresh",
            not_before=now - timedelta(days=1),
            not_after=now + timedelta(days=365),
        ),
    }

    for implementation, binary, is_rust in [
        ("go", go, False),
        ("rust", rust, True),
    ]:
        for case in ["expired", "future"]:
            _, certificate, private_key = fixtures[case]
            node = Node(f"{implementation}-{case}", binary, is_rust, free_port())
            configure_node(node, env, parent, access_key, certificate, private_key)
            error = refused(node, env, secret)
            expected = "expired" if case == "expired" else "NotBefore"
            assert expected in error, f"{implementation} omitted {case} diagnosis"
            assert "refusing to start" in error

        _, near_certificate, near_private_key = fixtures["near"]
        near = Node(f"{implementation}-near", binary, is_rust, free_port())
        configure_node(
            near, env, parent, access_key, near_certificate, near_private_key
        )
        near_log = starts(
            near,
            env,
            root / f"{near.name}.log",
            secret,
        )
        assert "rotate soon" in near_log
        assert "[cert-monitor] ≤7d:" in near_log

        _, fresh_certificate, fresh_private_key = fixtures["fresh"]
        override = Node(f"{implementation}-override", binary, is_rust, free_port())
        configure_node(
            override,
            env,
            parent,
            access_key,
            fresh_certificate,
            fresh_private_key,
        )
        override_error = refused(
            override,
            env,
            secret,
            "--tls-cert",
            str(fixtures["expired"][1]),
            "--tls-key",
            str(fixtures["expired"][2]),
        )
        assert "expired" in override_error

        override_log = starts(
            override,
            env,
            root / f"{override.name}.log",
            secret,
            "--tls-cert",
            str(fixtures["expired"][1]),
            "--tls-key",
            str(fixtures["expired"][2]),
            "--insecure-allow-expired",
        )
        assert "WARN --insecure-allow-expired" in override_log
        assert "[cert-monitor] expired:" in override_log

        malformed = root / f"{implementation}-malformed.crt"
        marker = f"{implementation}-certificate-private-marker"
        malformed.write_text(f"not a certificate\n{marker}\n")
        malformed_node = Node(
            f"{implementation}-malformed", binary, is_rust, free_port()
        )
        configure_node(
            malformed_node,
            env,
            parent,
            access_key,
            malformed,
            fresh_private_key,
        )
        malformed_error = refused(malformed_node, env, secret)
        assert "cannot read TLS certificate" in malformed_error
        assert marker not in malformed_error


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    if shutil.which("openssl") is None:
        parser.error("openssl is required to generate temporary TLS fixtures")
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")
    with tempfile.TemporaryDirectory(prefix="noema-rust-cert-lifecycle-") as directory:
        exercise(args.go.resolve(), args.rust.resolve(), Path(directory))
    print(
        "TLS certificate lifecycle: expiry, activation, warning, override, monitor, redaction PASS"
    )


if __name__ == "__main__":
    main()
