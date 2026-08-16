#!/usr/bin/env python3
"""Compare Go/Rust cortex backup and restore behavior."""

from __future__ import annotations

import argparse
import io
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile


def environment(home: Path) -> dict[str, str]:
    env = os.environ.copy()
    env["HOME"] = str(home)
    env.pop("XDG_CONFIG_HOME", None)
    return env


def run(
    binary: Path,
    env: dict[str, str],
    *arguments: str,
    check: bool = True,
) -> subprocess.CompletedProcess[str]:
    completed = subprocess.run(
        [str(binary), *arguments],
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    if check and completed.returncode != 0:
        raise AssertionError(
            f"command failed ({completed.returncode}): "
            f"{completed.stderr.strip()}\n{completed.stdout.strip()}"
        )
    return completed


def add_trace(binary: Path, env: dict[str, str], body: str) -> str:
    completed = run(
        binary,
        env,
        "--cortex",
        "source",
        "add",
        "--title",
        "Restore parity",
        "--type",
        "fact",
        "--body",
        body,
    )
    return completed.stdout.strip().rsplit(": ", 1)[-1]


def cross_restore(
    producer: Path,
    consumer: Path,
    root: Path,
    label: str,
) -> None:
    source_home = root / f"{label}-source-home"
    source_parent = root / f"{label}-source-cortexes"
    restore_home = root / f"{label}-restore-home"
    restore_parent = root / f"{label}-restored-cortexes"
    for directory in (source_home, source_parent, restore_home, restore_parent):
        directory.mkdir()
    source_env = environment(source_home)
    restore_env = environment(restore_home)
    archive = root / f"{label}.tar.gz"
    body = f"body preserved through {label}"

    run(producer, source_env, "init", "--name", "source", "--path", str(source_parent))
    trace_id = add_trace(producer, source_env, body)
    run(
        producer,
        source_env,
        "cortex",
        "backup",
        "source",
        "--output",
        str(archive),
    )
    run(
        consumer,
        restore_env,
        "cortex",
        "restore",
        str(archive),
        "--path",
        str(restore_parent),
    )
    restored = run(consumer, restore_env, "--cortex", "source", "get", trace_id)
    if body not in restored.stdout:
        raise AssertionError(f"{label}: restored trace body did not match")

    collision = run(
        consumer,
        restore_env,
        "cortex",
        "restore",
        str(archive),
        "--name",
        "clone",
        "--path",
        str(restore_parent),
        check=False,
    )
    if collision.returncode == 0 or "ID" not in collision.stderr:
        raise AssertionError(f"{label}: duplicate identity restore was not rejected")


def force_restore(rust: Path, root: Path) -> None:
    source_home = root / "force-source-home"
    restore_home = root / "force-restore-home"
    source_parent = root / "force-source-cortexes"
    restore_parent = root / "force-restored-cortexes"
    for directory in (source_home, restore_home, source_parent, restore_parent):
        directory.mkdir()
    source_env = environment(source_home)
    restore_env = environment(restore_home)
    archive = root / "force.tar.gz"

    run(rust, source_env, "init", "--name", "source", "--path", str(source_parent))
    trace_id = add_trace(rust, source_env, "force replacement body")
    run(
        rust,
        source_env,
        "cortex",
        "backup",
        "source",
        "--output",
        str(archive),
    )
    destination = restore_parent / "source"
    destination.mkdir()
    sentinel = destination / "operator-data"
    sentinel.write_text("preserve until commit\n")

    refused = run(
        rust,
        restore_env,
        "cortex",
        "restore",
        str(archive),
        "--path",
        str(restore_parent),
        check=False,
    )
    if refused.returncode == 0 or sentinel.read_text() != "preserve until commit\n":
        raise AssertionError("Rust restore did not preserve an existing destination on refusal")

    run(
        rust,
        restore_env,
        "cortex",
        "restore",
        str(archive),
        "--path",
        str(restore_parent),
        "--force",
    )
    if sentinel.exists():
        raise AssertionError("Rust forced restore retained replaced destination content")
    restored = run(rust, restore_env, "--cortex", "source", "get", trace_id)
    if "force replacement body" not in restored.stdout:
        raise AssertionError("Rust forced restore did not place the archived cortex")
    leftovers = list(restore_parent.glob(".noema-restore-*"))
    if leftovers:
        raise AssertionError(f"Rust forced restore left transaction artifacts: {leftovers}")


def unsafe_archives_fail_closed(rust: Path, root: Path) -> None:
    restore_home = root / "unsafe-home"
    restore_parent = root / "unsafe-destination"
    restore_home.mkdir()
    restore_parent.mkdir()
    env = environment(restore_home)

    traversal = root / "traversal.tar.gz"
    with tarfile.open(traversal, "w:gz") as archive:
        entry = tarfile.TarInfo("../escaped")
        payload = b"outside"
        entry.size = len(payload)
        archive.addfile(entry, io.BytesIO(payload))
    result = run(
        rust,
        env,
        "cortex",
        "restore",
        str(traversal),
        "--path",
        str(restore_parent),
        check=False,
    )
    if result.returncode == 0 or (root / "escaped").exists():
        raise AssertionError("Rust restore accepted archive path traversal")

    symlink = root / "symlink.tar.gz"
    with tarfile.open(symlink, "w:gz") as archive:
        directory = tarfile.TarInfo("source")
        directory.type = tarfile.DIRTYPE
        archive.addfile(directory)
        entry = tarfile.TarInfo("source/link")
        entry.type = tarfile.SYMTYPE
        entry.linkname = "../../outside"
        archive.addfile(entry)
    result = run(
        rust,
        env,
        "cortex",
        "restore",
        str(symlink),
        "--path",
        str(restore_parent),
        check=False,
    )
    if result.returncode == 0 or (root / "outside").exists():
        raise AssertionError("Rust restore accepted an archive symlink")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="noema-restore-parity-") as directory:
        root = Path(directory)
        cross_restore(args.go, args.rust, root, "go-to-rust")
        cross_restore(args.rust, args.go, root, "rust-to-go")
        force_restore(args.rust, root)
        unsafe_archives_fail_closed(args.rust, root)
    print(
        "Go/Rust restore: cross-format, identity guards, force transaction, "
        "traversal, and link rejection PASS"
    )


if __name__ == "__main__":
    main()
