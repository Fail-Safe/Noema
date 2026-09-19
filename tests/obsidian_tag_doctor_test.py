import importlib.util
import json
import sys
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).parents[1] / "scripts" / "obsidian-tag-doctor.py"
SPEC = importlib.util.spec_from_file_location("obsidian_tag_doctor", SCRIPT)
assert SPEC and SPEC.loader
MODULE = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


def trace(body, *, trace_id="20260829-example", tier="short"):
    return (
        "---\n"
        f"id: {trace_id}\n"
        "title: Example\n"
        "type: note\n"
        f"tier: {tier}\n"
        "tags:\n"
        "- real-frontmatter-tag\n"
        "created: 2026-08-29T00:00:00Z\n"
        "updated: 2026-08-29T00:00:00Z\n"
        "---\n\n"
        f"{body}"
    )


class ObsidianTagDoctorTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        (self.root / "traces").mkdir()
        (self.root / "archive/traces").mkdir(parents=True)
        (self.root / "trash/traces").mkdir(parents=True)
        (self.root / "cortex.md").write_text(
            "---\nid: cortex-test-id\nname: test-cortex\ncreated: 2026-08-29T00:00:00Z\nversion: 2\n---\n",
            encoding="utf-8",
        )

    def tearDown(self):
        self.temporary.cleanup()

    def write_trace(self, relative, body, tier="short"):
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(trace(body, trace_id=path.stem, tier=tier), encoding="utf-8")
        return path

    def test_classifies_visible_candidates_and_ignores_markdown_syntax(self):
        body = """# Heading with #heading-tag
Color #DA7756 and alpha #1e1e1ee6.
#font-family = Example Mono
Keep the intentional #real-tag for review.
Ignore `#inline-code` and \\#escaped-tag and numeric #2026.
Keep Unicode #café for review; ignore nested numeric #2026/08.
Ignore [an anchor](#section-name) and https://example.test/page#fragment.
```ini
palette = 1=#C4956A
#commented-setting = true
```not-a-closing-fence
#still-in-code = true
```
    indented = #abcdef
"""
        path = self.write_trace("traces/20260829-visible.md", body, tier="mid")
        before = path.read_bytes()

        report = MODULE.scan_cortex(self.root)

        self.assertEqual(before, path.read_bytes())
        self.assertEqual([], report.errors)
        self.assertEqual(
            [
                ("#heading-tag", "inline-hashtag", "review"),
                ("#DA7756", "hex-color", "high"),
                ("#1e1e1ee6", "hex-color", "high"),
                ("#font-family", "config-comment", "high"),
                ("#real-tag", "inline-hashtag", "review"),
                ("#café", "inline-hashtag", "review"),
            ],
            [(item.token, item.classification, item.confidence) for item in report.candidates],
        )
        self.assertTrue(all(item.tier == "mid" for item in report.candidates))

    def test_scans_active_and_archive_but_not_trash_by_default(self):
        self.write_trace("traces/20260829-active.md", "Color #abcdef\n")
        self.write_trace("archive/traces/20260829-archived.md", "Color #fedcba\n", tier="long")
        self.write_trace("trash/traces/20260829-trashed.md", "Color #aabbcc\n")

        report = MODULE.scan_cortex(self.root)

        self.assertEqual(1, report.scanned_active)
        self.assertEqual(1, report.scanned_archived)
        self.assertEqual(0, report.scanned_trash)
        self.assertEqual(
            {"traces/20260829-active.md", "archive/traces/20260829-archived.md"},
            {item.path for item in report.candidates},
        )

        with_trash = MODULE.scan_cortex(self.root, include_trash=True)
        self.assertEqual(3, len(with_trash.candidates))
        self.assertEqual(1, with_trash.scanned_trash)

    def test_reports_malformed_trace_without_scanning_its_body(self):
        malformed = self.root / "traces/invalid.md"
        malformed.write_text("Color #abcdef\n", encoding="utf-8")

        report = MODULE.scan_cortex(self.root)

        self.assertEqual([], report.candidates)
        self.assertEqual(1, len(report.errors))
        self.assertIn("missing framed YAML", report.errors[0].error)

    def test_json_summary_is_stable_and_marks_report_read_only(self):
        self.write_trace("traces/20260829-active.md", "Color #abcdef and #real-tag\n")

        payload = json.loads(MODULE.report_json(MODULE.scan_cortex(self.root)))

        self.assertEqual(1, payload["schema_version"])
        self.assertTrue(payload["read_only"])
        self.assertEqual(2, payload["summary"]["candidate_count"])
        self.assertEqual(
            {"hex-color": 1, "inline-hashtag": 1},
            payload["summary"]["classifications"],
        )
        self.assertEqual({"short": 1}, payload["summary"]["affected_traces_by_tier"])

    def test_active_cortex_parser_preserves_spaces_in_path(self):
        completed = MODULE.subprocess.CompletedProcess(
            args=["noema", "cortex", "list"],
            returncode=0,
            stdout="example\t/tmp/My Cortex  *\n",
            stderr="",
        )
        original = MODULE.subprocess.run
        MODULE.subprocess.run = lambda *args, **kwargs: completed
        try:
            self.assertEqual(Path("/tmp/My Cortex"), MODULE.active_cortex_dir())
        finally:
            MODULE.subprocess.run = original

    def test_plan_fences_dense_config_and_wraps_remaining_literals(self):
        body = """# --- COLORS ---
palette = 0=#1C1B1A
palette = 1=#DA7756
palette = 2=#6B9E7A
palette = 3=#C4956A
palette = 4=#8B9DC3
Summary uses #181818 and #F2EDE6.
"""
        path = self.write_trace("traces/20260829-config.md", body, tier="long")
        before = path.read_bytes()
        report = MODULE.scan_cortex(self.root)

        plan = MODULE.build_plans(report)[0]

        self.assertEqual(before, path.read_bytes())
        self.assertEqual(
            ["fenced-block", "inline-code"],
            [action.strategy for action in plan.actions],
        )
        self.assertIn("```ini\n# --- COLORS ---", plan.proposed_source)
        self.assertIn("```\n\nSummary uses `#181818` and `#F2EDE6`.", plan.proposed_source)
        self.assertEqual(1, plan.actions[1].additional_edit_count)
        self.assertIn("+++ traces/20260829-config.md (proposed)", plan.diff)

    def test_plan_fences_contiguous_commented_yaml_excerpt(self):
        body = """Prose before.

**User:** pasted configuration follows
root: value
#device_tracker:
#  - platform: snmp

#logger:
#  default: warning
template:
  - sensor: []
**Assistant:** analysis follows
"""
        self.write_trace("archive/traces/20260829-yaml.md", body)

        plan = MODULE.build_plans(MODULE.scan_cortex(self.root))[0]

        self.assertEqual(["fenced-block"], [action.strategy for action in plan.actions])
        self.assertIn("**User:** pasted configuration follows\n\n```yaml\nroot: value", plan.proposed_source)
        self.assertIn("template:\n  - sensor: []\n```\n\n**Assistant:**", plan.proposed_source)

    def test_machine_plan_is_hash_pinned_and_contains_only_typed_operations(self):
        self.write_trace(
            "traces/20260829-colors.md",
            "Color #abcdef.\n",
            tier="long",
        )
        report = MODULE.scan_cortex(self.root)

        payload = MODULE.normalization_payload(report)

        self.assertEqual("cortex-test-id", payload["cortex_id"])
        self.assertEqual(1, payload["schema_version"])
        planned = payload["traces"][0]
        self.assertEqual("long", planned["tier"])
        self.assertEqual(
            [
                {
                    "strategy": "inline-code",
                    "line": 1,
                    "column": 7,
                    "literal": "#abcdef",
                }
            ],
            planned["operations"],
        )
        self.assertNotIn("body", planned)
        self.assertNotEqual(
            planned["expected_content_hash"], planned["expected_result_hash"]
        )


if __name__ == "__main__":
    unittest.main()
