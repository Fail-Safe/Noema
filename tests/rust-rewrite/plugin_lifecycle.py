#!/usr/bin/env python3
"""Compare Go/Rust embedded plugin lifecycle and payload behavior."""

from __future__ import annotations

import argparse
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile


HERMES_FILES = ["__init__.py", "plugin.yaml", "transport.py"]
OBSIDIAN_FILES = ["main.js", "manifest.json", "styles.css"]


def invoke(
    binary: Path,
    env: dict[str, str],
    *arguments: str,
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [str(binary), "plugin", *arguments],
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )


def require_success(completed: subprocess.CompletedProcess[str]) -> str:
    if completed.returncode != 0:
        raise AssertionError(
            f"command failed ({completed.returncode}): {completed.stderr.strip()}"
        )
    return completed.stdout


def require_failure(completed: subprocess.CompletedProcess[str]) -> str:
    if completed.returncode == 0:
        raise AssertionError(f"command unexpectedly succeeded: {completed.stdout}")
    return completed.stdout


def normalize(text: str, root: Path) -> str:
    return text.replace(str(root), "<root>")


def hashes(directory: Path, names: list[str]) -> dict[str, str]:
    return {
        name: hashlib.sha256((directory / name).read_bytes()).hexdigest()
        for name in names
    }


def exercise(binary: Path, root: Path) -> dict[str, object]:
    root.mkdir()
    home = root / "home"
    hermes_home = root / "hermes"
    vault = root / "vault"
    home.mkdir()
    env = os.environ.copy()
    env.update({"HOME": str(home), "HERMES_HOME": str(hermes_home)})

    report: dict[str, object] = {}
    report["list"] = require_success(invoke(binary, env, "list"))

    missing = invoke(binary, env, "hermes", "status")
    report["missing"] = normalize(require_success(missing), root)
    missing_check = invoke(binary, env, "hermes", "status", "--check")
    report["missing_check_output"] = normalize(require_failure(missing_check), root)

    missing_parent = invoke(binary, env, "hermes", "install")
    require_failure(missing_parent)
    report["parent_guard"] = "Hermes plugin parent not found" in missing_parent.stderr

    (hermes_home / "plugins/memory").mkdir(parents=True)
    hermes_target = hermes_home / "plugins/memory/noema"
    dry = invoke(binary, env, "hermes", "install", "--check")
    report["dry"] = normalize(require_failure(dry), root)
    report["dry_no_write"] = not hermes_target.exists()

    first = invoke(binary, env, "hermes", "install")
    report["first"] = normalize(require_success(first), root)
    (hermes_target / "operator.txt").write_text("preserve me\n")
    second = invoke(binary, env, "hermes", "install")
    report["second"] = normalize(require_success(second), root)
    report["hermes_hashes"] = hashes(hermes_target, HERMES_FILES)

    changed_path = hermes_target / "transport.py"
    changed_path.write_text("local override\n")
    changed = invoke(binary, env, "hermes", "status")
    report["changed"] = normalize(require_success(changed), root)
    refused = invoke(binary, env, "hermes", "install")
    report["refused"] = normalize(require_failure(refused), root)
    report["refused_preserved"] = changed_path.read_text() == "local override\n"
    force_check = invoke(binary, env, "hermes", "install", "--check", "--force")
    report["force_check"] = normalize(require_failure(force_check), root)
    report["force_check_no_write"] = changed_path.read_text() == "local override\n"
    forced = invoke(binary, env, "hermes", "install", "--force")
    report["forced"] = normalize(require_success(forced), root)
    report["operator_preserved"] = (
        hermes_target / "operator.txt"
    ).read_text() == "preserve me\n"
    report["hermes_check"] = normalize(
        require_success(invoke(binary, env, "hermes", "status", "--check")), root
    )

    outside = root / "outside.txt"
    outside.write_text("outside data\n")
    managed = hermes_target / "__init__.py"
    managed.unlink()
    managed.symlink_to(outside)
    symlink_refused = invoke(binary, env, "hermes", "install")
    report["symlink_refused"] = normalize(require_failure(symlink_refused), root)
    symlink_forced = invoke(binary, env, "hermes", "install", "--force")
    report["symlink_forced"] = normalize(require_success(symlink_forced), root)
    report["symlink_safe"] = (
        managed.is_file()
        and not managed.is_symlink()
        and outside.read_text() == "outside data\n"
    )

    (vault / ".obsidian/plugins").mkdir(parents=True)
    obsidian_target = vault / ".obsidian/plugins/noema"
    obsidian = invoke(binary, env, "obsidian", "install", "--vault", str(vault))
    report["obsidian"] = normalize(require_success(obsidian), root)
    report["obsidian_hashes"] = hashes(obsidian_target, OBSIDIAN_FILES)
    report["obsidian_check"] = normalize(
        require_success(
            invoke(binary, env, "obsidian", "status", "--check", "--vault", str(vault))
        ),
        root,
    )
    report["overall"] = normalize(
        require_success(invoke(binary, env, "status", "--check", "--vault", str(vault))),
        root,
    )
    report["no_temp_files"] = not list(root.rglob(".noema-plugin-*.tmp"))
    return report


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")

    with tempfile.TemporaryDirectory(prefix="noema-rust-plugin-") as directory:
        root = Path(directory)
        reports = {
            "go": exercise(args.go.resolve(), root / "go"),
            "rust": exercise(args.rust.resolve(), root / "rust"),
        }

    if reports["go"] != reports["rust"]:
        for key in reports["go"]:
            if reports["go"][key] != reports["rust"][key]:
                raise AssertionError(
                    f"plugin lifecycle mismatch for {key}: "
                    f"Go={reports['go'][key]!r}, Rust={reports['rust'][key]!r}"
                )
        raise AssertionError("plugin lifecycle reports differ")
    if not all(
        reports["rust"][key]
        for key in [
            "parent_guard",
            "dry_no_write",
            "refused_preserved",
            "force_check_no_write",
            "operator_preserved",
            "symlink_safe",
            "no_temp_files",
        ]
    ):
        raise AssertionError(f"plugin safety invariant failed: {reports['rust']!r}")

    print("ok - Go/Rust embedded plugin inventory and payload hashes")
    print("ok - Go/Rust status, check, install, idempotency, and drift reporting")
    print("ok - Go/Rust refusal, force-check, and forced replacement behavior")
    print("ok - Go/Rust extra-file preservation and symlink safety")
    print("ok - Go/Rust Hermes and Obsidian target resolution")
    print("PASS: deterministic embedded-plugin lifecycle fixture")


if __name__ == "__main__":
    main()
