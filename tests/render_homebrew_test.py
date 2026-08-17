import importlib.util
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).parents[1] / "scripts" / "render-homebrew.py"
SPEC = importlib.util.spec_from_file_location("render_homebrew", SCRIPT)
assert SPEC and SPEC.loader
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class RenderHomebrewTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.dist = self.root / "dist"
        self.tap = self.root / "tap"
        self.dist.mkdir()

    def tearDown(self):
        self.temporary.cleanup()

    def write_archives(self, version):
        for os_name in ("darwin", "linux"):
            for arch in ("amd64", "arm64"):
                name = MODULE.archive_name(version, os_name, arch)
                (self.dist / name).write_bytes(name.encode())

    def test_stable_release_writes_formula_and_cask(self):
        self.write_archives("0.20.0")
        written = MODULE.render("0.20.0", "https://example.test/download", self.dist, self.tap)

        self.assertEqual(2, len(written))
        formula = (self.tap / "Formula" / "noema.rb").read_text()
        cask = (self.tap / "Casks" / "noema.rb").read_text()
        self.assertIn("class Noema < Formula", formula)
        self.assertIn("noema_0.20.0_linux_arm64.tar.gz", formula)
        self.assertIn('arch arm: "arm64", intel: "amd64"', cask)
        self.assertNotIn("noema-beta", formula)

    def test_prerelease_updates_only_beta_formula(self):
        self.write_archives("0.20.0-beta.1")
        written = MODULE.render(
            "0.20.0-beta.1", "https://example.test/download", self.dist, self.tap
        )

        self.assertEqual([self.tap / "Formula" / "noema-beta.rb"], written)
        formula = written[0].read_text()
        self.assertIn("class NoemaBeta < Formula", formula)
        self.assertIn('conflicts_with "noema"', formula)
        self.assertFalse((self.tap / "Casks" / "noema.rb").exists())


if __name__ == "__main__":
    unittest.main()
