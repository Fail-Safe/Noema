#!/usr/bin/env python3
"""Exercise authenticated TLS federation across the Go and Rust builds."""

from __future__ import annotations

import argparse
import http.client
import json
import os
from pathlib import Path
import shutil
import ssl
import subprocess
import tempfile

from federation_ring import (
    Node,
    add_trace,
    configure_policy,
    environment,
    free_port,
    has_trace,
    run,
    start,
    stop,
    wait_until,
)


def generate_certificate(root: Path) -> tuple[Path, Path, Path]:
    ca_certificate = root / "ca.crt"
    ca_private_key = root / "ca.key"
    certificate = root / "server.crt"
    private_key = root / "server.key"
    request = root / "server.csr"
    extensions = root / "server.ext"
    extensions.write_text(
        "subjectAltName=DNS:localhost,IP:127.0.0.1\n"
        "basicConstraints=critical,CA:FALSE\n"
        "keyUsage=critical,digitalSignature,keyEncipherment\n"
        "extendedKeyUsage=serverAuth\n"
    )
    commands = [
        [
            "openssl",
            "req",
            "-x509",
            "-newkey",
            "rsa:2048",
            "-sha256",
            "-days",
            "1",
            "-nodes",
            "-keyout",
            str(ca_private_key),
            "-out",
            str(ca_certificate),
            "-subj",
            "/CN=Noema Test CA",
            "-addext",
            "basicConstraints=critical,CA:TRUE",
            "-addext",
            "keyUsage=critical,keyCertSign,cRLSign",
        ],
        [
            "openssl",
            "req",
            "-newkey",
            "rsa:2048",
            "-sha256",
            "-nodes",
            "-keyout",
            str(private_key),
            "-out",
            str(request),
            "-subj",
            "/CN=localhost",
        ],
        [
            "openssl",
            "x509",
            "-req",
            "-in",
            str(request),
            "-CA",
            str(ca_certificate),
            "-CAkey",
            str(ca_private_key),
            "-CAcreateserial",
            "-out",
            str(certificate),
            "-days",
            "1",
            "-sha256",
            "-extfile",
            str(extensions),
        ],
    ]
    for command in commands:
        result = subprocess.run(
            command,
            check=False,
            text=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
        )
        if result.returncode != 0:
            raise RuntimeError(f"could not generate TLS fixture: {result.stderr}")
    ca_private_key.chmod(0o600)
    private_key.chmod(0o600)
    return ca_certificate, certificate, private_key


def insert_access(
    manifest: Path,
    shared_key_file: Path,
    certificate: Path | None = None,
    private_key: Path | None = None,
) -> None:
    lines = manifest.read_text().splitlines()
    boundaries = [index for index, line in enumerate(lines) if line == "---"]
    if len(boundaries) < 2:
        raise RuntimeError(f"manifest has no closing frontmatter boundary: {manifest}")
    block = ["access:", f"  shared_key_file: {shared_key_file}"]
    if certificate is not None and private_key is not None:
        block.extend(
            [
                f"  tls_cert_path: {certificate}",
                f"  tls_key_path: {private_key}",
            ]
        )
    lines[boundaries[1]:boundaries[1]] = block
    manifest.write_text("\n".join(lines) + "\n")


def set_peer_ca(manifest: Path, peer_name: str, certificate: Path) -> None:
    lines = manifest.read_text().splitlines()
    in_peer = False
    peer_indent = -1
    for index, line in enumerate(lines):
        stripped = line.strip()
        if stripped == f"- name: {peer_name}":
            in_peer = True
            peer_indent = len(line) - len(line.lstrip())
            continue
        indentation_width = len(line) - len(line.lstrip())
        if in_peer and stripped and (
            stripped.startswith("- name:") or indentation_width <= peer_indent
        ):
            lines.insert(index, f"{' ' * (peer_indent + 2)}ca: {certificate}")
            manifest.write_text("\n".join(lines) + "\n")
            return
        if in_peer and stripped.startswith("ca:"):
            indentation = line[: len(line) - len(line.lstrip())]
            lines[index] = f"{indentation}ca: {certificate}"
            manifest.write_text("\n".join(lines) + "\n")
            return
    if in_peer:
        lines.append(f"{' ' * (peer_indent + 2)}ca: {certificate}")
        manifest.write_text("\n".join(lines) + "\n")
        return
    raise RuntimeError(f"could not find CA field for {peer_name} in {manifest}")


def initialize_node(node: Node, env: dict[str, str], parent: Path) -> None:
    subprocess.run(
        [str(node.binary), "init", "--name", node.name, "--path", str(parent)],
        env=env,
        check=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    run(node, env, "keygen")


def initialize_status(port: int, certificate: Path, authorization: str | None) -> int:
    context = ssl.create_default_context(cafile=str(certificate))
    connection = http.client.HTTPSConnection(
        "127.0.0.1", port, timeout=3, context=context
    )
    headers = {
        "Content-Type": "application/json",
        "Accept": "application/json, text/event-stream",
    }
    if authorization is not None:
        headers["Authorization"] = authorization
    connection.request(
        "POST",
        "/mcp",
        body=json.dumps(
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "initialize",
                "params": {
                    "protocolVersion": "2025-03-26",
                    "capabilities": {},
                    "clientInfo": {"name": "tls-test", "version": "1"},
                },
            }
        ),
        headers=headers,
    )
    response = connection.getresponse()
    response.read()
    connection.close()
    return response.status


def exercise(go: Path, rust: Path, root: Path) -> None:
    env = environment(root)
    parent = root / "cortexes"
    ca_certificate, certificate, private_key = generate_certificate(root)
    access_key = root / "access.key"
    access_key.write_text("test-shared-access-key\n")
    access_key.chmod(0o600)
    wrong_key = root / "wrong-access.key"
    wrong_key.write_text("wrong-test-access-key\n")
    wrong_key.chmod(0o600)

    peer_a = Node("peer-a", go, False, free_port())
    peer_b = Node("peer-b", rust, True, free_port())
    nodes = [peer_a, peer_b]
    for node in nodes:
        initialize_node(node, env, parent)
    for node, peer in [(peer_a, peer_b), (peer_b, peer_a)]:
        run(
            node,
            env,
            "federation",
            "add-peer",
            peer.name,
            f"https://127.0.0.1:{peer.port}",
        )
        manifest = parent / node.name / "cortex.md"
        configure_policy(manifest, node.rust)
        set_peer_ca(manifest, peer.name, ca_certificate)
        insert_access(manifest, access_key, certificate, private_key)

    go_trace = add_trace(peer_a, env, "tls-go", "authenticated Go TLS payload")
    rust_trace = add_trace(peer_b, env, "tls-rust", "authenticated Rust TLS payload")

    try:
        for node in nodes:
            start(node, env)
        wait_until(
            "Go-to-Rust authenticated TLS convergence",
            lambda: has_trace(peer_b, env, go_trace, "authenticated Go TLS payload"),
        )
        wait_until(
            "Rust-to-Go authenticated TLS convergence",
            lambda: has_trace(peer_a, env, rust_trace, "authenticated Rust TLS payload"),
        )

        for node in nodes:
            assert initialize_status(node.port, ca_certificate, None) == 401
            assert (
                initialize_status(node.port, ca_certificate, "Bearer incorrect-test-key")
                == 401
            )
            assert (
                initialize_status(
                    node.port, ca_certificate, "Bearer test-shared-access-key"
                )
                == 200
            )

        client = Node("peer-client", rust, True, free_port())
        initialize_node(client, env, parent)
        run(
            client,
            env,
            "federation",
            "add-peer",
            peer_a.name,
            f"https://127.0.0.1:{peer_a.port}",
        )
        client_manifest = parent / client.name / "cortex.md"
        configure_policy(client_manifest, True)
        insert_access(client_manifest, wrong_key)
        no_ca = run(
            client,
            env,
            "federation",
            "sync",
            peer_a.name,
            check=False,
        )
        assert no_ca.returncode != 0
        assert "connecting to peer" in no_ca.stderr.lower()
        assert "test-shared-access-key" not in no_ca.stderr

        set_peer_ca(client_manifest, peer_a.name, ca_certificate)
        wrong_auth = run(
            client,
            env,
            "federation",
            "sync",
            peer_a.name,
            check=False,
        )
        assert wrong_auth.returncode != 0
        assert "401" in wrong_auth.stderr or "unauthorized" in wrong_auth.stderr.lower()
        assert "wrong-test-access-key" not in wrong_auth.stderr

        wrong_key.write_text("test-shared-access-key\n")
        recovered = run(
            client,
            env,
            "federation",
            "sync",
            peer_a.name,
        )
        assert "event(s)" in recovered.stdout

        plaintext = Node("peer-plaintext", rust, True, free_port())
        initialize_node(plaintext, env, parent)
        insert_access(parent / plaintext.name / "cortex.md", access_key)
        refused = run(
            plaintext,
            env,
            "serve",
            "--transport",
            "http",
            "--host",
            "127.0.0.1",
            "--port",
            str(plaintext.port),
            "--no-watch",
            check=False,
        )
        assert refused.returncode != 0
        assert "plaintext HTTP" in refused.stderr
    finally:
        if not all(stop(node) for node in nodes):
            raise RuntimeError("one or more TLS servers did not terminate gracefully")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    if shutil.which("openssl") is None:
        parser.error("openssl is required to generate the temporary TLS fixture")
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")
    with tempfile.TemporaryDirectory(prefix="noema-rust-tls-") as directory:
        exercise(args.go.resolve(), args.rust.resolve(), Path(directory))
    print("authenticated TLS federation: auth, CA trust, plaintext refusal PASS")


if __name__ == "__main__":
    main()
