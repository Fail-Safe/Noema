"""Exercise the release installer with local archives and mock HTTP clients."""

import hashlib
import io
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "install.sh"


class InstallScriptTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.assets = self.root / "assets"
        self.assets.mkdir()
        self.download_tmp = self.root / "tmp"
        self.download_tmp.mkdir()
        self.destination = self.root / "installed bin's"
        self.env = dict(os.environ, PATH=str(self.bin), HOME=str(self.root),
                        NOEMA_VERSION="0.24.0", NOEMA_INSTALL_DIR=str(self.destination),
                        ASSETS=str(self.assets), TMPDIR=str(self.download_tmp),
                        MOCK_OS="Linux", MOCK_ARCH="x86_64")
        self.env.pop("NOEMA_SKIP_VERIFY", None)
        for command in ("awk", "cat", "chmod", "cp", "grep", "mkdir", "mktemp", "mv",
                        "gzip", "rm", "sed", "tail", "tar", "tr", "shasum"):
            path = shutil.which(command)
            if path:
                (self.bin / command).symlink_to(path)
        self.write_command("uname", 'case "$1" in -s) echo "$MOCK_OS" ;; -m) echo "$MOCK_ARCH" ;; esac')
        self.write_command("getconf", 'echo "glibc 2.36"')
        self.write_command("curl", r'''
import os, pathlib, sys
args = sys.argv[1:]
url = args[-1]
root = pathlib.Path(os.environ["ASSETS"])
with (root / "requests").open("a") as f:
    f.write(url + "\n")
if url.endswith("/latest"):
    print("https://github.com/Fail-Safe/Noema/releases/tag/" + os.environ.get("LATEST_TAG", "v0.24.0"), end="")
    sys.exit(0)
asset = root / url.rsplit("/", 1)[-1]
status = os.environ.get("MUSL_STATUS", "200") if "_musl" in url else "200"
if not asset.exists():
    status = "404"
if status == "200":
    pathlib.Path(args[args.index("-o") + 1]).write_bytes(asset.read_bytes())
print(status, end="")
''', python=True)
        self.write_command("wget", r'''
import os, pathlib, sys
args = sys.argv[1:]
url = args[-1]
root = pathlib.Path(os.environ["ASSETS"])
with (root / "requests").open("a") as f:
    f.write(url + "\n")
if url.endswith("/latest"):
    print("  HTTP/1.1 302 Found\n  Location: https://github.com/Fail-Safe/Noema/releases/tag/" + os.environ.get("LATEST_TAG", "v0.24.0") + " [following]\n  HTTP/1.1 200 OK", file=sys.stderr)
    sys.exit(0)
asset = root / url.rsplit("/", 1)[-1]
status = os.environ.get("MUSL_STATUS", "200") if "_musl" in url else "200"
if not asset.exists():
    status = "404"
print("  HTTP/1.1 " + status, file=sys.stderr)
if status != "200":
    sys.exit(8)
pathlib.Path(args[args.index("-O") + 1]).write_bytes(asset.read_bytes())
''', python=True)

    def write_command(self, name, body, python=False):
        path = self.bin / name
        path.write_text((f"#!{sys.executable}\n" if python else "#!/bin/sh\n") + body + "\n")
        path.chmod(0o755)

    def archive(self, os_name="linux", arch="amd64", musl=True, version="0.24.0", broken=False):
        suffix = "_musl" if musl else ""
        name = f"noema_{version}_{os_name}_{arch}{suffix}.tar.gz"
        body = (b'#!/bin/sh\necho "compatibility stdout"\necho "compatibility stderr" >&2\nexit 7\n'
                if broken else b'#!/bin/sh\necho "noema fixture version"\n')
        with tarfile.open(self.assets / name, "w:gz") as archive:
            info = tarfile.TarInfo("noema")
            info.size = len(body)
            info.mode = 0o755
            archive.addfile(info, io.BytesIO(body))
        digest = hashlib.sha256((self.assets / name).read_bytes()).hexdigest()
        with (self.assets / "checksums.txt").open("a") as checksums:
            checksums.write(f"{digest}  {name}\n")
        return name

    def run_script(self, *args, success=True, content=None):
        result = subprocess.run(["/bin/sh", str(SCRIPT), *args] if content is None else ["/bin/sh"],
                                input=content, env=self.env, capture_output=True, text=True)
        if success:
            self.assertEqual(0, result.returncode, result.stderr)
        else:
            self.assertNotEqual(0, result.returncode, result.stdout)
        self.assertFalse(list(self.destination.glob(".noema.*")))
        self.assertEqual([], list(self.download_tmp.iterdir()))
        return result

    def test_musl_install_and_path_quoting(self):
        self.archive()
        result = self.run_script()
        self.assertTrue((self.destination / "noema").exists())
        self.assertIn("my-cortex", result.stdout)
        self.assertEqual(1, result.stdout.count("noema fixture version"))
        self.assertIn(f"Installed: {self.destination.resolve()}/noema\nnoema fixture version\n",
                      result.stdout)
        line = next(line.strip() for line in result.stdout.splitlines() if "export PATH=" in line)
        checked = subprocess.run(["/bin/sh", "-c", line + '; printf "%s" "$PATH"'],
                                 env={"PATH": "/usr/bin"}, capture_output=True, text=True, check=True)
        self.assertEqual(f"{self.destination.resolve()}:/usr/bin", checked.stdout)

    def test_fallback_only_on_404_for_both_downloaders(self):
        self.archive(musl=False)
        for downloader in ("curl", "wget"):
            with self.subTest(downloader=downloader):
                if downloader == "wget":
                    (self.bin / "curl").unlink()
                result = self.run_script()
                self.assertIn("falling back", result.stderr)
                self.assertIn("glibc 2.39", result.stderr)

    def test_server_error_does_not_fallback(self):
        self.archive()
        self.archive(musl=False)
        self.env["MUSL_STATUS"] = "503"
        for downloader in ("curl", "wget"):
            with self.subTest(downloader=downloader):
                if downloader == "wget":
                    (self.bin / "curl").unlink()
                result = self.run_script(success=False)
                self.assertNotIn("falling back", result.stderr)
        requests = (self.assets / "requests").read_text()
        self.assertNotIn("amd64.tar.gz", requests)

    def test_checksum_mismatch_preserves_existing_binary(self):
        self.archive()
        self.destination.mkdir()
        installed = self.destination / "noema"
        installed.write_text("original")
        (self.assets / "checksums.txt").write_text("0" * 64 + "  noema_0.24.0_linux_amd64_musl.tar.gz\n")
        self.env["NOEMA_SKIP_VERIFY"] = "1"
        self.assertIn("mismatch", self.run_script(success=False).stderr)
        self.assertEqual("original", installed.read_text())

    def test_missing_and_duplicate_checksums(self):
        self.archive()
        path = self.assets / "checksums.txt"
        original = path.read_text()
        for contents in ("", original + original):
            path.write_text(contents)
            self.assertIn("missing or duplicate", self.run_script(success=False).stderr)

    def test_missing_verifier_requires_opt_in(self):
        self.archive()
        (self.bin / "shasum").unlink()
        self.assertIn("sha256sum or shasum", self.run_script(success=False).stderr)
        self.env["NOEMA_SKIP_VERIFY"] = "1"
        self.assertIn("verification explicitly skipped", self.run_script().stderr)

    def test_sha256sum_verifier(self):
        self.archive()
        self.write_command("sha256sum", 'import hashlib, pathlib, sys\np=sys.argv[1]\nprint(hashlib.sha256(pathlib.Path(p).read_bytes()).hexdigest() + "  " + p)', python=True)
        self.run_script()

    def test_incompatible_binary_preserves_existing_install(self):
        self.archive(broken=True)
        self.destination.mkdir()
        (self.destination / "noema").write_text("original")
        result = self.run_script(success=False)
        self.assertIn("compatibility stdout\ncompatibility stderr\n", result.stderr)
        self.assertIn("failed the compatibility check", result.stderr)
        self.assertNotIn("compatibility", result.stdout)
        self.assertNotIn("Installed:", result.stdout)
        self.assertEqual("original", (self.destination / "noema").read_text())

    def test_other_supported_platforms(self):
        for os_name, arch, machine in (("linux", "arm64", "arm64"),
                                       ("darwin", "amd64", "amd64")):
            with self.subTest(os=os_name, arch=arch):
                self.archive(os_name=os_name, arch=arch, musl=os_name == "linux")
                self.env.update(MOCK_OS="Linux" if os_name == "linux" else "Darwin",
                                MOCK_ARCH=machine)
                self.run_script()

    def test_macos_arm64_signing_and_prerelease(self):
        self.archive(os_name="darwin", arch="arm64", musl=False, version="0.24.0-rc.1")
        self.env.update(MOCK_OS="Darwin", MOCK_ARCH="aarch64", NOEMA_VERSION="v0.24.0-rc.1")
        self.write_command("codesign", 'printf "%s\\n" "$*" > "$ASSETS/codesign"; echo "signing stdout"; echo "replacing existing signature" >&2')
        result = self.run_script()
        self.assertEqual("", result.stderr)
        self.assertNotIn("signing stdout", result.stdout)
        self.assertNotIn("replacing existing signature", result.stdout)
        self.assertEqual(1, result.stdout.count("noema fixture version"))
        self.assertIn("--force --sign -", (self.assets / "codesign").read_text())

    def test_codesign_failure_preserves_existing_install(self):
        self.archive(os_name="darwin", musl=False)
        self.env["MOCK_OS"] = "Darwin"
        self.destination.mkdir()
        installed = self.destination / "noema"
        installed.write_text("original")
        self.write_command("codesign", 'echo "signing stdout"; echo "signing stderr" >&2; exit 1')
        result = self.run_script(success=False)
        self.assertIn("signing stdout\nsigning stderr\n", result.stderr)
        self.assertIn("could not sign the downloaded binary", result.stderr)
        self.assertNotIn("signing", result.stdout)
        self.assertNotIn("Installed:", result.stdout)
        self.assertEqual("original", installed.read_text())

    def test_latest_stable_for_both_downloaders(self):
        self.archive()
        self.env.pop("NOEMA_VERSION")
        self.run_script()
        (self.bin / "curl").unlink()
        self.run_script()
        self.env["LATEST_TAG"] = "v0.24.0-beta.1"
        self.assertIn("prerelease", self.run_script(success=False).stderr)

    def test_musl_fallback_warning(self):
        self.archive(musl=False)
        self.write_command("getconf", "exit 1")
        self.write_command("ldd", 'echo "musl libc (x86_64)" >&2; exit 1')
        self.assertIn("system uses musl", self.run_script().stderr)

    def test_help_unsupported_platform_and_missing_downloader(self):
        self.assertIn("NOEMA_VERSION", self.run_script("--help").stdout)
        self.env["MOCK_OS"] = "FreeBSD"
        self.assertIn("unsupported OS", self.run_script(success=False).stderr)
        self.env["MOCK_OS"] = "Linux"
        self.env["MOCK_ARCH"] = "riscv64"
        self.assertIn("unsupported architecture", self.run_script(success=False).stderr)
        self.env["MOCK_ARCH"] = "amd64"
        (self.bin / "curl").unlink()
        (self.bin / "wget").unlink()
        self.assertIn("curl or wget", self.run_script(success=False).stderr)

    def test_truncated_pipe_does_not_execute(self):
        content = SCRIPT.read_text().rsplit('main "$@"', 1)[0]
        self.run_script(content=content)
        self.run_script(content=content[:len(content) // 2], success=False)
        self.assertFalse((self.assets / "requests").exists())
        self.assertFalse(self.destination.exists())


if __name__ == "__main__":
    unittest.main()
