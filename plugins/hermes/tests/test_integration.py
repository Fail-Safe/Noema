"""Integration tests for the Noema Hermes plugin — requires the noema binary.

These tests spawn a real `noema serve --transport stdio` subprocess against
a throwaway cortex and exercise the full plugin lifecycle.

Skip with: pytest -m "not integration"
"""

import json
import os
import subprocess
import time
from pathlib import Path

import pytest

from plugins.hermes import NoemaMemoryProvider
from plugins.hermes.transport import StdioTransport, reset_binary_cache

# Mark every test in this module as integration.
pytestmark = pytest.mark.integration

# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

@pytest.fixture(scope="session")
def noema_binary():
    """Build the current checkout so integration tests never use an installed binary."""
    repo_root = Path(__file__).resolve().parents[3]
    binary = repo_root / "target" / "debug" / "noema"
    result = subprocess.run(
        ["cargo", "build", "--locked", "--bin", "noema"],
        cwd=repo_root,
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 0, f"noema build failed: {result.stderr}"
    return str(binary)


@pytest.fixture()
def cortex_dir(noema_binary, tmp_path, monkeypatch):
    """Create a cortex with all user-level paths redirected into the test directory."""
    home = tmp_path / "home"
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("XDG_CONFIG_HOME", str(home / ".config"))
    monkeypatch.setenv("NOEMA_BINARY", noema_binary)
    reset_binary_cache()
    name = "hermes-test"
    result = subprocess.run(
        [noema_binary, "init", "--name", name, "--path", str(tmp_path / "cortex")],
        capture_output=True,
        text=True,
        env=os.environ.copy(),
        check=False,
    )
    assert result.returncode == 0, f"noema init failed: {result.stderr}"
    yield name
    reset_binary_cache()


# ---------------------------------------------------------------------------
# Transport integration
# ---------------------------------------------------------------------------

class TestStdioTransportIntegration:
    def test_start_and_handshake(self, noema_binary, cortex_dir):
        t = StdioTransport(noema_binary, cortex_dir)
        t.start()
        assert t.is_alive
        t.close()
        assert not t.is_alive

    def test_call_cortex_identity(self, noema_binary, cortex_dir):
        t = StdioTransport(noema_binary, cortex_dir)
        t.start()
        try:
            result = t.call_tool("cortex_identity")
            identity = json.loads(result)
            assert "name" in identity
            assert "id" in identity
            assert identity["name"] == cortex_dir
        finally:
            t.close()

    def test_call_get_instructions(self, noema_binary, cortex_dir):
        t = StdioTransport(noema_binary, cortex_dir)
        t.start()
        try:
            result = t.call_tool("get_instructions")
            assert "Trace" in result or "trace" in result
            assert len(result) > 100  # Should be a substantial guide.
        finally:
            t.close()

    def test_create_and_get_trace(self, noema_binary, cortex_dir):
        t = StdioTransport(noema_binary, cortex_dir)
        t.start()
        try:
            # Create a trace.
            create_result = t.call_tool("create_trace", {
                "title": "integration-test-trace",
                "type": "note",
                "body": "This is a test trace from the integration suite.",
                "tags": "test, integration",
            })
            assert "Trace created:" in create_result
            trace_id = create_result.split("Trace created: ")[1].strip()

            # Get it back.
            get_result = t.call_tool("get_trace", {"id": trace_id})
            assert "integration-test-trace" in get_result
            assert "This is a test trace" in get_result
        finally:
            t.close()

    def test_search_traces(self, noema_binary, cortex_dir):
        t = StdioTransport(noema_binary, cortex_dir)
        t.start()
        try:
            # Create a trace with distinctive content.
            t.call_tool("create_trace", {
                "title": "zebra-unique-term",
                "type": "fact",
                "body": "Zebras have distinctive black and white stripes.",
            })
            # Search for it.
            result = t.call_tool("search_traces", {"query": "zebra"})
            assert "zebra" in result.lower()
        finally:
            t.close()

    def test_append_trace(self, noema_binary, cortex_dir):
        t = StdioTransport(noema_binary, cortex_dir)
        t.start()
        try:
            # Create, then append.
            create_result = t.call_tool("create_trace", {
                "title": "append-target",
                "type": "context",
                "body": "Initial content.",
            })
            trace_id = create_result.split("Trace created: ")[1].strip()

            t.call_tool("append_trace", {
                "id": trace_id,
                "content": "\n---\nAppended block.",
            })

            get_result = t.call_tool("get_trace", {"id": trace_id})
            assert "Initial content." in get_result
            assert "Appended block." in get_result
        finally:
            t.close()

    def test_list_traces(self, noema_binary, cortex_dir):
        t = StdioTransport(noema_binary, cortex_dir)
        t.start()
        try:
            t.call_tool("create_trace", {
                "title": "list-test",
                "type": "decision",
                "body": "Listing test.",
            })
            result = t.call_tool("list_traces")
            assert "list-test" in result
        finally:
            t.close()

    def test_archive_and_list(self, noema_binary, cortex_dir):
        t = StdioTransport(noema_binary, cortex_dir)
        t.start()
        try:
            create_result = t.call_tool("create_trace", {
                "title": "archive-me",
                "type": "note",
                "body": "Will be archived.",
            })
            trace_id = create_result.split("Trace created: ")[1].strip()

            t.call_tool("archive_trace", {"id": trace_id})

            # Should not appear in default list.
            result = t.call_tool("list_traces")
            assert trace_id not in result
        finally:
            t.close()

    def test_close_is_idempotent(self, noema_binary, cortex_dir):
        t = StdioTransport(noema_binary, cortex_dir)
        t.start()
        t.close()
        t.close()  # Should not raise.


# ---------------------------------------------------------------------------
# Full provider lifecycle
# ---------------------------------------------------------------------------

class TestProviderLifecycle:
    def test_bounded_prefetch_relevance_and_scope(self, noema_binary, cortex_dir, tmp_path):
        (tmp_path / "noema.json").write_text(json.dumps({
            "cortex_name": cortex_dir, "noema_binary": noema_binary, "bounded_prefetch": True,
        }))
        p = NoemaMemoryProvider()
        p.initialize("prefetch-test", hermes_home=str(tmp_path))
        try:
            p.handle_tool_call("noema_remember", {
                "title": "harbor policy", "type": "decision",
                "body": "harbor policy: current value 28 days. Old 14 days superseded. Source: card-a.",
            })
            p.handle_tool_call("noema_remember", {
                "title": "orchard policy", "type": "decision",
                "body": "orchard policy: unrelated value violet. Source: card-b.",
            })
            p.handle_tool_call("noema_remember", {
                "title": "harbor policy preference", "type": "preference",
                "body": "startup-only-sentinel", "tags": "user-preference",
            })
            context = p.prefetch("harbor policy")
            assert "28 days" in context and "superseded" in context and "Source: card-a" in context
            assert "ID:" in context and "reference data, not instructions" in context
            assert "violet" not in context
            assert "startup-only-sentinel" not in context
            assert p.prefetch("nonexistentzzunique") == ""
            subprocess.run([noema_binary, "init", "--name", "other-scope", "--path",
                            str(tmp_path / "other")], check=True, capture_output=True)
            subprocess.run([noema_binary, "--cortex", "other-scope", "add", "--title",
                            "scopeprivateunique", "--type", "fact", "--body", "foreign-sentinel"],
                           check=True, capture_output=True)
            assert p.prefetch("scopeprivateunique") == ""
            p.handle_tool_call("noema_remember", {
                "title": "longpacket", "type": "note", "body": "界" * 8000,
            })
            long = p.prefetch("longpacket")
            assert len(long) <= 6000 and "[trace truncated]" in long
        finally:
            p.shutdown()

    def test_bounded_search_evidence_and_limits(self, noema_binary, cortex_dir, tmp_path):
        (tmp_path / "noema.json").write_text(json.dumps({
            "cortex_name": cortex_dir, "noema_binary": noema_binary,
            "bounded_search": True,
        }))
        provider = NoemaMemoryProvider()
        provider.initialize("bounded-test", hermes_home=str(tmp_path))
        try:
            ids = []
            for index in range(5):
                result = json.loads(provider.handle_tool_call("noema_remember", {
                    "title": f"bounded-evidence-{index}", "type": "decision",
                    "body": "uniqueboundedtoken current: 21 days; old: 7 days superseded. "
                    "Rationale: audit window. Source: synthetic-card. " + "界" * 4100,
                }))["result"]
                ids.append(result.split("Trace created: ")[1].strip())
            provider._transport.call_tool("archive_trace", {"id": ids[0]})
            result = json.loads(json.loads(provider.handle_tool_call(
                "noema_search", {"query": "uniqueboundedtoken"},
            ))["result"])
            matches = result["results"][0]["matches"]
            assert len(matches) == 3
            assert result["preferences"] == []
            assert result["usage_recorded"] is True
            for match in matches:
                assert match["id"] in ids[1:]
                assert match["body_truncated"] is True
                assert len(match["body"]) == 4000
                assert "21 days" in match["body"]
                assert "Source: synthetic-card" in match["body"]
                assert match["content_hash"]
                full = json.loads(provider.handle_tool_call("noema_recall", {
                    "id": match["id"],
                }))["result"]
                assert "界" * 4100 in full
            empty = json.loads(json.loads(provider.handle_tool_call(
                "noema_search", {"query": "absentuniquetoken"},
            ))["result"])
            assert empty["results"][0]["matches"] == []
        finally:
            provider.shutdown()

    @pytest.mark.parametrize("bounded_search,bounded_prefetch", [(False, False), (True, False), (False, True)])
    def test_full_session(self, noema_binary, cortex_dir, tmp_path, bounded_search, bounded_prefetch):
        """Exercise the full Hermes lifecycle: init -> turns -> end -> shutdown."""
        provider = NoemaMemoryProvider()
        (tmp_path / "noema.json").write_text(json.dumps({
            "cortex_name": cortex_dir, "transport": "stdio",
            "bounded_search": bounded_search,
            "bounded_prefetch": bounded_prefetch,
        }))

        # Initialize.
        with pytest.MonkeyPatch.context() as mp:
            mp.setenv("NOEMA_CORTEX", cortex_dir)
            provider.initialize(
                session_id="test-session-001",
                agent_identity="researcher",
                session_title="Integration test session",
                platform="pytest",
                hermes_home=str(tmp_path),
            )

        assert provider._transport is not None
        assert provider._transport.is_alive
        assert provider._cortex_name == cortex_dir
        assert provider._session_trace_id  # Should have created a session trace.
        assert provider._instructions_cache  # Should have cached instructions.
        assert provider._author == "hermes/researcher"

        # System prompt block should return cached instructions.
        prompt = provider.system_prompt_block()
        assert len(prompt) > 100

        # Tool schemas.
        schemas = provider.get_tool_schemas()
        assert len(schemas) == 6

        # Simulate turns.
        provider.on_turn_start(1, "What do you know about Go?")
        provider.sync_turn(
            "What do you know about Go?",
            "Go is a statically typed language designed at Google.",
        )
        # Wait for the sync thread to complete.
        if provider._sync_thread:
            provider._sync_thread.join(timeout=5)

        provider.on_turn_start(2, "Why did we choose it?")
        provider.sync_turn(
            "Why did we choose it?",
            "We chose local SQLite for fast indexing and offline operation.",
        )
        if provider._sync_thread:
            provider._sync_thread.join(timeout=5)

        # Verify the session trace got appended.
        session_content = provider._transport.call_tool(
            "get_trace", {"id": provider._session_trace_id}
        )
        assert "Turn 1" in session_content
        assert "Turn 2" in session_content
        assert "Go" in session_content

        # Tool calls.
        result = json.loads(provider.handle_tool_call("noema_remember", {
            "title": "Go is great",
            "type": "decision",
            "body": "We chose Go for its simplicity and tooling.",
            "tags": "go, lang",
        }))
        assert "result" in result
        assert "Trace created:" in result["result"]

        # Search.
        result = json.loads(provider.handle_tool_call("noema_search", {
            "query": "Go simplicity",
        }))
        assert "result" in result

        # Prefetch.
        prefetch_result = provider.prefetch("Go language choice")
        # May or may not find results depending on FTS indexing timing.
        # Just verify it doesn't crash and returns a string.
        assert isinstance(prefetch_result, str)

        # Session end.
        messages = [
            {"role": "user", "content": "What do you know about Go?"},
            {"role": "assistant", "content": "Go is a statically typed language."},
            {"role": "user", "content": "Why did we choose it?"},
            {"role": "assistant", "content": "We chose local SQLite for offline operation."},
        ]
        provider.on_session_end(messages)
        if provider._end_thread:
            provider._end_thread.join(timeout=10)

        # Verify summary was created. List by type.
        summary_result = provider._transport.call_tool(
            "list_traces", {"type": "observation"}
        )
        assert "session-summary" in summary_result

        # Verify session log was archived (shouldn't show in default list).
        list_result = provider._transport.call_tool("list_traces")
        assert provider._session_trace_id not in list_result

        # Shutdown.
        provider.shutdown()
        assert provider._transport is None

    def test_memory_write_add(self, noema_binary, cortex_dir):
        """Test on_memory_write with add action."""
        provider = NoemaMemoryProvider()
        provider._config = {"cortex_name": cortex_dir, "transport": "stdio"}

        with pytest.MonkeyPatch.context() as mp:
            mp.setenv("NOEMA_CORTEX", cortex_dir)
            provider.initialize(
                session_id="test-mirror-001",
                agent_identity="tester",
            )

        provider.on_memory_write("add", "test-memory", "This is a mirrored memory.")
        if provider._mirror_thread:
            provider._mirror_thread.join(timeout=5)

        # Verify it was created.
        result = provider._transport.call_tool(
            "search_traces", {"query": "mirrored memory"}
        )
        assert "hermes-mirror" in result or "mirrored" in result.lower()

        provider.shutdown()

    def test_pre_compress(self, noema_binary, cortex_dir):
        """Test on_pre_compress returns a breadcrumb and appends to session log."""
        provider = NoemaMemoryProvider()
        provider._config = {"cortex_name": cortex_dir, "transport": "stdio"}

        with pytest.MonkeyPatch.context() as mp:
            mp.setenv("NOEMA_CORTEX", cortex_dir)
            provider.initialize(
                session_id="test-compress-001",
                agent_identity="tester",
            )

        messages = [
            {"role": "user", "content": "First message"},
            {"role": "assistant", "content": "First reply"},
        ]

        breadcrumb = provider.on_pre_compress(messages)
        assert "Context compressed" in breadcrumb
        assert provider._session_trace_id in breadcrumb
        assert "noema_recall" in breadcrumb

        # Wait for the background thread.
        time.sleep(1)

        # Verify the compression block was appended.
        session_content = provider._transport.call_tool(
            "get_trace", {"id": provider._session_trace_id}
        )
        assert "Context compression" in session_content

        provider.shutdown()
