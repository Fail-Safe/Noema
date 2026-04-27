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

export class McpClient {
	private nextId = 1;

	constructor(private endpoint: string, private bearerKey: string) {}

	updateConfig(endpoint: string, bearerKey: string) {
		this.endpoint = endpoint;
		this.bearerKey = bearerKey;
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
		const id = this.nextId++;
		const body = {
			jsonrpc: "2.0",
			id,
			method: "tools/call",
			params: { name, arguments: args },
		};
		const headers: Record<string, string> = {
			"Content-Type": "application/json",
			Accept: "application/json",
		};
		if (this.bearerKey) {
			headers["Authorization"] = `Bearer ${this.bearerKey}`;
		}

		const url = this.endpoint.replace(/\/+$/, "") + "/mcp";
		const resp = await fetch(url, {
			method: "POST",
			headers,
			body: JSON.stringify(body),
		});
		if (!resp.ok) {
			throw new Error(`HTTP ${resp.status}: ${resp.statusText}`);
		}
		const payload = (await resp.json()) as {
			result?: ToolResult;
			error?: { code: number; message: string; data?: unknown };
		};
		if (payload.error) {
			throw new JsonRpcError(payload.error.code, payload.error.message, payload.error.data);
		}
		if (!payload.result) {
			throw new Error("response missing result");
		}
		return payload.result;
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

function parseIdList(s: string): string[] {
	const trimmed = s.trim();
	if (!trimmed || trimmed === "(none)") return [];
	return trimmed.split(",").map((id) => id.trim()).filter(Boolean);
}
