const assert = require("node:assert/strict");
const test = require("node:test");
const esbuild = require("esbuild");

async function loadRenameTitle() {
	const result = await esbuild.build({
		entryPoints: ["src/rename-title.ts"],
		bundle: true,
		format: "esm",
		platform: "node",
		write: false,
	});
	const source = result.outputFiles[0].text;
	return import(`data:text/javascript;base64,${Buffer.from(source).toString("base64")}`);
}

test("filename rename becomes the proposed semantic title", async () => {
	const { titleFromFilenameRename } = await loadRenameTitle();
	assert.equal(
		titleFromFilenameRename("20260830-hugging-face", "Read our How to Run DeepSeek-V4 Guide!"),
		"Read our How to Run DeepSeek-V4 Guide!"
	);
});

test("an accidentally retained stable ID prefix is removed", async () => {
	const { titleFromFilenameRename } = await loadRenameTitle();
	assert.equal(
		titleFromFilenameRename(
			"20260830-hugging-face",
			"20260830-hugging-face-DeepSeek-V4-0731 Guide"
		),
		"DeepSeek-V4-0731 Guide"
	);
});
