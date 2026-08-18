// Tiny MCP-over-HTTP client. We deliberately don't pull in
// @modelcontextprotocol/sdk: this plugin only needs two tools, and
// the SDK's discovery / multi-transport surface would dominate the
// bundle size. The whole protocol surface we use is JSON-RPC 2.0 POSTs
// to a single /mcp endpoint, with a Bearer-key Authorization header
// when the server is in keyed mode.
//
// References:
//   - MCP 2025-03-26 Streamable HTTP transport
//   - noema's auth posture: NOEMA_MCP_KEY env var or sidecar file
//   - noema serve --transport http always exposes /mcp regardless of
//     mode (sync/publish/subscribe gate which tools are allowed, not
//     which paths are served)

export type ToolResult = {
	content: Array<{ type: string; text?: string }>;
	isError?: boolean;
};

// JsonRpcError is what the server returns when a tool call fails at
// the protocol layer (bad params, unknown method, etc.). Tool-level
// errors (e.g. trace not found) come back as a normal result with
// isError=true, not as a JsonRpcError.
export class JsonRpcError extends Error {
	constructor(public code: number, message: string, public data?: unknown) {
		super(message);
		this.name = "JsonRpcError";
	}
}

// UnauthorizedError signals that the server rejected our credentials —
// an HTTP 401 from noema's AuthMiddleware, which fires before any MCP
// routing when the server is in keyed mode and the Authorization header
// is missing or doesn't match. It's distinct from generic transport
// failures (server down, DNS, TLS) and from InvalidSessionError (404)
// so the plugin can tell the user "your bearer key is wrong" rather
// than the ambiguous "disconnected". serverMessage carries the body the
// middleware returned (e.g. `unauthorized: NOEMA_MCP_KEY ... required`)
// when one was present.
export class UnauthorizedError extends Error {
	constructor(public serverMessage?: string) {
		super("MCP server rejected the bearer key (HTTP 401)");
		this.name = "UnauthorizedError";
	}
}

export interface NoemaIdentity {
	id: string;
	name: string;
	version: number;
	mode: string;
	rank?: { rank: number; observed_at: string; cortex_id: string };
}

// Lineage is the parsed trace_lineage tool response. The MCP server
// returns plaintext with three lines:
//
//   Trace: <id>
//   Derived from: <comma-separated ids> | (none)
//   Derived by:   <comma-separated ids> | (none)
//
// We parse it into plain ID lists. Title/tier metadata for ancestors
// and descendants is looked up from Obsidian's own metadataCache by
// the lineage view (it's faster and lighter than a get_trace round
// trip per ID, and the cortex dir IS the Obsidian vault so the data
// is already in memory).
export interface Lineage {
	traceId: string;
	derivedFrom: string[];
	derivedBy: string[];
}

// TraceType matches the cortex's trace-type enum. Surfaced as a
// shared constant so the create-trace modal and any future tier-aware
// UI can populate dropdowns from one source of truth that tracks the
// server's accepted values.
export const TRACE_TYPES = [
	"fact",
	"decision",
	"preference",
	"context",
	"skill",
	"intent",
	"observation",
	"note",
] as const;
export type TraceType = (typeof TRACE_TYPES)[number];
export type SearchMode = "lexical" | "semantic" | "hybrid";

export interface SearchResult {
	id: string;
	title: string;
	type: string;
	author: string;
	tags: string;
	created: string;
}

// CreateTraceParams is the input shape for McpClient.createTrace.
// tags and derivedFrom are arrays at this layer for caller ergonomics;
// they get joined to the comma-separated wire format the server
// expects inside createTrace.
export interface CreateTraceParams {
	title: string;
	type: TraceType;
	body: string;
	author?: string;
	tags?: string[];
	derivedFrom?: string[];
}

// Streamable-HTTP MCP servers (per the 2025-03-26 spec) bind every
// JSON-RPC request to a server-allocated session id. The lifecycle is:
//
//   1. Client POSTs `initialize` with no session header.
//   2. Server responds with the Mcp-Session-Id response header plus an
//      InitializeResult JSON-RPC payload.
//   3. Client POSTs `notifications/initialized` (no id, it's a
//      notification) with the session header attached.
//   4. Subsequent tools/call requests carry Mcp-Session-Id.
//
// Skipping step 1 produces "404 Invalid session ID" — the noema server
// surfaces that exact status/body, which is how this implementation
// originally drifted: I went straight to tools/call and got rejected.
//
// Sessions can be invalidated server-side (server restart, idle
// timeout); when that happens we get another 404 mid-call. We catch
// that, drop the cached session id, and re-handshake on the next
// request. One transparent retry is enough — if the server is genuinely
// down we'll hit a different error path.
const MCP_PROTOCOL_VERSION = "2025-03-26";

export class McpClient {
	private nextId = 1;
	private sessionId: string | null = null;
	private sessionPromise: Promise<void> | null = null;

	constructor(private endpoint: string, private bearerKey: string) {}

	updateConfig(endpoint: string, bearerKey: string) {
		this.endpoint = endpoint;
		this.bearerKey = bearerKey;
		// Force a fresh handshake against the (possibly different)
		// server on the next call. The old session id is meaningless
		// once we've changed endpoints.
		this.sessionId = null;
		this.sessionPromise = null;
	}

	// cortexIdentity is the cheapest tool to call. Used as a
	// connection-health ping so the status bar can flip between
	// "connected (cortex=X)" and "disconnected" without doing real
	// work. Returns the parsed identity payload on success.
	async cortexIdentity(): Promise<NoemaIdentity> {
		const text = await this.callToolText("cortex_identity", {});
		return JSON.parse(text);
	}

	// traceLineage parses the plaintext response shape into ID lists.
	// (none) is normalized to an empty array.
	async traceLineage(traceId: string): Promise<Lineage> {
		const text = await this.callToolText("trace_lineage", { id: traceId });
		return parseLineage(text, traceId);
	}

	// appendTrace invokes the append_trace MCP tool, which is the
	// designed primitive for "add a line or two to an existing
	// trace" — the cortex reads the current body server-side, appends,
	// recomputes the content_hash, and emits an update event in a
	// single transaction. Compared to a full update_trace round-trip
	// this avoids the read-modify-write window where another process
	// could race against us, and saves shipping the existing body
	// over the wire just to re-send it back.
	async appendTrace(traceId: string, content: string): Promise<void> {
		await this.callToolText("append_trace", {
			id: traceId,
			content: content,
		});
	}

	// createTrace invokes the create_trace MCP tool. The server
	// generates the canonical YYYYMMDD-<slug>.md filename, computes
	// the content_hash, sets origin to the local cortex name, and
	// emits the create event in the same transaction. Returns the
	// freshly-allocated trace ID parsed out of the server's
	// "Trace created: <id>" response.
	//
	// body is required by the server schema; pass at least a single
	// placeholder character if the caller wants to fill content in
	// the editor afterwards. tags and derivedFrom come in as arrays
	// for caller ergonomics and get joined into comma-separated
	// strings (the wire shape the server expects).
	async createTrace(params: CreateTraceParams): Promise<string> {
		const args: Record<string, unknown> = {
			title: params.title,
			type: params.type,
			body: params.body,
		};
		if (params.author) args.author = params.author;
		if (params.tags && params.tags.length > 0) args.tags = params.tags.join(",");
		if (params.derivedFrom && params.derivedFrom.length > 0) {
			args.derived_from = params.derivedFrom.join(",");
		}
		const text = await this.callToolText("create_trace", args);
		const match = text.match(/Trace created:\s*(\S+)/);
		if (!match) {
			throw new Error(`create_trace: unexpected response shape: ${text}`);
		}
		return match[1];
	}

	async searchTraces(query: string, mode: SearchMode): Promise<SearchResult[]> {
		const text = await this.callToolText("search_traces", { query, mode });
		return parseSearchResults(text);
	}

	// callToolText invokes a tool and returns the concatenated text
	// content, or throws JsonRpcError if the protocol-layer call
	// failed. Most noema tools return a single text block; we
	// concatenate any extras for safety.
	private async callToolText(name: string, args: Record<string, unknown>): Promise<string> {
		const result = await this.callTool(name, args);
		if (result.isError) {
			const msg = result.content
				.map((c) => c.text ?? "")
				.join("\n")
				.trim();
			throw new Error(`tool ${name}: ${msg || "unknown error"}`);
		}
		return result.content
			.map((c) => c.text ?? "")
			.join("");
	}

	private async callTool(name: string, args: Record<string, unknown>): Promise<ToolResult> {
		await this.ensureSession();
		try {
			return await this.callToolOnce(name, args);
		} catch (err) {
			// Session may have been invalidated server-side (restart,
			// idle timeout). Drop it, re-handshake, retry once. Any
			// non-session failure surfaces on the second attempt.
			if (err instanceof InvalidSessionError) {
				this.sessionId = null;
				this.sessionPromise = null;
				await this.ensureSession();
				return await this.callToolOnce(name, args);
			}
			throw err;
		}
	}

	private async callToolOnce(
		name: string,
		args: Record<string, unknown>
	): Promise<ToolResult> {
		const payload = await this.rpc({
			method: "tools/call",
			params: { name, arguments: args },
		});
		const result = payload as ToolResult;
		return result;
	}

	// ensureSession does a one-shot handshake (initialize +
	// notifications/initialized) and caches the session id. Concurrent
	// callers share a single in-flight promise so a burst of tool
	// calls during plugin startup doesn't trigger multiple
	// handshakes.
	private async ensureSession(): Promise<void> {
		if (this.sessionId) return;
		if (!this.sessionPromise) {
			this.sessionPromise = this.handshake().catch((err) => {
				// Reset so the next caller can retry rather than
				// permanently latching the rejection.
				this.sessionPromise = null;
				throw err;
			});
		}
		await this.sessionPromise;
	}

	private async handshake(): Promise<void> {
		// Step 1: initialize. Server returns the session id in a
		// response header, alongside the JSON-RPC InitializeResult
		// payload. We don't actually care about the result body — the
		// session id is what unlocks subsequent tool calls.
		const headers = this.buildHeaders();
		const url = this.mcpUrl();
		const initBody = {
			jsonrpc: "2.0",
			id: this.nextId++,
			method: "initialize",
			params: {
				protocolVersion: MCP_PROTOCOL_VERSION,
				capabilities: {},
				clientInfo: { name: "noema-obsidian", version: "0.3.0" },
			},
		};
		const resp = await fetch(url, {
			method: "POST",
			headers,
			body: JSON.stringify(initBody),
		});
		if (resp.status === 401) {
			// Keyed-mode server, missing/wrong bearer key. Surface this
			// distinctly — re-handshaking won't help, the credential is
			// the problem.
			throw new UnauthorizedError(await read401Message(resp));
		}
		if (!resp.ok) {
			throw new Error(`initialize: HTTP ${resp.status}: ${resp.statusText}`);
		}
		await readJsonRpcResponse(resp, initBody.id);
		const sid = resp.headers.get("Mcp-Session-Id");
		if (!sid) {
			throw new Error("initialize: server did not return Mcp-Session-Id header");
		}
		this.sessionId = sid;

		// Step 2: notifications/initialized. No response is expected
		// for a JSON-RPC notification (no id field), but we still POST
		// it so the server can advance its session state-machine.
		const notifyHeaders = { ...this.buildHeaders(), "Mcp-Session-Id": sid };
		await fetch(url, {
			method: "POST",
			headers: notifyHeaders,
			body: JSON.stringify({
				jsonrpc: "2.0",
				method: "notifications/initialized",
				params: {},
			}),
		});
	}

	// rpc sends an arbitrary JSON-RPC request (method + params) on the
	// established session and returns the result field, throwing
	// JsonRpcError on protocol-level errors and InvalidSessionError on
	// 404s so the caller's retry loop can re-handshake.
	private async rpc(body: { method: string; params: unknown }): Promise<unknown> {
		const id = this.nextId++;
		const headers = this.buildHeaders();
		if (this.sessionId) {
			headers["Mcp-Session-Id"] = this.sessionId;
		}
		const resp = await fetch(this.mcpUrl(), {
			method: "POST",
			headers,
			body: JSON.stringify({ jsonrpc: "2.0", id, ...body }),
		});
		if (resp.status === 401) {
			// Bearer key rejected mid-session (e.g. the server was
			// restarted into keyed mode, or the key was rotated). The
			// retry loop deliberately does NOT re-handshake on this —
			// re-sending the same bad key just 401s again.
			throw new UnauthorizedError(await read401Message(resp));
		}
		if (resp.status === 404) {
			// "Invalid session ID" or similar. Caller will re-
			// handshake and retry. We swallow the body here because
			// the only useful information was the status.
			throw new InvalidSessionError();
		}
		if (!resp.ok) {
			throw new Error(`HTTP ${resp.status}: ${resp.statusText}`);
		}
		const json = await readJsonRpcResponse(resp, id);
		if (json.error) {
			throw new JsonRpcError(json.error.code, json.error.message, json.error.data);
		}
		if (json.result === undefined) {
			throw new Error("response missing result");
		}
		return json.result;
	}

	private buildHeaders(): Record<string, string> {
		const h: Record<string, string> = {
			"Content-Type": "application/json",
			Accept: "application/json, text/event-stream",
		};
		if (this.bearerKey) {
			h["Authorization"] = `Bearer ${this.bearerKey}`;
		}
		return h;
	}

	private mcpUrl(): string {
		return this.endpoint.replace(/\/+$/, "") + "/mcp";
	}
}

type JsonRpcResponse = {
	id?: string | number | null;
	result?: unknown;
	error?: { code: number; message: string; data?: unknown };
};

// Streamable HTTP permits a JSON-RPC response as either plain JSON or a
// finite server-sent-event stream. noema's Rust transport uses the latter
// for the session-based 2025-03-26 protocol, even for one-shot tool calls.
// Extract the response matching our request id while ignoring any comments,
// retry hints, or unrelated messages that precede it.
export async function readJsonRpcResponse(
	response: Response,
	expectedId: string | number
): Promise<JsonRpcResponse> {
	const contentType = response.headers.get("Content-Type") ?? "";
	if (!contentType.toLowerCase().includes("text/event-stream")) {
		return (await response.json()) as JsonRpcResponse;
	}

	const messages: JsonRpcResponse[] = [];
	for (const event of (await response.text()).split(/\r?\n\r?\n/)) {
		const data = event
			.split(/\r?\n/)
			.filter((line) => line.startsWith("data:"))
			.map((line) => line.slice(5).replace(/^ /, ""))
			.join("\n");
		if (!data) continue;
		try {
			messages.push(JSON.parse(data) as JsonRpcResponse);
		} catch {
			// A malformed or non-JSON event cannot be our JSON-RPC response.
		}
	}

	const match = messages.find((message) => message.id === expectedId);
	if (!match) {
		throw new Error(`SSE response missing JSON-RPC id ${expectedId}`);
	}
	return match;
}

// InvalidSessionError signals "the server didn't recognize our session
// id" — distinct from generic transport errors so the retry loop in
// callTool can react specifically. Internal; not exported.
class InvalidSessionError extends Error {
	constructor() {
		super("invalid MCP session");
		this.name = "InvalidSessionError";
	}
}

// read401Message best-effort-extracts the human-readable reason from a
// 401 response body. noema's AuthMiddleware returns
// `{"error":"unauthorized: ..."}`; anything else (proxy, gateway) we
// just hand back verbatim, trimmed. Never throws — a missing/garbled
// body simply yields undefined so the caller falls back to its default
// message.
async function read401Message(resp: Response): Promise<string | undefined> {
	try {
		const text = (await resp.text()).trim();
		if (!text) return undefined;
		try {
			const parsed = JSON.parse(text) as { error?: string };
			if (parsed && typeof parsed.error === "string") return parsed.error;
		} catch {
			// Not JSON — return the raw body.
		}
		return text;
	} catch {
		return undefined;
	}
}

// parseLineage turns the trace_lineage plaintext response into a
// structured Lineage. Exported for unit-style smoke testing without
// having to spin up a real MCP server. Tolerates the literal "(none)"
// marker and trims surrounding whitespace, which the server emits
// for empty derivation lists.
export function parseLineage(text: string, fallbackId: string): Lineage {
	const lines = text.split(/\r?\n/);
	let derivedFrom: string[] = [];
	let derivedBy: string[] = [];
	let traceId = fallbackId;
	for (const line of lines) {
		const trimmed = line.trim();
		if (trimmed.startsWith("Trace:")) {
			traceId = trimmed.slice("Trace:".length).trim();
		} else if (trimmed.startsWith("Derived from:")) {
			derivedFrom = parseIdList(trimmed.slice("Derived from:".length));
		} else if (trimmed.startsWith("Derived by:")) {
			derivedBy = parseIdList(trimmed.slice("Derived by:".length));
		}
	}
	return { traceId, derivedFrom, derivedBy };
}

export function parseSearchResults(text: string): SearchResult[] {
	const rows: SearchResult[] = [];
	const lines = text.split(/\r?\n/);
	for (let i = 0; i < lines.length; i++) {
		const line = lines[i];
		const mcpRow = line.match(
			/^\[[^\]]+\]\s+\[([^\]]+)\]\s+(\S+)\s+\((\d{4}-\d{2}-\d{2})\)(?:\s+—\s+([^\[]+?))?(?:\s+\[(.*)\])?$/
		);
		if (mcpRow) {
			const titleLine = lines[i + 1]?.startsWith("  ")
				? lines[++i].trim()
				: "";
			rows.push({
				id: mcpRow[2],
				title: titleLine,
				type: mcpRow[1],
				author: mcpRow[4]?.trim() ?? "",
				tags: mcpRow[5]?.trim() ?? "",
				created: mcpRow[3],
			});
			continue;
		}

		const id = line.trimStart().split(/\s+/, 1)[0];
		if (!/^\d{8}-[a-z0-9][a-z0-9-]*$/.test(id)) continue;
		const parts = line.split(/\s{2,}/).map((s) => s.trim());
		if (parts.length < 2 || parts[0] !== id) continue;
		rows.push({
			id: parts[0],
			title: parts[1] ?? "",
			type: parts[2] ?? "",
			author: parts[3] ?? "",
			tags: parts[4] ?? "",
			created: parts[5] ?? "",
		});
	}
	return rows;
}

function parseIdList(s: string): string[] {
	const trimmed = s.trim();
	if (!trimmed || trimmed === "(none)") return [];
	return trimmed.split(",").map((id) => id.trim()).filter(Boolean);
}
