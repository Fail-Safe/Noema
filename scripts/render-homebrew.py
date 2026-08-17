#!/usr/bin/env python3
"""Render Homebrew metadata from already-built Noema release archives."""

from __future__ import annotations

import argparse
import hashlib
from pathlib import Path


def checksum(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def archive_name(version: str, os_name: str, arch: str) -> str:
    return f"noema_{version}_{os_name}_{arch}.tar.gz"


def release_url(base_url: str, tag: str, archive: str) -> str:
    return f"{base_url.rstrip('/')}/{tag}/{archive}"


def formula(version: str, base_url: str, dist: Path, prerelease: bool) -> str:
    tag = f"v{version}"
    class_name = "NoemaBeta" if prerelease else "Noema"
    conflict = '  conflicts_with "noema", because: "both install the noema binary"\n' if prerelease else ""

    targets = {
        ("darwin", "amd64"): checksum(dist / archive_name(version, "darwin", "amd64")),
        ("darwin", "arm64"): checksum(dist / archive_name(version, "darwin", "arm64")),
        ("linux", "amd64"): checksum(dist / archive_name(version, "linux", "amd64")),
        ("linux", "arm64"): checksum(dist / archive_name(version, "linux", "arm64")),
    }

    def stanza(os_name: str, arch: str, indent: str) -> str:
        archive = archive_name(version, os_name, arch)
        return (
            f'{indent}url "{release_url(base_url, tag, archive)}"\n'
            f'{indent}sha256 "{targets[(os_name, arch)]}"\n'
        )

    return (
        f"class {class_name} < Formula\n"
        '  desc "The intentional memory layer for your AI agents"\n'
        '  homepage "https://github.com/Fail-Safe/Noema"\n'
        f'  version "{version}"\n'
        '  license "MIT"\n'
        f"{conflict}"
        "\n"
        "  on_macos do\n"
        "    if Hardware::CPU.arm?\n"
        f'{stanza("darwin", "arm64", "      ")}'
        "    else\n"
        f'{stanza("darwin", "amd64", "      ")}'
        "    end\n"
        "  end\n"
        "\n"
        "  on_linux do\n"
        "    if Hardware::CPU.arm?\n"
        f'{stanza("linux", "arm64", "      ")}'
        "    else\n"
        f'{stanza("linux", "amd64", "      ")}'
        "    end\n"
        "  end\n"
        "\n"
        "  def install\n"
        '    bin.install "noema"\n'
        "  end\n"
        "\n"
        "  test do\n"
        '    system "#{bin}/noema", "version"\n'
        "  end\n"
        "end\n"
    )


def cask(version: str, base_url: str, dist: Path) -> str:
    tag = f"v{version}"
    arm_archive = archive_name(version, "darwin", "arm64")
    intel_archive = archive_name(version, "darwin", "amd64")
    arm_sha = checksum(dist / arm_archive)
    intel_sha = checksum(dist / intel_archive)
    return (
        'cask "noema" do\n'
        f'  version "{version}"\n'
        '  arch arm: "arm64", intel: "amd64"\n'
        f'  sha256 arm: "{arm_sha}", intel: "{intel_sha}"\n'
        f'  url "{release_url(base_url, tag, archive_name(version, "darwin", "#{arch}"))}"\n'
        '  name "Noema"\n'
        '  desc "The intentional memory layer for your AI agents"\n'
        '  homepage "https://github.com/Fail-Safe/Noema"\n'
        "\n"
        '  binary "noema"\n'
        "\n"
        "  postflight do\n"
        '    system_command "/usr/bin/xattr", args: ["-dr", "com.apple.quarantine", "#{staged_path}/noema"]\n'
        "  end\n"
        "end\n"
    )


def render(version: str, base_url: str, dist: Path, tap: Path) -> list[Path]:
    prerelease = "-" in version
    formula_dir = tap / "Formula"
    formula_dir.mkdir(parents=True, exist_ok=True)
    formula_path = formula_dir / ("noema-beta.rb" if prerelease else "noema.rb")
    formula_path.write_text(formula(version, base_url, dist, prerelease), encoding="utf-8")
    written = [formula_path]

    if not prerelease:
        cask_dir = tap / "Casks"
        cask_dir.mkdir(parents=True, exist_ok=True)
        cask_path = cask_dir / "noema.rb"
        cask_path.write_text(cask(version, base_url, dist), encoding="utf-8")
        written.append(cask_path)

    return written


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--version", required=True)
    parser.add_argument("--dist", type=Path, required=True)
    parser.add_argument("--tap", type=Path, required=True)
    parser.add_argument(
        "--base-url",
        default="https://github.com/Fail-Safe/Noema/releases/download",
    )
    args = parser.parse_args()
    render(args.version, args.base_url, args.dist, args.tap)


if __name__ == "__main__":
    main()
