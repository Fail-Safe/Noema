"""Cursor startup snapshot adoption and conservative fallback."""

import json
import os
from pathlib import Path
import subprocess

import pytest


@pytest.mark.skipif(os.name != "posix", reason="Cursor shell bootstrap")
@pytest.mark.parametrize("mode", ["complete", "truncated", "older", "invalid"])
def test_cursor_bootstrap_uses_complete_server_view_only(tmp_path, mode):
    template = Path(__file__).resolve().parents[3] / "src/integrate/cursor_noema_bootstrap.sh"
    script = tmp_path / "hook.sh"
    script.write_text(template.read_text().replace("__CURSOR_HOOK_MARKER__", "test-hook")
                      .replace("__CURSOR_PREFERENCE_BANNER__", "[preferences]")
                      .replace("__BOOTSTRAP_JSON__", json.dumps("startup guidance")))
    script.chmod(0o700)
    fixture = tmp_path / "snapshot.json"
    fixture.write_text(json.dumps({"cortex_id": "cortex-a", "cortex_name": "example",
        "cortex_directory": "/example/cortex", "max_body_chars": 12000,
        "recall_context": {"preferences_revision": None if mode == "invalid" else "server-revision",
                           "preferences_unchanged": False, "preference_results_truncated": mode == "truncated",
                           "preferences": [{"id": "policy", "title": "Policy", "body": "snapshot-body",
                                            "body_truncated": False}]}}))
    noema = tmp_path / "noema"
    noema.write_text('''#!/usr/bin/env python3
import json,os,sys,pathlib
command=sys.argv[1]
with open(os.environ['CALL_LOG'],'a') as out:out.write(command+'\\n')
if command=='preference-snapshot':
 if os.environ['TEST_MODE']=='older':
  print("error: unrecognized subcommand 'preference-snapshot'",file=sys.stderr);sys.exit(2)
 print(pathlib.Path(os.environ['SNAPSHOT']).read_text())
elif command=='list':print('ID\\tTitle\\npolicy\\tPolicy')
elif command=='get':print('Title: Policy\\n\\nlegacy-full-body')
else:sys.exit(9)
''')
    noema.chmod(0o700)
    log = tmp_path / "calls"
    env = dict(os.environ, NOEMA_BIN=str(noema), TEST_MODE=mode, SNAPSHOT=str(fixture), CALL_LOG=str(log))
    env.pop("NOEMA_CORTEX", None)
    result = subprocess.run([str(script)], input='{"session_id":"example"}\n', text=True,
                            capture_output=True, env=env, check=True)
    context = json.loads(result.stdout)["additional_context"]
    if mode == "complete":
        assert "snapshot-body" in context and "server-revision" in context
        assert "cortex_usage" in context and "cortex.id" in context
        assert "HTTP and stdio" in context and "verified remote connection" in context
        assert "ID is different or unavailable" in context
        assert "matching name or hostname alone" in context
        assert "local stdio" not in context
        assert log.read_text().splitlines() == ["preference-snapshot"]
    elif mode == "invalid":
        assert "ERROR" in context and "snapshot-body" not in context
        assert "server-revision" not in context
    else:
        assert "legacy-full-body" in context and "server-revision" not in context
        assert log.read_text().splitlines() == ["preference-snapshot", "list", "get"]
