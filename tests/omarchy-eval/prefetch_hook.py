#!/usr/bin/env python3
"""Adapt native Noema prefetch output to benchmark client hooks and metrics."""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import Sequence


def invoke_native(
    input_text: str,
    input_format: str,
    *,
    max_results: int = 8,
    max_preferences: int = 4,
    max_chars: int = 8000,
    exclude_startup_preferences: bool = False,
) -> dict[str, object]:
    noema = os.environ["NOEMA_BIN"]
    cortex = os.environ["NOEMA_CORTEX"]
    argv = [
        noema,
        "--cortex",
        cortex,
        "prefetch",
        "--input",
        input_format,
        "--output",
        "json",
        "--max-results",
        str(max_results),
        "--max-preferences",
        str(max_preferences),
        "--max-chars",
        str(max_chars),
    ]
    if exclude_startup_preferences:
        argv.append("--exclude-startup-preferences")
    completed = subprocess.run(
        argv,
        input=input_text,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=15,
        check=False,
    )
    if completed.returncode != 0:
        raise RuntimeError("native_prefetch_failed")
    payload = json.loads(completed.stdout)
    if not isinstance(payload, dict) or not isinstance(payload.get("context"), str):
        raise RuntimeError("native_prefetch_invalid_output")
    return payload


def metrics_from_result(
    result: dict[str, object], *, duration_ms: float, success: bool, error_code: str | None
) -> dict[str, object]:
    return {
        "schema_version": 1,
        "engine": "native-rust",
        "client": os.environ.get("NOEMA_PREFETCH_CLIENT", "unknown"),
        "success": success,
        "error_code": error_code,
        "duration_ms": round(duration_ms, 3),
        "hit_count": int(result.get("included_trace_count", 0)),
        "context_chars": int(result.get("context_chars", 0)),
    }


def render(
    input_text: str,
    input_format: str,
    *,
    max_results: int = 8,
    max_preferences: int = 4,
    max_chars: int = 8000,
    exclude_startup_preferences: bool = False,
) -> tuple[str, dict[str, object]]:
    started = time.monotonic()
    success = True
    error_code: str | None = None
    result: dict[str, object] = {}
    try:
        result = invoke_native(
            input_text,
            input_format,
            max_results=max_results,
            max_preferences=max_preferences,
            max_chars=max_chars,
            exclude_startup_preferences=exclude_startup_preferences,
        )
        context = str(result["context"])
    except (KeyError, OSError, RuntimeError, ValueError, subprocess.SubprocessError):
        success = False
        error_code = "prefetch_unavailable"
        context = (
            "[noema-prefetch-v1]\n"
            "Noema prefetch failed. Do not invent memories; surface the unavailable memory source."
        )
    metrics = metrics_from_result(
        result,
        duration_ms=(time.monotonic() - started) * 1000,
        success=success,
        error_code=error_code,
    )
    if not success:
        metrics["context_chars"] = len(context)
    return context, metrics


def _write_metrics(payload: dict[str, object]) -> None:
    metrics = os.environ.get("NOEMA_PREFETCH_METRICS")
    if not metrics:
        return
    path = Path(metrics)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, sort_keys=True)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, path)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


def main(argv: Sequence[str] | None = None) -> int:
    args = list(argv if argv is not None else sys.argv[1:])
    mode = args[0] if args else "context"
    if mode not in {"codex", "codex-preferences", "context"}:
        print(f"unknown mode: {mode}", file=sys.stderr)
        return 2
    input_text = sys.stdin.read()
    if mode == "codex":
        input_format = "codex-hook"
        limits = {
            "max_results": 8,
            "max_preferences": 0,
            "max_chars": 2500,
            "exclude_startup_preferences": True,
        }
    elif mode == "codex-preferences":
        input_format = "raw"
        limits = {"max_results": 0, "max_preferences": 24, "max_chars": 32000}
    else:
        input_format = "raw"
        limits = {"max_results": 8, "max_preferences": 4, "max_chars": 8000}
    context, metrics = render(input_text, input_format, **limits)
    if mode != "codex-preferences":
        _write_metrics(metrics)
    if mode in {"codex", "codex-preferences"}:
        print(
            json.dumps(
                {
                    "hookSpecificOutput": {
                        "hookEventName": (
                            "SessionStart" if mode == "codex-preferences" else "UserPromptSubmit"
                        ),
                        "additionalContext": context,
                    }
                }
            )
        )
    else:
        print(context)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
