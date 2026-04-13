"""Integration tests for the Noema Hermes plugin — requires the noema binary.

These tests spawn a real `noema serve --transport stdio` subprocess against
a throwaway cortex and exercise the full plugin lifecycle.

Skip with: pytest -m "not integration"
"""

import json
import os
import shutil
import subprocess
import tempfile
import time

import pytest

from plugins.hermes.transport import find_binary, reset_binary_cache, StdioTransport
from plugins.hermes import NoemaMemoryProvider

# Mark every test in this module as integration.
pytestmark = pytest.mark.integration

# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

NOEMA_BINARY = None


def _find_noema():
    global NOEMA_BINARY
    if NOEMA_BINARY is None:
        reset_binary_cache()
        NOEMA_BINARY = find_binary()
    return NOEMA_BINARY


@pytest.fixture(scope="module")
def noema_binary():
    binary = _find_noema()
    if not binary:
        pytest.skip("noema binary not found — skipping integration tests")
    return binary


@pytest.fixture()
def cortex_dir(noema_binary):
    """Create a throwaway cortex and return its path. Cleaned up after the test."""
    tmpdir = tempfile.mkdtemp(prefix="noema-test-")
    name = "hermes-test"
    result = subprocess.run(
        [noema_binary, "init", "--name", name, "--path", tmpdir],
        capture_output=True, text=True,
    )
    assert result.returncode == 0, f"noema init failed: {result.stderr}"
    yield name
    shutil.rmtree(tmpdir, ignore_errors=True)


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
    def test_full_session(self, noema_binary, cortex_dir):
        """Exercise the full Hermes lifecycle: init -> turns -> end -> shutdown."""
        provider = NoemaMemoryProvider()
        provider._config = {"cortex_name": cortex_dir, "transport": "stdio"}

        # Initialize.
        with pytest.MonkeyPatch.context() as mp:
            mp.setenv("NOEMA_CORTEX", cortex_dir)
            provider.initialize(
                session_id="test-session-001",
                agent_identity="researcher",
                session_title="Integration test session",
                platform="pytest",
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
            "We chose Go for pure-Go SQLite and fast iteration.",
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
            {"role": "assistant", "content": "We chose Go for pure-Go SQLite."},
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
