#!/usr/bin/env python3
"""Portable frozen-profile hook adapter, not the private tested adapter's bytes.

Invoke explicitly as `python profile_hook.py codex` with hook JSON on stdin to
run native prefetch at 8,000 characters. Other modes delegate unchanged to the
public harness. This adapter is not hash-identical to the private historical
adapter and publishing it does not establish a new live-run result. Importing
this module performs no native invocation. NOEMA_BIN/NOEMA_CORTEX and the public
harness's optional metrics environment are consumed only when invoked.
"""
from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import sys


def load_harness():
    source = Path(__file__).resolve().parents[2] / "prefetch_hook.py"
    spec = importlib.util.spec_from_file_location("continuity_public_prefetch_hook", source)
    if spec is None or spec.loader is None:
        raise RuntimeError("public prefetch harness unavailable")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def main(argv=None):
    args = list(sys.argv[1:] if argv is None else argv)
    harness = load_harness()
    if not args or args[0] != "codex":
        return harness.main(args)
    context, metrics = harness.render(
        sys.stdin.read(), "codex-hook", max_results=8, max_preferences=0,
        max_chars=8000, exclude_startup_preferences=True,
    )
    harness._write_metrics(metrics)
    print(json.dumps({"hookSpecificOutput": {
        "hookEventName": "UserPromptSubmit", "additionalContext": context,
    }}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
