import { App, Modal, Notice, TFile } from "obsidian";
import type NoemaPlugin from "./main";
import { CreateTraceParams, TRACE_TYPES, TraceType } from "./mcp-client";

// CreateTraceModal collects the structured frontmatter fields that
// every trace needs and hands them to McpClient.createTrace, which
// generates the canonical filename and writes the file via the
// cortex's mutation method (so origin, content_hash, and the
// create event all land correctly).
//
// The body is intentionally NOT in this modal: a multi-line text
// area in a modal scales badly past a few sentences and competes
// with Obsidian's own editor for the same UX role. Instead we
// create the file with a single-character placeholder body, then
// open it in the editor so the user can write the real content
// using the tool they already have open.
export class CreateTraceModal extends Modal {
	private titleInput!: HTMLInputElement;
	private typeSelect!: HTMLSelectElement;
	private tagsInput!: HTMLInputElement;
	private tagWarningsEl!: HTMLElement;
	private derivedFromInput!: HTMLInputElement;
	private submitBtn!: HTMLButtonElement;
	private errorEl!: HTMLElement;
	private submitting = false;

	constructor(app: App, private plugin: NoemaPlugin) {
		super(app);
	}

	onOpen(): void {
		const { contentEl } = this;
		contentEl.empty();
		contentEl.addClass("noema-create-modal");

		this.titleEl.setText("New trace");

		this.titleInput = this.row("Title", "input", {
			type: "text",
			placeholder: "Why we chose Go",
		}) as HTMLInputElement;

		this.typeSelect = this.row("Type", "select") as HTMLSelectElement;
		for (const t of TRACE_TYPES) {
			const opt = this.typeSelect.createEl("option", { text: t });
			opt.value = t;
		}
		// Default to "note" — the most generic type and the right
		// fallback when the author hasn't decided yet. Specific types
		// (decision, fact, etc.) are deliberate choices a user makes
		// when they know what they're capturing.
		this.typeSelect.value = "note";

		this.tagsInput = this.row("Tags", "input", {
			type: "text",
			placeholder: "go, language, architecture",
		}) as HTMLInputElement;
		this.descriptor("Comma- or semicolon-separated. Optional.");
		// Tag-validation hints for Obsidian-incompatible tag formats.
		// We don't BLOCK submission — the cortex accepts any string,
		// the limitation is purely what Obsidian's tag panel renders.
		// Inline hints surface the gotcha at the moment of entry, so
		// users authoring through Obsidian can adjust without
		// re-reading the docs.
		this.tagWarningsEl = contentEl.createEl("div", {
			cls: "noema-create-tag-warnings",
		});
		this.tagWarningsEl.style.display = "none";
		this.tagsInput.addEventListener("input", () => this.refreshTagWarnings());

		this.derivedFromInput = this.row("Derived from", "input", {
			type: "text",
			placeholder: "20260328-language-candidates, 20260329-runtime-survey",
		}) as HTMLInputElement;
		this.descriptor("Comma-separated trace IDs this one is derived from. Optional.");

		this.errorEl = contentEl.createEl("div", { cls: "noema-create-error" });
		this.errorEl.style.display = "none";

		const buttonRow = contentEl.createEl("div", { cls: "noema-create-buttons" });
		const cancelBtn = buttonRow.createEl("button", { text: "Cancel" });
		cancelBtn.addEventListener("click", () => this.close());
		this.submitBtn = buttonRow.createEl("button", {
			text: "Create",
			cls: "mod-cta",
		});
		this.submitBtn.addEventListener("click", () => this.submit());

		// Submit on Enter from the title field — most common path.
		this.titleInput.addEventListener("keydown", (evt) => {
			if (evt.key === "Enter") {
				evt.preventDefault();
				this.submit();
			}
		});

		// Focus the title field on open so the user can start typing
		// immediately. Obsidian focuses the modal's first focusable
		// element by default but only after a render tick; doing it
		// explicitly here removes the perceptible lag.
		setTimeout(() => this.titleInput.focus(), 0);
	}

	onClose(): void {
		this.contentEl.empty();
	}

	private row(
		label: string,
		tag: "input" | "select",
		attrs?: Record<string, string>
	): HTMLElement {
		const row = this.contentEl.createEl("div", { cls: "noema-create-row" });
		row.createEl("label", { text: label });
		const el = row.createEl(tag);
		if (attrs) {
			for (const [k, v] of Object.entries(attrs)) {
				el.setAttribute(k, v);
			}
		}
		return el;
	}

	// descriptor adds a small helper line under the most recent
	// row — used for one-liner field hints that are too long for
	// the placeholder.
	private descriptor(text: string): void {
		this.contentEl.createEl("div", {
			cls: "noema-create-hint",
			text,
		});
	}

	// refreshTagWarnings re-validates the tag input on every keystroke
	// and updates the inline hint area below the field. Hidden when no
	// warnings; revealed with a list of `<tag> — <reason>` lines when
	// any tag would be invisible in Obsidian's tag panel. The cortex
	// itself accepts every input — this is purely UX guidance for the
	// Obsidian-authored workflow.
	private refreshTagWarnings(): void {
		const tags = splitDelimited(this.tagsInput.value);
		const warnings = validateObsidianTags(tags);
		this.tagWarningsEl.empty();
		if (warnings.length === 0) {
			this.tagWarningsEl.style.display = "none";
			return;
		}
		this.tagWarningsEl.style.display = "";
		for (const w of warnings) {
			const row = this.tagWarningsEl.createEl("div", {
				cls: "noema-create-tag-warning",
			});
			row.createEl("span", {
				cls: "noema-create-tag-warning-glyph",
				text: "⚠",
			});
			row.createEl("code", {
				cls: "noema-create-tag-warning-tag",
				text: w.tag,
			});
			row.appendChild(document.createTextNode(" — "));
			row.createEl("span", {
				cls: "noema-create-tag-warning-reason",
				text: w.reason,
			});
		}
	}

	private async submit(): Promise<void> {
		if (this.submitting) return;
		this.errorEl.style.display = "none";
		this.errorEl.empty();

		const title = this.titleInput.value.trim();
		if (!title) {
			this.showError("Title is required.");
			this.titleInput.focus();
			return;
		}
		const type = this.typeSelect.value as TraceType;
		const tags = splitDelimited(this.tagsInput.value);
		const derivedFrom = splitDelimited(this.derivedFromInput.value);

		const client = this.plugin.client;
		if (!client) {
			this.showError(
				"No noema endpoint configured. Set one in Settings → Noema."
			);
			return;
		}

		const params: CreateTraceParams = {
			title,
			type,
			// One-character placeholder body to satisfy the server's
			// required-body validation. The user fills the real
			// content in the editor afterwards. We use a NUL-equivalent
			// glyph that survives YAML/markdown parsing cleanly:
			// a single space.
			body: " ",
			author: this.plugin.settings.defaultAuthor || undefined,
			tags: tags.length > 0 ? tags : undefined,
			derivedFrom: derivedFrom.length > 0 ? derivedFrom : undefined,
		};

		this.submitting = true;
		this.submitBtn.disabled = true;
		this.submitBtn.setText("Creating…");

		try {
			const traceId = await client.createTrace(params);
			this.close();
			new Notice(`Created ${traceId}`);
			await this.openTraceFile(traceId);
		} catch (err) {
			const msg = err instanceof Error ? err.message : String(err);
			this.showError(`Couldn't create trace: ${msg}`);
			this.submitBtn.disabled = false;
			this.submitBtn.setText("Create");
		} finally {
			this.submitting = false;
		}
	}

	// openTraceFile waits for Obsidian to notice the new file (fsnotify
	// lag from the server-side write) and opens it in the active leaf.
	// Retry budget is short — if the file genuinely never appears the
	// caller saw the success notice anyway, so silently giving up is
	// preferable to a stuck loading state.
	private async openTraceFile(traceId: string): Promise<void> {
		const folder = this.plugin.settings.tracesFolder.replace(/\/+$/, "");
		const path = `${folder}/${traceId}.md`;
		for (let i = 0; i < 20; i++) {
			const file = this.app.vault.getFileByPath(path);
			if (file instanceof TFile) {
				await this.app.workspace.getLeaf(false).openFile(file);
				return;
			}
			await sleep(100);
		}
	}

	private showError(text: string): void {
		this.errorEl.setText(text);
		this.errorEl.style.display = "";
	}
}

// splitDelimited turns "go, language; architecture" into
// ["go", "language", "architecture"]. Tolerates either separator
// because users invariably mix them.
function splitDelimited(raw: string): string[] {
	if (!raw) return [];
	return raw
		.split(/[,;]/)
		.map((s) => s.trim())
		.filter(Boolean);
}

// TagWarning is a hint that a tag will be invisible or misinterpreted
// in Obsidian's tag panel. The cortex itself accepts every input;
// these warnings are purely for the Obsidian-authored workflow.
export interface TagWarning {
	tag: string;
	reason: string;
}

// validateObsidianTags returns a list of warnings for tags that won't
// render correctly in Obsidian's tag panel. Per Obsidian's tag spec
// (https://obsidian.md/help/tags#Tag+format):
//
//   - Tags must contain at least one non-numeric character. A tag
//     like "2026" is silently dropped from the tag panel.
//   - Dots inside a tag are interpreted as nested-tag separators
//     ("release.candidate" → tag tree "release" / "candidate"),
//     which is rarely what an author of a flat tag intends.
//
// Both forms are valid in the cortex — they're stored, indexed by
// FTS5, and searchable via MCP tools. The Obsidian-side limitation is
// purely about what Obsidian's UI surfaces. Exported so a future
// command-palette tag editor (or other plugin surface) can reuse the
// rule without duplicating the regex.
export function validateObsidianTags(tags: string[]): TagWarning[] {
	const warnings: TagWarning[] = [];
	for (const t of tags) {
		// Numeric-only check: any tag whose chars are all in the set
		// [0-9 - _] (i.e. no letter at all) is dropped by Obsidian.
		// Trailing-letter "v1" passes, "2026" doesn't, "2026q1" passes.
		if (/^[0-9_-]+$/.test(t)) {
			warnings.push({
				tag: t,
				reason:
					"Obsidian requires at least one letter. Pure-numeric tags are not surfaced in its tag panel.",
			});
		}
		if (t.includes(".")) {
			warnings.push({
				tag: t,
				reason:
					"Obsidian interprets dots as nested-tag separators (so \"a.b\" becomes tag tree a/b).",
			});
		}
	}
	return warnings;
}

function sleep(ms: number): Promise<void> {
	return new Promise((resolve) => setTimeout(resolve, ms));
}
