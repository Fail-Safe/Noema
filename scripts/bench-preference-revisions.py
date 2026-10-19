#!/usr/bin/env python3
"""Measure conditional preference retrieval over a real isolated MCP stdio server."""

import argparse
import importlib.util
import json
import math
import os
from pathlib import Path
import statistics
import subprocess
import sys
import tempfile
import time


def distribution(values):
    ordered = sorted(values)
    return {
        "samples": len(values),
        "p50_ms": statistics.median(values),
        "p95_ms": ordered[math.ceil(len(values) * 0.95) - 1],
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--bodies", type=Path, help="Private JSON array of preference bodies")
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    binary = args.binary.resolve()
    bodies = json.loads(args.bodies.read_text()) if args.bodies else ["Evidence policy. " * 250] * 8
    assert bodies and len(bodies) < 24, "Leave one preference slot for the addition check"
    spec = importlib.util.spec_from_file_location(
        "noema_transport", Path(__file__).resolve().parents[1] / "plugins/hermes/transport.py"
    )
    transport = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(transport)

    class MeasuredTransport(transport.StdioTransport):
        def _send(self, method, params):
            with self._lock:
                stdin, stdout = self._require_pipes()
                self._req_id += 1
                request = transport._jsonrpc_request(method, params, self._req_id)
                stdin.write(request)
                stdin.flush()
                while True:
                    response = stdout.readline()
                    if not response:
                        raise RuntimeError("MCP server closed the stream")
                    if response.startswith(b"{"):
                        self.request_bytes = len(request)
                        self.response_bytes = len(response)
                        return transport._parse_jsonrpc_response(response.decode())

    cache_spec = importlib.util.spec_from_file_location(
        "preference_context", Path(__file__).resolve().parents[1] / "plugins/hermes/preference_context.py"
    )
    cache_module = importlib.util.module_from_spec(cache_spec)
    sys.modules[cache_spec.name] = cache_module
    cache_spec.loader.exec_module(cache_module)

    class ContextCache:
        def __init__(self, connection):
            self.connection = connection
            self.scope = cache_module.PreferenceScope("benchmark", "stdio:" + str(binary))
            self.client = cache_module.RecallContextClient(self._call)

        @property
        def preferences(self):
            return self.client.context.preferences

        def forget(self):
            self.client.context.invalidate("context_reset")

        def _call(self, request):
            result = self.connection._send("tools/call", {"name": "recall_context", "arguments": request})
            assert not result.get("isError"), "recall_context failed"
            self.model_text_bytes = len(transport._extract_text(result).encode("utf-8"))
            return result.get("structuredContent") or json.loads(transport._extract_text(result))

        def recall(self, conditional):
            request = {"queries": [], "include_preferences": True, "record_usage": False}
            delivery = self.client.recall(request, self.scope, reuse=conditional)
            assert delivery is not None
            # This harness owns its simulated context and explicitly delivers each view.
            assert self.client.context.mark_delivered(delivery)
            return delivery.output

    with tempfile.TemporaryDirectory(prefix="noema-preference-revisions-") as temp:
        root = Path(temp)
        os.environ["XDG_CONFIG_HOME"] = str(root / "config")
        for key in ["NOEMA_CORTEX", "TYPESAFE_API_KEY", "JEV_API_TOKEN", "SYSTEM_ONE_API_TOKEN", "NOEMA_CLEF_LOCAL"]:
            os.environ.pop(key, None)

        def cli(arguments, body=None):
            result = subprocess.run([str(binary), *arguments], input=body, text=True, capture_output=True)
            if result.returncode:
                raise RuntimeError("Fixture command failed")

        cli(["init", "--name", "benchmark", "--path", str(root / "cortexes")])
        for index, body in enumerate(bodies):
            cli(["--cortex", "benchmark", "add", "--title", f"Preference {index}", "--type", "preference", "--author", "benchmark", "--tag", "user-preference", "--body", body])
        connection = MeasuredTransport(str(binary), "benchmark")
        cache = ContextCache(connection)
        timings = {"full": [], "conditional": []}
        sizes = {"full": [], "conditional": []}
        request_sizes = {"full": [], "conditional": []}
        text_sizes = {"full": [], "conditional": []}
        try:
            connection.start()
            first = cache.recall(False)
            assert first.get("preferences_revision"), "Server does not support preference revisions"
            expected = cache.preferences
            for block in range(12):
                for arm in (["full", "conditional"] if block % 2 == 0 else ["conditional", "full"]):
                    cache.forget()
                    loaded = cache.recall(arm == "conditional")
                    assert not loaded["preferences_unchanged"] and cache.preferences == expected
                    for _ in range(50):
                        start = time.perf_counter()
                        output = cache.recall(arm == "conditional")
                        timings[arm].append((time.perf_counter() - start) * 1000)
                        sizes[arm].append(connection.response_bytes)
                        request_sizes[arm].append(connection.request_bytes)
                        text_sizes[arm].append(cache.model_text_bytes)
                        assert cache.preferences == expected
                        assert output["preferences_unchanged"] == (arm == "conditional")
            cache.forget()
            restored = cache.recall(True)
            assert not restored["preferences_unchanged"] and cache.preferences == expected
            cli(["--cortex", "benchmark", "add", "--title", "Added preference", "--type", "preference", "--author", "benchmark", "--tag", "user-preference", "--body", "New policy"])
            changed = cache.recall(True)
            assert not changed["preferences_unchanged"]
            assert len(cache.preferences) == len(expected) + 1
            assert cache.recall(True)["preferences_unchanged"]
            for preference in cache.preferences:
                cli(["--cortex", "benchmark", "archive", preference["id"]])
            removed = cache.recall(True)
            assert not removed["preferences_unchanged"] and cache.preferences == []
            assert cache.recall(True)["preferences_unchanged"]
        finally:
            connection.close()
        report = {
            "measurement": "real MCP stdio round trip including request/response JSON and client cache handling; excludes server startup and model evaluation",
            "preferences": len(bodies),
            "samples_per_arm": 600,
            "cached_views_identical": True,
            "context_reset_reload_passed": True,
            "addition_refresh_passed": True,
            "removal_refresh_and_empty_set_reuse_passed": True,
            "arms": {
                arm: {
                    "latency": distribution(timings[arm]),
                    "median_response_bytes": statistics.median(sizes[arm]),
                    "median_request_bytes": statistics.median(request_sizes[arm]),
                    "median_tool_text_bytes": statistics.median(text_sizes[arm]),
                }
                for arm in timings
            },
        }
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(report, indent=2) + "\n")
        print(json.dumps(report))


if __name__ == "__main__":
    main()
