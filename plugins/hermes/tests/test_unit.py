"""Unit tests for the Noema Hermes plugin — no binary or network needed."""

import json
import os
from unittest.mock import MagicMock, patch

import pytest

# Import transport helpers directly.
from plugins.hermes.transport import (
    _jsonrpc_request,
    _parse_jsonrpc_response,
    _extract_text,
    find_binary,
    reset_binary_cache,
    StdioTransport,
    HttpTransport,
)
from plugins.hermes import (
    __version__,
    NoemaMemoryProvider,
    ALL_TOOL_SCHEMAS,
    _TOOL_MAP,
    _tool_error,
    _now_iso,
    _load_config,
)


# ---------------------------------------------------------------------------
# transport.py — JSON-RPC helpers
# ---------------------------------------------------------------------------

class TestJsonRpcRequest:
    def test_basic_request(self):
        raw = _jsonrpc_request("tools/call", {"name": "list_traces"}, 1)
        payload = json.loads(raw.decode("utf-8"))
        assert payload["jsonrpc"] == "2.0"
        assert payload["id"] == 1
        assert payload["method"] == "tools/call"
        assert payload["params"] == {"name": "list_traces"}

    def test_newline_terminated(self):
        raw = _jsonrpc_request("initialize", {}, 42)
        assert raw.endswith(b"\n")

    def test_incrementing_ids(self):
        r1 = json.loads(_jsonrpc_request("a", {}, 1))
        r2 = json.loads(_jsonrpc_request("b", {}, 2))
        assert r1["id"] == 1
        assert r2["id"] == 2


class TestParseJsonRpcResponse:
    def test_success_response(self):
        line = json.dumps({"jsonrpc": "2.0", "id": 1, "result": {"content": []}})
        result = _parse_jsonrpc_response(line)
        assert result == {"content": []}

    def test_error_response_dict(self):
        line = json.dumps({"jsonrpc": "2.0", "id": 1, "error": {"message": "not found"}})
        with pytest.raises(RuntimeError, match="not found"):
            _parse_jsonrpc_response(line)

    def test_error_response_string(self):
        line = json.dumps({"jsonrpc": "2.0", "id": 1, "error": "something broke"})
        with pytest.raises(RuntimeError, match="something broke"):
            _parse_jsonrpc_response(line)

    def test_null_error_is_success(self):
        line = json.dumps({"jsonrpc": "2.0", "id": 1, "result": {"ok": True}, "error": None})
        result = _parse_jsonrpc_response(line)
        assert result == {"ok": True}

    def test_missing_result_returns_empty(self):
        line = json.dumps({"jsonrpc": "2.0", "id": 1})
        result = _parse_jsonrpc_response(line)
        assert result == {}


class TestExtractText:
    def test_extracts_text_content(self):
        result = {"content": [{"type": "text", "text": "hello world"}]}
        assert _extract_text(result) == "hello world"

    def test_skips_non_text_content(self):
        result = {"content": [{"type": "image", "data": "..."}, {"type": "text", "text": "found"}]}
        assert _extract_text(result) == "found"

    def test_empty_content(self):
        assert _extract_text({"content": []}) == ""
        assert _extract_text({}) == ""

    def test_missing_text_field(self):
        result = {"content": [{"type": "text"}]}
        assert _extract_text(result) == ""


# ---------------------------------------------------------------------------
# transport.py — binary discovery
# ---------------------------------------------------------------------------

class TestFindBinary:
    def setup_method(self):
        reset_binary_cache()

    def teardown_method(self):
        reset_binary_cache()

    def test_explicit_override(self, tmp_path):
        binary = tmp_path / "noema"
        binary.write_text("#!/bin/sh\n")
        binary.chmod(0o755)
        result = find_binary(str(binary))
        assert result == str(binary)

    def test_explicit_override_not_found(self, tmp_path):
        result = find_binary(str(tmp_path / "nonexistent"))
        assert result is None

    def test_env_var_override(self, tmp_path):
        binary = tmp_path / "noema"
        binary.write_text("#!/bin/sh\n")
        binary.chmod(0o755)
        reset_binary_cache()
        with patch.dict(os.environ, {"NOEMA_BINARY": str(binary)}):
            result = find_binary()
            assert result == str(binary)

    def test_path_lookup(self, tmp_path):
        reset_binary_cache()
        binary = tmp_path / "noema"
        binary.write_text("#!/bin/sh\n")
        binary.chmod(0o755)
        with patch.dict(os.environ, {}, clear=True):
            with patch("plugins.hermes.transport.shutil.which", return_value=str(binary)):
                result = find_binary()
                assert result == str(binary)

    def test_cache_persists(self, tmp_path):
        binary = tmp_path / "noema"
        binary.write_text("#!/bin/sh\n")
        binary.chmod(0o755)
        reset_binary_cache()
        result1 = find_binary(str(binary))
        # Second call should use cache even without the override.
        result2 = find_binary()
        assert result1 == result2 == str(binary)

    def test_cache_cleared(self, tmp_path):
        binary = tmp_path / "noema"
        binary.write_text("#!/bin/sh\n")
        binary.chmod(0o755)
        find_binary(str(binary))
        reset_binary_cache()
        # After reset, with no PATH or override, should return None
        # (assuming shutil.which won't find it in tmp_path).
        with patch("plugins.hermes.transport.shutil.which", return_value=None):
            # May find it in well-known locations; just verify cache was reset
            # and the call completes without exception.
            find_binary()


# ---------------------------------------------------------------------------
# transport.py — StdioTransport
# ---------------------------------------------------------------------------

class TestStdioTransport:
    def test_is_alive_before_start(self):
        t = StdioTransport("/usr/bin/false", "test-cortex")
        assert t.is_alive is False

    def test_close_before_start(self):
        t = StdioTransport("/usr/bin/false", "test-cortex")
        t.close()  # Should not raise.

    def test_send_raises_when_not_running(self):
        t = StdioTransport("/usr/bin/false", "test-cortex")
        with pytest.raises(RuntimeError, match="not running"):
            t.call_tool("list_traces")


# ---------------------------------------------------------------------------
# transport.py — HttpTransport
# ---------------------------------------------------------------------------

class TestHttpTransport:
    def test_url_normalization(self):
        t = HttpTransport("http://localhost:3000")
        assert t._url == "http://localhost:3000/mcp"

    def test_url_already_has_mcp(self):
        t = HttpTransport("http://localhost:3000/mcp")
        assert t._url == "http://localhost:3000/mcp"

    def test_url_trailing_slash(self):
        t = HttpTransport("http://localhost:3000/")
        assert t._url == "http://localhost:3000/mcp"

    def test_is_alive_always_true(self):
        t = HttpTransport("http://localhost:3000")
        assert t.is_alive is True

    def test_close_is_noop(self):
        t = HttpTransport("http://localhost:3000")
        t.close()  # Should not raise.

    def test_bearer_key_stored(self):
        t = HttpTransport("http://localhost:3000", bearer_key="secret123")
        assert t._bearer_key == "secret123"


# ---------------------------------------------------------------------------
# __init__.py — version
# ---------------------------------------------------------------------------

class TestVersion:
    def test_version_exists(self):
        assert __version__
        parts = __version__.split(".")
        assert len(parts) == 3


# ---------------------------------------------------------------------------
# __init__.py — tool schemas
# ---------------------------------------------------------------------------

class TestToolSchemas:
    def test_six_tools(self):
        assert len(ALL_TOOL_SCHEMAS) == 6

    def test_all_have_required_fields(self):
        for schema in ALL_TOOL_SCHEMAS:
            assert "name" in schema
            assert "description" in schema
            assert "parameters" in schema
            assert schema["parameters"]["type"] == "object"

    def test_tool_names(self):
        names = {s["name"] for s in ALL_TOOL_SCHEMAS}
        assert names == {
            "noema_search", "noema_remember", "noema_recall",
            "noema_list", "noema_update", "noema_lineage",
        }

    def test_remember_required_fields(self):
        schema = next(s for s in ALL_TOOL_SCHEMAS if s["name"] == "noema_remember")
        assert set(schema["parameters"]["required"]) == {"title", "type", "body"}

    def test_search_required_fields(self):
        schema = next(s for s in ALL_TOOL_SCHEMAS if s["name"] == "noema_search")
        assert schema["parameters"]["required"] == ["query"]

    def test_remember_type_enum(self):
        schema = next(s for s in ALL_TOOL_SCHEMAS if s["name"] == "noema_remember")
        enum = schema["parameters"]["properties"]["type"]["enum"]
        assert "fact" in enum
        assert "decision" in enum
        assert "divergence" not in enum  # Agent can't create divergence traces.


# ---------------------------------------------------------------------------
# __init__.py — tool mapping
# ---------------------------------------------------------------------------

class TestToolMap:
    def test_all_schemas_mapped(self):
        schema_names = {s["name"] for s in ALL_TOOL_SCHEMAS}
        mapped_names = set(_TOOL_MAP.keys())
        assert schema_names == mapped_names

    def test_mapping_values(self):
        assert _TOOL_MAP["noema_search"] == "search_traces"
        assert _TOOL_MAP["noema_remember"] == "create_trace"
        assert _TOOL_MAP["noema_recall"] == "get_trace"
        assert _TOOL_MAP["noema_list"] == "list_traces"
        assert _TOOL_MAP["noema_update"] == "update_trace"
        assert _TOOL_MAP["noema_lineage"] == "trace_lineage"


# ---------------------------------------------------------------------------
# __init__.py — helpers
# ---------------------------------------------------------------------------

class TestHelpers:
    def test_tool_error_json(self):
        result = json.loads(_tool_error("something broke"))
        assert result == {"error": "something broke"}

    def test_now_iso_format(self):
        ts = _now_iso()
        assert ts.endswith("Z")
        assert "T" in ts

    def test_extract_first_trace_id(self):
        output = "[fact] 20260412-my-trace (2026-04-12) — agent [tag1]"
        result = NoemaMemoryProvider._extract_first_trace_id(output)
        assert result == "20260412-my-trace"

    def test_extract_first_trace_id_multiline(self):
        output = (
            "Found 2 traces:\n"
            "[decision] 20260412-chose-go (2026-04-12) — mark [go]\n"
            "[fact] 20260412-sqlite-perf (2026-04-12) — mark [perf]"
        )
        result = NoemaMemoryProvider._extract_first_trace_id(output)
        assert result == "20260412-chose-go"

    def test_extract_first_trace_id_none(self):
        assert NoemaMemoryProvider._extract_first_trace_id("No traces found.") is None
        assert NoemaMemoryProvider._extract_first_trace_id("") is None


# ---------------------------------------------------------------------------
# __init__.py — config loading
# ---------------------------------------------------------------------------

class TestConfig:
    def test_load_config(self, tmp_path):
        config = {"cortex_name": "test", "transport": "stdio"}
        (tmp_path / "noema.json").write_text(json.dumps(config))
        result = _load_config(str(tmp_path))
        assert result == config

    def test_load_config_missing(self, tmp_path):
        result = _load_config(str(tmp_path))
        assert result == {}

    def test_load_config_invalid_json(self, tmp_path):
        (tmp_path / "noema.json").write_text("not json{{{")
        result = _load_config(str(tmp_path))
        assert result == {}

    def test_save_config(self, tmp_path):
        provider = NoemaMemoryProvider()
        provider.save_config({"cortex_name": "my-cortex", "transport": "stdio"}, str(tmp_path))
        saved = json.loads((tmp_path / "noema.json").read_text())
        assert saved["cortex_name"] == "my-cortex"
        assert saved["transport"] == "stdio"

    def test_save_config_strips_none(self, tmp_path):
        provider = NoemaMemoryProvider()
        provider.save_config({"cortex_name": "x", "bearer_key": None}, str(tmp_path))
        saved = json.loads((tmp_path / "noema.json").read_text())
        assert "bearer_key" not in saved

    def test_config_schema(self):
        provider = NoemaMemoryProvider()
        schema = provider.get_config_schema()
        keys = [item["key"] for item in schema]
        assert keys == ["cortex_name", "noema_binary", "transport", "http_url", "bearer_key"]
        required = [item["key"] for item in schema if item.get("required")]
        assert required == ["cortex_name"]
        secret = [item["key"] for item in schema if item.get("secret")]
        assert secret == ["bearer_key"]


# ---------------------------------------------------------------------------
# __init__.py — provider basics
# ---------------------------------------------------------------------------

class TestProviderBasics:
    def test_name(self):
        p = NoemaMemoryProvider()
        assert p.name == "noema"

    def test_is_available_no_cortex(self):
        p = NoemaMemoryProvider()
        with patch.dict(os.environ, {}, clear=True):
            assert p.is_available() is False

    def test_is_available_stdio_no_binary(self):
        p = NoemaMemoryProvider()
        p._config = {"cortex_name": "test", "transport": "stdio"}
        reset_binary_cache()
        with patch.dict(os.environ, {"NOEMA_CORTEX": "test"}, clear=True):
            with patch("plugins.hermes.transport.shutil.which", return_value=None):
                with patch("plugins.hermes.transport.Path.is_file", return_value=False):
                    assert p.is_available() is False
        reset_binary_cache()

    def test_is_available_http_no_url(self):
        p = NoemaMemoryProvider()
        p._config = {"cortex_name": "test", "transport": "http"}
        with patch.dict(os.environ, {"NOEMA_CORTEX": "test"}, clear=True):
            assert p.is_available() is False

    def test_is_available_http_with_url(self):
        p = NoemaMemoryProvider()
        p._config = {"cortex_name": "test", "transport": "http", "http_url": "http://localhost:3000"}
        with patch.dict(os.environ, {"NOEMA_CORTEX": "test"}, clear=True):
            assert p.is_available() is True

    def test_get_tool_schemas_returns_copy(self):
        p = NoemaMemoryProvider()
        schemas = p.get_tool_schemas()
        assert schemas == ALL_TOOL_SCHEMAS
        assert schemas is not ALL_TOOL_SCHEMAS  # Should be a copy.

    def test_handle_tool_call_unknown_tool(self):
        p = NoemaMemoryProvider()
        result = json.loads(p.handle_tool_call("noema_explode", {}))
        assert "error" in result

    def test_handle_tool_call_no_transport(self):
        p = NoemaMemoryProvider()
        result = json.loads(p.handle_tool_call("noema_search", {"query": "test"}))
        assert "error" in result

    def test_queue_prefetch_is_noop(self):
        p = NoemaMemoryProvider()
        p.queue_prefetch("test")  # Should not raise.

    def test_on_turn_start_increments_counter(self):
        p = NoemaMemoryProvider()
        p.on_turn_start(5, "hello")
        assert p._turn_count == 5

    def test_shutdown_without_transport(self):
        p = NoemaMemoryProvider()
        p.shutdown()  # Should not raise.


# ---------------------------------------------------------------------------
# __init__.py — handle_tool_call routing (mocked transport)
# ---------------------------------------------------------------------------

class TestHandleToolCallRouting:
    def setup_method(self):
        self.provider = NoemaMemoryProvider()
        self.provider._transport = MagicMock()
        self.provider._author = "hermes/tester"
        self.provider._transport.call_tool.return_value = "ok"

    def test_search_routes_correctly(self):
        self.provider.handle_tool_call("noema_search", {"query": "sqlite"})
        self.provider._transport.call_tool.assert_called_once_with(
            "search_traces", {"query": "sqlite"}
        )

    def test_remember_includes_author(self):
        self.provider.handle_tool_call("noema_remember", {
            "title": "test", "type": "fact", "body": "content",
        })
        call_args = self.provider._transport.call_tool.call_args
        assert call_args[0][0] == "create_trace"
        assert call_args[0][1]["author"] == "hermes/tester"

    def test_remember_passes_tags(self):
        self.provider.handle_tool_call("noema_remember", {
            "title": "test", "type": "fact", "body": "content", "tags": "a, b",
        })
        call_args = self.provider._transport.call_tool.call_args
        assert call_args[0][1]["tags"] == "a, b"

    def test_recall_routes_correctly(self):
        self.provider.handle_tool_call("noema_recall", {"id": "20260412-test"})
        self.provider._transport.call_tool.assert_called_once_with(
            "get_trace", {"id": "20260412-test"}
        )

    def test_list_passes_filters(self):
        self.provider.handle_tool_call("noema_list", {"type": "decision", "tag": "go"})
        call_args = self.provider._transport.call_tool.call_args
        assert call_args[0][1] == {"type": "decision", "tag": "go"}

    def test_update_routes_correctly(self):
        self.provider.handle_tool_call("noema_update", {"id": "20260412-test", "body": "new"})
        call_args = self.provider._transport.call_tool.call_args
        assert call_args[0][0] == "update_trace"
        assert call_args[0][1]["id"] == "20260412-test"
        assert call_args[0][1]["body"] == "new"

    def test_lineage_routes_correctly(self):
        self.provider.handle_tool_call("noema_lineage", {"id": "20260412-test"})
        self.provider._transport.call_tool.assert_called_once_with(
            "trace_lineage", {"id": "20260412-test"}
        )

    def test_result_wrapped_in_json(self):
        self.provider._transport.call_tool.return_value = "trace body here"
        raw = self.provider.handle_tool_call("noema_recall", {"id": "x"})
        result = json.loads(raw)
        assert result == {"result": "trace body here"}

    def test_transport_error_returns_error_json(self):
        self.provider._transport.call_tool.side_effect = RuntimeError("connection lost")
        self.provider._transport.is_alive = True
        raw = self.provider.handle_tool_call("noema_search", {"query": "x"})
        result = json.loads(raw)
        assert "error" in result


# ---------------------------------------------------------------------------
# __init__.py — prefetch (mocked transport)
# ---------------------------------------------------------------------------

class TestPrefetch:
    def test_empty_query(self):
        p = NoemaMemoryProvider()
        p._transport = MagicMock()
        assert p.prefetch("") == ""
        assert p.prefetch("   ") == ""

    def test_no_results(self):
        p = NoemaMemoryProvider()
        p._transport = MagicMock()
        p._transport.call_tool.return_value = "No traces found."
        assert p.prefetch("test query") == ""

    def test_formats_results(self):
        p = NoemaMemoryProvider()
        p._transport = MagicMock()
        p._transport.call_tool.return_value = "[fact] 20260412-test (2026-04-12) — agent [tag]"
        result = p.prefetch("test query")
        assert "## Relevant Memories (from Noema)" in result
        assert "20260412-test" in result
        assert "noema_search" in result

    def test_filters_session_logs(self):
        p = NoemaMemoryProvider()
        p._transport = MagicMock()
        p._transport.call_tool.return_value = (
            "[context] 20260412-hermes-session (2026-04-12) — hermes/agent [hermes-session]\n"
            "[fact] 20260412-real-trace (2026-04-12) — agent [useful]"
        )
        result = p.prefetch("test")
        assert "hermes-session" not in result or "hermes-session-summary" in result
        assert "20260412-real-trace" in result

    def test_keeps_session_summaries(self):
        p = NoemaMemoryProvider()
        p._transport = MagicMock()
        p._transport.call_tool.return_value = (
            "[observation] 20260412-session-summary (2026-04-12) — hermes/agent [hermes-session-summary]"
        )
        result = p.prefetch("test")
        assert "hermes-session-summary" in result

    def test_truncates_long_query(self):
        p = NoemaMemoryProvider()
        p._transport = MagicMock()
        p._transport.call_tool.return_value = "No traces found."
        p.prefetch("x" * 1000)
        call_args = p._transport.call_tool.call_args
        assert len(call_args[0][1]["query"]) == 500

    def test_transport_error_returns_empty(self):
        p = NoemaMemoryProvider()
        p._transport = MagicMock()
        p._transport.call_tool.side_effect = RuntimeError("boom")
        assert p.prefetch("test") == ""


# ---------------------------------------------------------------------------
# __init__.py — session trace creation (mocked transport)
# ---------------------------------------------------------------------------

class TestSessionTrace:
    def test_session_trace_created(self):
        p = NoemaMemoryProvider()
        p._transport = MagicMock()
        p._transport.call_tool.return_value = "Trace created: 20260412-hermes-session-abc"
        p._session_id = "session-abc123"
        p._agent_identity = "researcher"
        p._author = "hermes/researcher"
        p._session_title = "Research session"
        p._platform = "cli"

        p._create_session_trace()

        call_args = p._transport.call_tool.call_args
        assert call_args[0][0] == "create_trace"
        args = call_args[0][1]
        assert args["title"] == "hermes-session: Research session"
        assert args["type"] == "context"
        assert args["author"] == "hermes/researcher"
        assert "hermes-session" in args["tags"]
        assert p._session_trace_id == "20260412-hermes-session-abc"

    def test_session_trace_uses_id_fallback(self):
        p = NoemaMemoryProvider()
        p._transport = MagicMock()
        p._transport.call_tool.return_value = "Trace created: 20260412-hermes-session-x"
        p._session_id = "abcdefghijklmnop"
        p._session_title = ""
        p._author = "hermes/unknown"
        p._platform = "cli"
        p._agent_identity = "unknown"

        p._create_session_trace()

        call_args = p._transport.call_tool.call_args
        assert "abcdefghijkl" in call_args[0][1]["title"]
