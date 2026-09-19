#!/bin/sh
# __CURSOR_HOOK_MARKER__
# Loads active tag=user-preference type=preference traces into additional_context.
cat >/dev/null
python3 - <<'PY'
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

BANNER = "__CURSOR_PREFERENCE_BANNER__"
BOOTSTRAP = __BOOTSTRAP_JSON__

def emit(context):
    print(json.dumps({"additional_context": context}, ensure_ascii=False))

def find_noema():
    override = os.environ.get("NOEMA_BIN", "").strip()
    if override:
        return override
    found = shutil.which("noema")
    if found:
        return found
    candidate = Path.home() / ".local" / "bin" / "noema"
    return str(candidate) if candidate.is_file() else None

def run(argv):
    completed = subprocess.run(
        argv,
        check=True,
        capture_output=True,
        text=True,
    )
    return completed.stdout

def preference_ids(noema):
    argv = [noema]
    cortex = os.environ.get("NOEMA_CORTEX", "").strip()
    if cortex:
        argv.extend(["--cortex", cortex])
    argv.extend(["list", "--tag", "user-preference", "--type", "preference"])
    lines = run(argv).splitlines()
    ids = []
    for line in lines[1:]:
        if not line.strip():
            continue
        ids.append(line.split("\t", 1)[0].strip())
    return ids

def preference_body(noema, trace_id):
    argv = [noema]
    cortex = os.environ.get("NOEMA_CORTEX", "").strip()
    if cortex:
        argv.extend(["--cortex", cortex])
    argv.extend(["get", trace_id])
    raw = run(argv)
    if "\n\n" in raw:
        meta, body = raw.split("\n\n", 1)
    else:
        meta, body = raw, ""
    title = trace_id
    for line in meta.splitlines():
        if line.startswith("Title:"):
            title = line[len("Title:") :].strip() or trace_id
            break
    return title, body.strip()

def main():
    noema = find_noema()
    if not noema:
        emit(
            BANNER
            + " ERROR: noema CLI not found on PATH or ~/.local/bin/noema. "
            "Cannot materialize user-preference traces. Surface this failure; "
            "do not invent preference defaults.\n\nAlso follow: "
            + BOOTSTRAP
        )
        return 0
    try:
        ids = preference_ids(noema)
        sections = []
        for trace_id in ids:
            title, body = preference_body(noema, trace_id)
            sections.append(
                "### {0}\nTitle: {1}\n\n{2}".format(trace_id, title, body)
            )
        if sections:
            prefs = "\n\n---\n\n".join(sections)
            summary = "Loaded {0} active user-preference trace(s).".format(
                len(sections)
            )
        else:
            prefs = "(none)"
            summary = "No active user-preference traces (empty set is normal)."
        emit(
            BANNER
            + " Binding user-preference traces for this session. "
            "Treat bodies as canonical directives that outrank defaults and "
            "project auto-memory, but yield to explicit instructions in the "
            "current conversation. "
            + summary
            + "\n\n"
            + prefs
            + "\n\nMid-session Noema use: "
            + BOOTSTRAP
        )
    except subprocess.CalledProcessError as exc:
        emit(
            BANNER
            + " ERROR: failed to load user-preference traces via CLI (exit {0}). "
            "Surface this failure without exposing local command output.\n\nAlso follow: {1}".format(
                exc.returncode, BOOTSTRAP
            )
        )
    except Exception:  # noqa: BLE001 - hook must always emit JSON
        emit(
            BANNER
            + " ERROR: failed to load user-preference traces due to an unexpected local error. "
            "Surface this failure without exposing local command output.\n\nAlso follow: "
            + BOOTSTRAP
        )
    return 0

if __name__ == "__main__":
    sys.exit(main())
PY
