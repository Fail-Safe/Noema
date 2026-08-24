const assert = require("node:assert/strict");
const test = require("node:test");
const esbuild = require("esbuild");

async function loadClient() {
	const result = await esbuild.build({
		entryPoints: ["src/mcp-client.ts"],
		bundle: true,
		format: "esm",
		platform: "node",
		write: false,
	});
	const source = result.outputFiles[0].text;
	return import(`data:text/javascript;base64,${Buffer.from(source).toString("base64")}`);
}

function sse(payload, headers = {}) {
	return new Response(`event: message\ndata: ${JSON.stringify(payload)}\n\n`, {
		status: 200,
		headers: { "Content-Type": "text/event-stream", ...headers },
	});
}

test("MCP client accepts and decodes Streamable HTTP SSE responses", async () => {
	const { McpClient } = await loadClient();
	const requests = [];
	const originalFetch = globalThis.fetch;
	globalThis.fetch = async (url, init) => {
		requests.push({ url, init });
		switch (requests.length) {
			case 1:
				return sse(
					{ jsonrpc: "2.0", id: 1, result: { protocolVersion: "2025-03-26" } },
					{ "Mcp-Session-Id": "test-session" }
				);
			case 2:
				return new Response(null, { status: 202 });
			case 3:
				return sse({
					jsonrpc: "2.0",
					id: 2,
					result: {
						content: [
							{
								type: "text",
								text: JSON.stringify({ id: "cortex-id", name: "test", version: 2, mode: "sync" }),
							},
						],
					},
				});
			default:
				throw new Error("unexpected fetch");
		}
	};

	try {
		const client = new McpClient("https://memory.example.com:3000", "test-key");
		const identity = await client.cortexIdentity();
		assert.equal(identity.name, "test");
		assert.equal(requests.length, 3);
		for (const request of requests) {
			assert.equal(request.init.headers.Accept, "application/json, text/event-stream");
		}
		assert.equal(requests[1].init.headers["Mcp-Session-Id"], "test-session");
		assert.equal(requests[2].init.headers["Mcp-Session-Id"], "test-session");
	} finally {
		globalThis.fetch = originalFetch;
	}
});

test("MCP client reports the failed transport phase and network category", async () => {
	const { McpClient } = await loadClient();
	const originalFetch = globalThis.fetch;
	globalThis.fetch = async () => {
		const error = new TypeError("fetch failed");
		error.cause = { code: "ECONNREFUSED" };
		throw error;
	};

	try {
		const client = new McpClient("https://memory.example.com:3000", "test-key");
		await assert.rejects(
			client.cortexIdentity(),
			/initialize request: connection refused \(ECONNREFUSED\)/
		);
	} finally {
		globalThis.fetch = originalFetch;
	}
});

test("connection errors retain useful HTTP and protocol messages", async () => {
	const { describeConnectionError } = await loadClient();

	assert.equal(
		describeConnectionError(new Error("initialize: HTTP 403: Forbidden")),
		"initialize: HTTP 403: Forbidden"
	);
	assert.equal(describeConnectionError(undefined), "unknown connection error");
});
