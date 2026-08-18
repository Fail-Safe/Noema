#!/usr/bin/env python3
"""Exercise Rust durability boundaries on disposable APFS disk images."""

from __future__ import annotations

import argparse
from contextlib import closing
import os
from pathlib import Path
import plistlib
import re
import sqlite3
import subprocess
import tempfile
import time


PENDING_LOCK = re.compile(r"^[0-9A-HJKMNP-TV-Z]{26}\.lock$")


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


def environment(root: Path, durability: str | None = None) -> dict[str, str]:
    env = os.environ.copy()
    env["HOME"] = str(root / "home")
    env["XDG_CONFIG_HOME"] = str(root / "config")
    for key in tuple(env):
        if key.startswith("NOEMA_RUST_TEST_"):
            env.pop(key)
    if durability is None:
        env.pop("NOEMA_DURABILITY", None)
    else:
        env["NOEMA_DURABILITY"] = durability
    return env


class DiskImage:
    def __init__(self, root: Path, label: str) -> None:
        self.root = root
        self.image = root / f"{label}.sparseimage"
        self.mount = root / "mount"
        self.device: str | None = None

    def create(self) -> None:
        self.mount.mkdir()
        subprocess.run(
            [
                "hdiutil",
                "create",
                "-size",
                "128m",
                "-fs",
                "APFS",
                "-volname",
                "NOEMA_STORAGE_FAULT",
                "-type",
                "SPARSE",
                str(self.image),
            ],
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        self.attach()

    def attach(self) -> None:
        if self.device is not None:
            raise RuntimeError("disk image is already attached")
        completed = subprocess.run(
            [
                "hdiutil",
                "attach",
                "-plist",
                "-nobrowse",
                "-mountpoint",
                str(self.mount),
                str(self.image),
            ],
            check=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
        )
        document = plistlib.loads(completed.stdout)
        entities = document.get("system-entities", [])
        devices = [entity.get("dev-entry") for entity in entities]
        self.device = next((device for device in devices if device), None)
        mounted = any(entity.get("mount-point") == str(self.mount) for entity in entities)
        if self.device is None or not mounted:
            raise RuntimeError("hdiutil did not report the expected mounted image")

    def detach(self, force: bool = False) -> None:
        if self.device is None:
            return
        arguments = ["hdiutil", "detach"]
        if force:
            arguments.append("-force")
        arguments.append(self.device)
        subprocess.run(
            arguments,
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        self.device = None

    def close(self) -> None:
        if self.device is not None:
            self.detach(force=True)


def wait_for_marker(child: subprocess.Popen[str], marker: Path) -> None:
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        if marker.exists():
            return
        status = child.poll()
        if status is not None:
            stdout, stderr = child.communicate()
            raise AssertionError(
                f"fault-injected command exited before pause ({status}): "
                f"{stderr.strip()}\n{stdout.strip()}"
            )
        time.sleep(0.05)
    child.kill()
    child.wait()
    raise TimeoutError("timed out waiting for the storage-fault marker")


def cut_at_pause(
    image: DiskImage,
    binary: Path,
    env: dict[str, str],
    marker: Path,
    pause_env: dict[str, str],
    *arguments: str,
) -> None:
    child_env = env | pause_env
    child = subprocess.Popen(
        [str(binary), *arguments],
        env=child_env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    wait_for_marker(child, marker)
    image.detach(force=True)
    child.kill()
    child.communicate(timeout=5)
    image.attach()


def add_trace(binary: Path, env: dict[str, str], cortex: str, body: str) -> str:
    completed = run(
        binary,
        env,
        "--cortex",
        cortex,
        "add",
        "--title",
        "Storage fault fixture",
        "--type",
        "fact",
        "--body",
        body,
    )
    return completed.stdout.strip().rsplit(": ", 1)[-1]


def assert_integrity(database: Path) -> None:
    with closing(sqlite3.connect(database)) as connection:
        assert connection.execute("PRAGMA integrity_check").fetchone() == ("ok",)


def transaction_id(config_home: Path) -> str:
    records = sorted((config_home / "noema/restore-transactions").glob("*.json"))
    if len(records) != 1:
        raise AssertionError(f"expected one restore journal, found {len(records)}")
    return records[0].stem


def build_restore_fixture(binary: Path, root: Path, host: Path) -> tuple[
    dict[str, str], Path, Path, str
]:
    source_env = environment(root / "source-state", "strong")
    restore_env = environment(root / "restore-state", "strong")
    source_parent = root / "source-cortexes"
    restore_parent = root / "restored-cortexes"
    source_parent.mkdir(parents=True)
    restore_parent.mkdir(parents=True)
    archive = host / "source.tar.gz"

    run(binary, source_env, "init", "--name", "source", "--path", str(source_parent))
    trace_id = add_trace(binary, source_env, "source", "restored body")
    run(
        binary,
        source_env,
        "cortex",
        "backup",
        "source",
        "--output",
        str(archive),
    )
    destination = restore_parent / "source"
    destination.mkdir()
    (destination / "operator-data").write_text("preserved old destination\n")
    return restore_env, archive, restore_parent, trace_id


def restore_placement_survives_cut(binary: Path) -> None:
    with tempfile.TemporaryDirectory(prefix="noema-storage-restore-placement-") as directory:
        host = Path(directory)
        image = DiskImage(host, "restore-placement")
        image.create()
        try:
            env, archive, parent, _ = build_restore_fixture(binary, image.mount, host)
            marker = host / "restore-placed.marker"
            cut_at_pause(
                image,
                binary,
                env,
                marker,
                {
                    "NOEMA_RUST_TEST_RESTORE_PAUSE_POINT": "restore-placed",
                    "NOEMA_RUST_TEST_RESTORE_PAUSE_MARKER": str(marker),
                },
                "cortex",
                "restore",
                str(archive),
                "--path",
                str(parent),
                "--force",
            )
            record = transaction_id(Path(env["XDG_CONFIG_HOME"]))
            status = run(binary, env, "cortex", "restore-status").stdout
            assert record in status and "state=resumable" in status
            run(
                binary,
                env,
                "cortex",
                "restore-recover",
                record,
                "--action",
                "rollback",
            )
            assert (parent / "source/operator-data").read_text() == (
                "preserved old destination\n"
            )
            assert run(binary, env, "cortex", "restore-status").stdout == (
                "Restore transactions: clean\n"
            )
        finally:
            image.close()


def config_rename_survives_cut(binary: Path) -> None:
    with tempfile.TemporaryDirectory(prefix="noema-storage-config-rename-") as directory:
        host = Path(directory)
        image = DiskImage(host, "config-rename")
        image.create()
        try:
            env, archive, parent, trace_id = build_restore_fixture(binary, image.mount, host)
            marker = host / "config-before-rename.marker"
            cut_at_pause(
                image,
                binary,
                env,
                marker,
                {
                    "NOEMA_RUST_TEST_CONFIG_PAUSE_POINT": "before-rename",
                    "NOEMA_RUST_TEST_CONFIG_PAUSE_MARKER": str(marker),
                },
                "cortex",
                "restore",
                str(archive),
                "--path",
                str(parent),
                "--force",
            )
            record = transaction_id(Path(env["XDG_CONFIG_HOME"]))
            run(
                binary,
                env,
                "cortex",
                "restore-recover",
                record,
                "--action",
                "resume",
            )
            restored = run(binary, env, "--cortex", "source", "get", trace_id)
            assert "restored body" in restored.stdout
            assert not Path(env["XDG_CONFIG_HOME"]).joinpath(
                "noema/.config.yaml.tmp"
            ).exists()
            assert_integrity(parent / "source/db/noema.db")
        finally:
            image.close()


def strong_mutation_recovers_after_cut(binary: Path) -> None:
    with tempfile.TemporaryDirectory(prefix="noema-storage-strong-mutation-") as directory:
        host = Path(directory)
        image = DiskImage(host, "strong-mutation")
        image.create()
        try:
            env = environment(image.mount / "state", "strong")
            parent = image.mount / "cortexes"
            parent.mkdir()
            run(binary, env, "init", "--name", "storage", "--path", str(parent))
            trace_id = add_trace(binary, env, "storage", "original body")
            marker = host / "archive-filesystem-complete.marker"
            cut_at_pause(
                image,
                binary,
                env,
                marker,
                {
                    "NOEMA_RUST_TEST_PAUSE_AFTER_FILESYSTEM_MUTATION": str(marker),
                },
                "--cortex",
                "storage",
                "archive",
                trace_id,
            )
            recovered = run(binary, env, "--cortex", "storage", "get", trace_id)
            assert "original body" in recovered.stdout
            cortex = parent / "storage"
            assert (cortex / "traces" / f"{trace_id}.md").is_file()
            assert not (cortex / "archive/traces" / f"{trace_id}.md").exists()
            database = cortex / "db/noema.db"
            with closing(sqlite3.connect(database)) as connection:
                archived, pending = connection.execute(
                    "SELECT archived_at, "
                    "(SELECT count(*) FROM federation_state "
                    "WHERE key GLOB 'rust_pending_mutation:*') "
                    "FROM traces WHERE id=?",
                    (trace_id,),
                ).fetchone()
            assert archived is None and pending == 0
            mutation_locks = [
                path
                for path in (cortex / "db/pending-mutations").glob("*.lock")
                if PENDING_LOCK.fullmatch(path.name)
            ]
            assert mutation_locks == []
            assert_integrity(database)
            run(binary, env, "--cortex", "storage", "verify")
        finally:
            image.close()


def standard_acknowledged_state_survives_cut(binary: Path) -> None:
    with tempfile.TemporaryDirectory(prefix="noema-storage-standard-") as directory:
        host = Path(directory)
        image = DiskImage(host, "standard")
        image.create()
        try:
            env = environment(image.mount / "state")
            parent = image.mount / "cortexes"
            parent.mkdir()
            run(binary, env, "init", "--name", "storage", "--path", str(parent))
            trace_id = add_trace(binary, env, "storage", "acknowledged body")
            run(binary, env, "--cortex", "storage", "archive", trace_id)
            image.detach(force=True)
            image.attach()
            archived = run(
                binary,
                env,
                "--cortex",
                "storage",
                "get",
                trace_id,
            )
            assert "acknowledged body" in archived.stdout
            cortex = parent / "storage"
            assert (cortex / "archive/traces" / f"{trace_id}.md").is_file()
            assert_integrity(cortex / "db/noema.db")
            run(binary, env, "--cortex", "storage", "verify")
        finally:
            image.close()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    binary = args.rust.resolve()
    if not binary.is_file():
        parser.error(f"binary does not exist: {binary}")
    if os.uname().sysname != "Darwin":
        parser.error("this harness requires macOS and hdiutil")

    restore_placement_survives_cut(binary)
    print("ok - forced detach preserves explicit restore rollback")
    config_rename_survives_cut(binary)
    print("ok - forced detach preserves resumable config replacement")
    strong_mutation_recovers_after_cut(binary)
    print("ok - strong archive recovers its journal and SQLite state")
    standard_acknowledged_state_survives_cut(binary)
    print("ok - standard acknowledged archive survives detach and remount")
    print("PASS: Rust macOS APFS storage-fault qualification")


if __name__ == "__main__":
    main()
