"""Noema MCP transport layer for the Hermes memory provider plugin.

Two transports are supported:

- StdioTransport: spawns `noema serve --transport stdio` as a subprocess
  and communicates via JSON-RPC 2.0 over stdin/stdout pipes.
- HttpTransport: connects to an already-running `noema serve --transport http`
  endpoint via POST /mcp with JSON-RPC 2.0 payloads.

Both expose the same call_tool(name, args) -> dict interface. A threading.Lock
guards all I/O so concurrent calls from daemon threads are safe.
"""

import json
import logging
import os
import shutil
import subprocess
import threading
from pathlib import Path
from typing import Any, Dict, List, Optional
from urllib.request import Request, urlopen
from urllib.error import URLError

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Binary discovery
# ---------------------------------------------------------------------------

_binary_cache: Optional[str] = None
_binary_lock = threading.Lock()


def find_binary(override: Optional[str] = None) -> Optional[str]:
    """Locate the noema binary. Resolution order:
    1. Explicit override (config or NOEMA_BINARY env)
    2. shutil.which("noema") — standard PATH lookup
    3. Well-known install locations
    """
    global _binary_cache
    with _binary_lock:
        if _binary_cache is not None:
            return _binary_cache if _binary_cache != "" else None

    # Explicit override.
    path = override or os.environ.get("NOEMA_BINARY", "")
    if path:
        if os.path.isfile(path) and os.access(path, os.X_OK):
            with _binary_lock:
                _binary_cache = path
            return path
        logger.warning("Configured noema binary %s not found or not executable", path)
        with _binary_lock:
            _binary_cache = ""
        return None

    # PATH lookup.
    found = shutil.which("noema")
    if found:
        with _binary_lock:
            _binary_cache = found
        return found

    # Well-known locations.
    home = Path.home()
    candidates = [
        home / "go" / "bin" / "noema",
        Path("/usr/local/bin/noema"),
        home / ".local" / "bin" / "noema",
    ]
    for c in candidates:
        if c.is_file() and os.access(str(c), os.X_OK):
            with _binary_lock:
                _binary_cache = str(c)
            return str(c)

    with _binary_lock:
        _binary_cache = ""
    return None


def reset_binary_cache() -> None:
    """Clear the cached binary path (useful after config changes)."""
    global _binary_cache
    with _binary_lock:
        _binary_cache = None


# ---------------------------------------------------------------------------
# JSON-RPC helpers
# ---------------------------------------------------------------------------

def _jsonrpc_request(method: str, params: dict, req_id: int) -> bytes:
    """Build a JSON-RPC 2.0 request as a newline-terminated byte string."""
    payload = {
        "jsonrpc": "2.0",
        "id": req_id,
        "method": method,
        "params": params,
    }
    return json.dumps(payload).encode("utf-8") + b"\n"


def _parse_jsonrpc_response(line: str) -> dict:
    """Parse a JSON-RPC 2.0 response, returning the result or raising on error."""
    resp = json.loads(line)
    if "error" in resp and resp["error"] is not None:
        err = resp["error"]
        msg = err.get("message", str(err)) if isinstance(err, dict) else str(err)
        raise RuntimeError(f"JSON-RPC error: {msg}")
    return resp.get("result", {})


def _extract_text(result: dict) -> str:
    """Extract text content from an MCP CallToolResult."""
    content = result.get("content", [])
    for item in content:
        if isinstance(item, dict) and item.get("type") == "text":
            return item.get("text", "")
    return ""


# ---------------------------------------------------------------------------
# MCP initialize handshake
# ---------------------------------------------------------------------------

_INIT_PARAMS = {
    "protocolVersion": "2025-03-26",
    "capabilities": {},
    "clientInfo": {
        "name": "noema-hermes-plugin",
        "version": "0.1.0",
    },
}


# ---------------------------------------------------------------------------
# StdioTransport
# ---------------------------------------------------------------------------

class StdioTransport:
    """Spawn `noema serve --transport stdio` and communicate via pipes."""

    def __init__(self, binary: str, cortex_name: str):
        self._binary = binary
        self._cortex_name = cortex_name
        self._process: Optional[subprocess.Popen] = None
        self._lock = threading.Lock()
        self._req_id = 0

    def start(self) -> None:
        """Spawn the subprocess and perform the MCP initialize handshake."""
        cmd = [self._binary, "serve", "--transport", "stdio", "--cortex", self._cortex_name]
        self._process = subprocess.Popen(
            cmd,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=False,
        )
        self._handshake()

    def _handshake(self) -> None:
        """Send MCP initialize + initialized notification."""
        # initialize request
        resp = self._send("initialize", _INIT_PARAMS)
        logger.debug("MCP initialize response: %s", resp)
        # initialized notification (no id, no response expected)
        notif = json.dumps({
            "jsonrpc": "2.0",
            "method": "notifications/initialized",
        }).encode("utf-8") + b"\n"
        with self._lock:
            self._process.stdin.write(notif)
            self._process.stdin.flush()

    def _send(self, method: str, params: dict) -> dict:
        """Send a JSON-RPC request and read the response. Thread-safe.

        Skips non-JSON lines (e.g. server log output on startup) by
        reading until a line starting with '{' is found.
        """
        with self._lock:
            if self._process is None or self._process.poll() is not None:
                raise RuntimeError("Noema subprocess is not running")
            self._req_id += 1
            req = _jsonrpc_request(method, params, self._req_id)
            self._process.stdin.write(req)
            self._process.stdin.flush()
            while True:
                line = self._process.stdout.readline()
                if not line:
                    raise RuntimeError("Noema subprocess returned no output (EOF)")
                text = line.decode("utf-8").strip()
                if text.startswith("{"):
                    return _parse_jsonrpc_response(text)
                logger.debug("Skipping non-JSON output: %s", text)

    def call_tool(self, name: str, arguments: Optional[Dict[str, Any]] = None) -> str:
        """Call a Noema MCP tool. Returns the text content of the result."""
        params = {"name": name}
        if arguments:
            params["arguments"] = arguments
        result = self._send("tools/call", params)
        return _extract_text(result)

    @property
    def is_alive(self) -> bool:
        return self._process is not None and self._process.poll() is None

    def close(self) -> None:
        """Terminate the subprocess."""
        if self._process is None:
            return
        try:
            self._process.terminate()
            self._process.wait(timeout=2)
        except subprocess.TimeoutExpired:
            self._process.kill()
            self._process.wait(timeout=2)
        except Exception:
            pass
        self._process = None


# ---------------------------------------------------------------------------
# HttpTransport
# ---------------------------------------------------------------------------

class HttpTransport:
    """Connect to an already-running Noema HTTP MCP endpoint."""

    def __init__(self, url: str, bearer_key: Optional[str] = None):
        self._url = url.rstrip("/")
        if not self._url.endswith("/mcp"):
            self._url += "/mcp"
        self._bearer_key = bearer_key
        self._session_id: Optional[str] = None
        self._lock = threading.Lock()
        self._req_id = 0

    def start(self) -> None:
        """Perform the MCP initialize handshake over HTTP."""
        resp = self._send("initialize", _INIT_PARAMS)
        logger.debug("MCP initialize response: %s", resp)
        # Send initialized notification.
        self._send_notification("notifications/initialized")

    def _build_request(self, body: bytes) -> Request:
        """Build an HTTP request with standard headers."""
        req = Request(self._url, data=body, method="POST")
        req.add_header("Content-Type", "application/json")
        if self._bearer_key:
            req.add_header("Authorization", f"Bearer {self._bearer_key}")
        if self._session_id:
            req.add_header("Mcp-Session-Id", self._session_id)
        return req

    def _send(self, method: str, params: dict) -> dict:
        """Send a JSON-RPC request over HTTP. Thread-safe."""
        with self._lock:
            self._req_id += 1
            body = json.dumps({
                "jsonrpc": "2.0",
                "id": self._req_id,
                "method": method,
                "params": params,
            }).encode("utf-8")
            req = self._build_request(body)
            with urlopen(req, timeout=30) as resp:
                # Capture session ID from first response.
                sid = resp.headers.get("Mcp-Session-Id")
                if sid:
                    self._session_id = sid
                data = resp.read().decode("utf-8")
                return _parse_jsonrpc_response(data)

    def _send_notification(self, method: str) -> None:
        """Send a JSON-RPC notification (no id, no response expected)."""
        with self._lock:
            body = json.dumps({
                "jsonrpc": "2.0",
                "method": method,
            }).encode("utf-8")
            req = self._build_request(body)
            try:
                with urlopen(req, timeout=10) as resp:
                    resp.read()
            except Exception:
                pass  # Notifications are best-effort.

    def call_tool(self, name: str, arguments: Optional[Dict[str, Any]] = None) -> str:
        """Call a Noema MCP tool. Returns the text content of the result."""
        params = {"name": name}
        if arguments:
            params["arguments"] = arguments
        result = self._send("tools/call", params)
        return _extract_text(result)

    @property
    def is_alive(self) -> bool:
        return True  # HTTP is connectionless; we can't check without a call.

    def close(self) -> None:
        """No-op for HTTP (the server is independently managed)."""
        pass
