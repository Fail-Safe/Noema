"""Noema memory provider plugin for Hermes.

Gives any Hermes agent structured, persistent memory backed by a Noema Cortex.
Traces (markdown files with typed frontmatter) are searched, created, and
appended through the standard Hermes lifecycle hooks.

Usage:
    1. Copy this directory into <hermes>/plugins/memory/noema/
    2. Run `hermes memory setup` and select noema
    3. Set NOEMA_CORTEX=<cortex-name> (or configure via the setup wizard)

See README.md for full setup instructions.
"""

__version__ = "0.1.0"

import json
import logging
import os
import threading
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, List, Optional

try:
    from agent.memory_provider import MemoryProvider
except ImportError:
    # Allow importing outside of Hermes for testing.
    class MemoryProvider:  # type: ignore[no-redef]
        pass

from .transport import (
    HttpTransport,
    StdioTransport,
    find_binary,
    reset_binary_cache as reset_binary_cache,
)

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Tool schemas (OpenAI function-calling format)
# ---------------------------------------------------------------------------

SEARCH_SCHEMA = {
    "name": "noema_search",
    "description": (
        "Search your memory for relevant traces. Returns matching memories "
        "ranked by relevance (FTS5 full-text search)."
    ),
    "parameters": {
        "type": "object",
        "properties": {
            "query": {
                "type": "string",
                "description": "Search query — keywords or natural phrases.",
            },
        },
        "required": ["query"],
    },
}

REMEMBER_SCHEMA = {
    "name": "noema_remember",
    "description": (
        "Create a new memory trace. Choose a type that reflects the intent: "
        "fact, decision, preference, context, skill, intent, observation, or note."
    ),
    "parameters": {
        "type": "object",
        "properties": {
            "title": {
                "type": "string",
                "description": (
                    "Short descriptive title. Do NOT include a date — "
                    "the ID generator prepends today's date automatically."
                ),
            },
            "type": {
                "type": "string",
                "description": "Memory type.",
                "enum": [
                    "fact", "decision", "preference", "context",
                    "skill", "intent", "observation", "note",
                ],
            },
            "body": {
                "type": "string",
                "description": "Full content of the memory (markdown).",
            },
            "tags": {
                "type": "string",
                "description": "Comma-separated keyword tags.",
            },
            "derived_from": {
                "type": "string",
                "description": "Comma-separated trace IDs this memory was derived from.",
            },
        },
        "required": ["title", "type", "body"],
    },
}

RECALL_SCHEMA = {
    "name": "noema_recall",
    "description": "Read a specific memory trace by ID, including its full body.",
    "parameters": {
        "type": "object",
        "properties": {
            "id": {
                "type": "string",
                "description": "Trace ID (e.g. 20260412-my-trace).",
            },
        },
        "required": ["id"],
    },
}

LIST_SCHEMA = {
    "name": "noema_list",
    "description": "List memory traces, optionally filtered by type, author, tag, or origin.",
    "parameters": {
        "type": "object",
        "properties": {
            "type": {"type": "string", "description": "Filter by trace type."},
            "author": {"type": "string", "description": "Filter by author."},
            "tag": {"type": "string", "description": "Filter by tag."},
            "origin": {"type": "string", "description": "Filter by origin cortex."},
        },
        "required": [],
    },
}

UPDATE_SCHEMA = {
    "name": "noema_update",
    "description": "Update fields of an existing memory trace. Only provided fields are changed.",
    "parameters": {
        "type": "object",
        "properties": {
            "id": {"type": "string", "description": "Trace ID to update."},
            "title": {"type": "string", "description": "New title."},
            "type": {
                "type": "string",
                "description": "New type.",
                "enum": [
                    "fact", "decision", "preference", "context",
                    "skill", "intent", "observation", "note",
                ],
            },
            "body": {"type": "string", "description": "New body content."},
            "tags": {"type": "string", "description": "New tags (comma-separated, replaces existing)."},
        },
        "required": ["id"],
    },
}

LINEAGE_SCHEMA = {
    "name": "noema_lineage",
    "description": "Show the derivation graph for a trace: what it was derived from and what derives from it.",
    "parameters": {
        "type": "object",
        "properties": {
            "id": {"type": "string", "description": "Trace ID."},
        },
        "required": ["id"],
    },
}

ALL_TOOL_SCHEMAS = [
    SEARCH_SCHEMA, REMEMBER_SCHEMA, RECALL_SCHEMA,
    LIST_SCHEMA, UPDATE_SCHEMA, LINEAGE_SCHEMA,
]

# Map Hermes tool names to Noema MCP tool names.
_TOOL_MAP = {
    "noema_search": "search_traces",
    "noema_remember": "create_trace",
    "noema_recall": "get_trace",
    "noema_list": "list_traces",
    "noema_update": "update_trace",
    "noema_lineage": "trace_lineage",
}

# Max chars to keep from user/assistant content in the session log.
_TURN_MAX_CHARS = 2000


def _tool_error(msg: str) -> str:
    return json.dumps({"error": msg})


def _now_iso() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


# ---------------------------------------------------------------------------
# Config loading
# ---------------------------------------------------------------------------

def _load_config(hermes_home: str) -> dict:
    """Load noema.json from the Hermes home directory."""
    config_path = Path(hermes_home) / "noema.json"
    if config_path.exists():
        try:
            return json.loads(config_path.read_text(encoding="utf-8"))
        except Exception:
            logger.debug("Failed to parse %s", config_path, exc_info=True)
    return {}


def _load_config_from_hermes_home() -> dict:
    """Locate and load noema.json without an explicit hermes_home path.

    Resolution: HERMES_HOME env -> ~/.hermes (default).
    Called by is_available() which runs before initialize() provides hermes_home.
    """
    try:
        hermes_home = os.environ.get("HERMES_HOME", "")
        if not hermes_home:
            hermes_home = str(Path.home() / ".hermes")
        return _load_config(hermes_home)
    except Exception:
        return {}


# ---------------------------------------------------------------------------
# NoemaMemoryProvider
# ---------------------------------------------------------------------------

class NoemaMemoryProvider(MemoryProvider):
    """Hermes memory provider backed by a Noema Cortex."""

    def __init__(self):
        self._config: dict = {}
        self._transport = None
        self._cortex_name: str = ""
        self._cortex_id: str = ""
        self._instructions_cache: str = ""
        self._session_id: str = ""
        self._session_trace_id: str = ""
        self._agent_identity: str = "unknown"
        self._session_title: str = ""
        self._platform: str = ""
        self._turn_count: int = 0
        self._author: str = "hermes/unknown"

        # Thread tracking for non-blocking hooks.
        self._sync_thread: Optional[threading.Thread] = None
        self._end_thread: Optional[threading.Thread] = None
        self._mirror_thread: Optional[threading.Thread] = None

    # ---- Core required methods ----

    @property
    def name(self) -> str:
        return "noema"

    def is_available(self) -> bool:
        """Check if the plugin can activate — no network calls."""
        # Load config eagerly if not yet loaded (is_available is called
        # before initialize, so self._config may be empty).
        config = self._config
        if not config:
            config = _load_config_from_hermes_home()
        cortex = os.environ.get("NOEMA_CORTEX", "")
        if not cortex:
            cortex = config.get("cortex_name", "")
        if not cortex:
            return False
        transport = config.get("transport", "stdio")
        if transport == "stdio":
            return find_binary(config.get("noema_binary")) is not None
        elif transport == "http":
            return bool(
                config.get("http_url") or os.environ.get("NOEMA_HTTP_URL")
            )
        return False

    def initialize(self, session_id: str, **kwargs) -> None:
        hermes_home = kwargs.get("hermes_home", "")
        self._config = _load_config(hermes_home) if hermes_home else {}
        self._session_id = session_id
        self._agent_identity = kwargs.get("agent_identity", "unknown")
        self._session_title = kwargs.get("session_title", "")
        self._platform = kwargs.get("platform", "cli")
        self._author = f"hermes/{self._agent_identity}"

        self._cortex_name = (
            os.environ.get("NOEMA_CORTEX", "")
            or self._config.get("cortex_name", "")
        )

        transport_mode = self._config.get("transport", "stdio")
        if transport_mode == "http":
            url = (
                self._config.get("http_url")
                or os.environ.get("NOEMA_HTTP_URL", "")
            )
            key = (
                os.environ.get("NOEMA_MCP_KEY", "")
                or self._config.get("bearer_key", "")
            )
            self._transport = HttpTransport(url, bearer_key=key or None)
        else:
            binary = find_binary(self._config.get("noema_binary"))
            if not binary:
                raise RuntimeError("noema binary not found")
            self._transport = StdioTransport(binary, self._cortex_name)

        self._transport.start()

        # Learn cortex identity.
        try:
            identity_text = self._transport.call_tool("cortex_identity")
            identity = json.loads(identity_text)
            self._cortex_name = identity.get("name", self._cortex_name)
            self._cortex_id = identity.get("id", "")
        except Exception as e:
            logger.warning("Failed to get cortex identity: %s", e)

        # Cache instructions for system_prompt_block.
        try:
            self._instructions_cache = self._transport.call_tool("get_instructions")
        except Exception as e:
            logger.warning("Failed to get instructions: %s", e)

        # Create the session log trace.
        self._create_session_trace()

    def get_tool_schemas(self) -> List[Dict[str, Any]]:
        return list(ALL_TOOL_SCHEMAS)

    def handle_tool_call(self, tool_name: str, args: Dict[str, Any], **kwargs) -> str:
        noema_tool = _TOOL_MAP.get(tool_name)
        if not noema_tool:
            return _tool_error(f"Unknown tool: {tool_name}")

        if not self._transport:
            return _tool_error("Noema is not connected.")

        # Build arguments for the Noema MCP tool.
        mcp_args: Dict[str, Any] = {}

        if tool_name == "noema_search":
            mcp_args["query"] = args.get("query", "")

        elif tool_name == "noema_remember":
            mcp_args["title"] = args.get("title", "")
            mcp_args["type"] = args.get("type", "note")
            mcp_args["body"] = args.get("body", "")
            mcp_args["author"] = self._author
            if args.get("tags"):
                mcp_args["tags"] = args["tags"]
            if args.get("derived_from"):
                mcp_args["derived_from"] = args["derived_from"]

        elif tool_name == "noema_recall":
            mcp_args["id"] = args.get("id", "")

        elif tool_name == "noema_list":
            for key in ("type", "author", "tag", "origin"):
                if args.get(key):
                    mcp_args[key] = args[key]

        elif tool_name == "noema_update":
            mcp_args["id"] = args.get("id", "")
            for key in ("title", "type", "body", "tags"):
                if args.get(key):
                    mcp_args[key] = args[key]

        elif tool_name == "noema_lineage":
            mcp_args["id"] = args.get("id", "")

        try:
            result = self._transport.call_tool(noema_tool, mcp_args)
            return json.dumps({"result": result})
        except Exception as e:
            # Attempt one restart for stdio transport.
            if isinstance(self._transport, StdioTransport) and not self._transport.is_alive:
                try:
                    logger.info("Noema subprocess died, attempting restart")
                    self._transport.start()
                    result = self._transport.call_tool(noema_tool, mcp_args)
                    return json.dumps({"result": result})
                except Exception as e2:
                    return _tool_error(f"Noema restart failed: {e2}")
            return _tool_error(str(e))

    # ---- Optional lifecycle hooks ----

    def system_prompt_block(self) -> str:
        return self._instructions_cache

    def prefetch(self, query: str, *, session_id: str = "") -> str:
        if not self._transport or not query.strip():
            return ""
        try:
            result = self._transport.call_tool("search_traces", {"query": query[:500]})
            if not result or result == "No traces found.":
                return ""
            # Filter out session log traces from prefetch results.
            lines = result.split("\n")
            filtered = []
            skip_until_next = False
            for line in lines:
                if "hermes-session" in line and "hermes-session-summary" not in line:
                    skip_until_next = True
                    continue
                if skip_until_next and line.startswith("["):
                    skip_until_next = False
                if not skip_until_next:
                    filtered.append(line)
            context = "\n".join(filtered).strip()
            if not context:
                return ""
            return (
                "## Relevant Memories (from Noema)\n\n"
                f"{context}\n\n"
                "---\n"
                "Use noema_search for more specific queries, "
                "or noema_recall to read a full trace."
            )
        except Exception as e:
            logger.debug("Noema prefetch failed: %s", e)
            return ""

    def queue_prefetch(self, query: str, *, session_id: str = "") -> None:
        # No-op in v1 — FTS5 on local SQLite is sub-millisecond.
        pass

    def on_turn_start(self, turn_number: int, message: str, **kwargs) -> None:
        self._turn_count = turn_number

    def sync_turn(self, user_content: str, assistant_content: str, *, session_id: str = "") -> None:
        if not self._transport or not self._session_trace_id:
            return

        user_trunc = user_content[:_TURN_MAX_CHARS]
        asst_trunc = assistant_content[:_TURN_MAX_CHARS]
        turn_num = self._turn_count
        timestamp = _now_iso()

        def _sync():
            try:
                block = (
                    f"\n---\n### Turn {turn_num} — {timestamp}\n\n"
                    f"**User:** {user_trunc}\n\n"
                    f"**Assistant:** {asst_trunc}"
                )
                self._transport.call_tool("append_trace", {
                    "id": self._session_trace_id,
                    "content": block,
                })
            except Exception as e:
                logger.debug("Noema sync_turn failed: %s", e)

        if self._sync_thread and self._sync_thread.is_alive():
            self._sync_thread.join(timeout=5.0)
        self._sync_thread = threading.Thread(target=_sync, daemon=True, name="noema-sync")
        self._sync_thread.start()

    def on_session_end(self, messages: List[Dict[str, Any]]) -> None:
        if not self._transport or not self._session_trace_id:
            return

        session_trace_id = self._session_trace_id
        turn_count = self._turn_count
        title = self._session_title or self._session_id[:12]

        # Extract last assistant message for the summary.
        last_assistant = ""
        for msg in reversed(messages or []):
            if msg.get("role") == "assistant":
                content = msg.get("content", "")
                if isinstance(content, str) and content.strip():
                    last_assistant = content[:3000]
                    break

        def _end():
            try:
                # Wait for any pending sync_turn to finish.
                if self._sync_thread and self._sync_thread.is_alive():
                    self._sync_thread.join(timeout=5.0)

                body = f"Session with {turn_count} turns.\n\n{last_assistant}" if last_assistant else f"Session with {turn_count} turns."
                self._transport.call_tool("create_trace", {
                    "title": f"session-summary: {title}",
                    "type": "observation",
                    "author": self._author,
                    "tags": f"hermes-session-summary, session-{self._session_id[:12]}",
                    "derived_from": session_trace_id,
                    "body": body,
                })
                self._transport.call_tool("archive_trace", {
                    "id": session_trace_id,
                })
            except Exception as e:
                logger.debug("Noema on_session_end failed: %s", e)

        if self._end_thread and self._end_thread.is_alive():
            self._end_thread.join(timeout=5.0)
        self._end_thread = threading.Thread(target=_end, daemon=True, name="noema-end")
        self._end_thread.start()

    def on_pre_compress(self, messages: List[Dict[str, Any]]) -> str:
        if not self._transport or not self._session_trace_id:
            return ""

        timestamp = _now_iso()
        session_trace_id = self._session_trace_id
        turn_count = len([m for m in (messages or []) if m.get("role") == "user"])

        # Build a compressed block from the messages being dropped.
        parts = []
        for msg in messages or []:
            role = msg.get("role", "")
            content = str(msg.get("content", ""))[:500]
            if role in ("user", "assistant") and content.strip():
                parts.append(f"**{role.title()}:** {content}")
        block = "\n\n".join(parts)

        def _compress():
            try:
                self._transport.call_tool("append_trace", {
                    "id": session_trace_id,
                    "content": (
                        f"\n---\n## Context compression at {timestamp}\n\n"
                        f"{turn_count} turns compressed.\n\n{block}"
                    ),
                })
            except Exception as e:
                logger.debug("Noema on_pre_compress append failed: %s", e)

        thread = threading.Thread(target=_compress, daemon=True, name="noema-compress")
        thread.start()

        return (
            f"Context compressed. Prior conversation preserved in Noema trace "
            f"{session_trace_id}. Use noema_recall to retrieve if needed."
        )

    def on_memory_write(self, action: str, target: str, content: str) -> None:
        if not self._transport:
            return

        def _mirror():
            try:
                if action == "add":
                    # Extract a title from the first line or truncate.
                    lines = content.strip().split("\n")
                    title = lines[0][:80] if lines else "mirrored memory"
                    self._transport.call_tool("create_trace", {
                        "title": title,
                        "type": "note",
                        "author": self._author,
                        "tags": "hermes-mirror",
                        "body": content,
                    })
                elif action == "update":
                    # Try to find and update a matching mirrored trace.
                    results = self._transport.call_tool("search_traces", {
                        "query": target[:200],
                    })
                    # Best-effort: if we can't find it, create a new one.
                    if results and results != "No traces found.":
                        # Extract the first trace ID from the results.
                        trace_id = self._extract_first_trace_id(results)
                        if trace_id:
                            self._transport.call_tool("update_trace", {
                                "id": trace_id,
                                "body": content,
                            })
                            return
                    # Fallback: create a new trace.
                    self._transport.call_tool("create_trace", {
                        "title": target[:80] or "mirrored memory",
                        "type": "note",
                        "author": self._author,
                        "tags": "hermes-mirror",
                        "body": content,
                    })
                elif action == "delete":
                    results = self._transport.call_tool("search_traces", {
                        "query": target[:200],
                    })
                    if results and results != "No traces found.":
                        trace_id = self._extract_first_trace_id(results)
                        if trace_id:
                            self._transport.call_tool("archive_trace", {"id": trace_id})
            except Exception as e:
                logger.debug("Noema on_memory_write failed: %s", e)

        if self._mirror_thread and self._mirror_thread.is_alive():
            self._mirror_thread.join(timeout=3.0)
        self._mirror_thread = threading.Thread(target=_mirror, daemon=True, name="noema-mirror")
        self._mirror_thread.start()

    def shutdown(self) -> None:
        # Join all daemon threads.
        for thread in (self._sync_thread, self._end_thread, self._mirror_thread):
            if thread and thread.is_alive():
                thread.join(timeout=5.0)
        # Close the transport.
        if self._transport:
            self._transport.close()
            self._transport = None

    # ---- Config ----

    def get_config_schema(self) -> List[Dict[str, Any]]:
        return [
            {
                "key": "cortex_name",
                "description": "Name of the Noema cortex to use",
                "required": True,
                "env_var": "NOEMA_CORTEX",
            },
            {
                "key": "noema_binary",
                "description": "Path to the noema binary (auto-detected from PATH if omitted)",
                "required": False,
                "env_var": "NOEMA_BINARY",
            },
            {
                "key": "transport",
                "description": "How to connect: 'stdio' (spawn subprocess) or 'http' (connect to running server)",
                "required": False,
                "default": "stdio",
                "choices": ["stdio", "http"],
            },
            {
                "key": "http_url",
                "description": "Noema HTTP endpoint (e.g. https://10.0.0.1:3000). Required when transport=http",
                "required": False,
                "env_var": "NOEMA_HTTP_URL",
            },
            {
                "key": "bearer_key",
                "description": "Bearer key for keyed-mode auth",
                "required": False,
                "secret": True,
                "env_var": "NOEMA_MCP_KEY",
            },
        ]

    def save_config(self, values: Dict[str, Any], hermes_home: str) -> None:
        sanitized = {k: v for k, v in (values or {}).items() if v is not None}
        config_path = Path(hermes_home) / "noema.json"
        config_path.write_text(
            json.dumps(sanitized, indent=2) + "\n",
            encoding="utf-8",
        )

    # ---- Internal helpers ----

    def _create_session_trace(self) -> None:
        """Create the session log trace on initialize.

        If the trace already exists (same session re-initialized on the same
        day), look it up and reuse it instead of failing.
        """
        title_label = self._session_title or self._session_id[:12]
        session_tag = f"session-{self._session_id[:12]}"
        body = (
            f"Session ID: {self._session_id}\n"
            f"Agent: {self._agent_identity}\n"
            f"Platform: {self._platform}\n"
            f"Started: {_now_iso()}"
        )
        try:
            result = self._transport.call_tool("create_trace", {
                "title": f"hermes-session: {title_label}",
                "type": "context",
                "author": self._author,
                "tags": f"hermes-session, {session_tag}",
                "body": body,
            })
            # Extract trace ID from "Trace created: <id>" response.
            if result and result.startswith("Trace created: "):
                self._session_trace_id = result.split("Trace created: ", 1)[1].strip()
            else:
                logger.warning("Unexpected create_trace response: %s", result)
        except RuntimeError as e:
            if "UNIQUE constraint" in str(e):
                # Session re-initialized — find and reuse the existing trace.
                logger.info("Session trace already exists, reusing")
                self._recover_session_trace(session_tag)
            else:
                logger.warning("Failed to create session trace: %s", e)
        except Exception as e:
            logger.warning("Failed to create session trace: %s", e)

    def _recover_session_trace(self, session_tag: str) -> None:
        """Find an existing session trace by tag and reuse it."""
        try:
            result = self._transport.call_tool(
                "search_traces", {"query": session_tag}
            )
            trace_id = self._extract_first_trace_id(result)
            if trace_id:
                self._session_trace_id = trace_id
                logger.info("Recovered session trace: %s", trace_id)
            else:
                logger.warning("Could not find existing session trace for %s", session_tag)
        except Exception as e:
            logger.warning("Failed to recover session trace: %s", e)

    @staticmethod
    def _extract_first_trace_id(list_output: str) -> Optional[str]:
        """Extract the first trace ID from list_traces/search_traces output.

        Output format: [type] trace-id (YYYY-MM-DD) — author [tags]
        """
        for line in list_output.split("\n"):
            line = line.strip()
            if line.startswith("["):
                # [type] trace-id (date) — ...
                parts = line.split("]", 1)
                if len(parts) > 1:
                    rest = parts[1].strip()
                    trace_id = rest.split(" ", 1)[0].strip()
                    if trace_id:
                        return trace_id
        return None


# ---------------------------------------------------------------------------
# Plugin registration
# ---------------------------------------------------------------------------

def register(ctx) -> None:
    """Called by Hermes plugin discovery."""
    ctx.register_memory_provider(NoemaMemoryProvider())
