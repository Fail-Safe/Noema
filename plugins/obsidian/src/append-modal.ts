import { App, Modal, Notice, TFile } from "obsidian";
import type NoemaPlugin from "./main";
import { readTraceMetadata, tierGlyph } from "./tier-status";

// AppendModal lets the user add content to an existing trace's body
// from inside Obsidian. It's the editor-side counterpart to the
// `noema append` CLI command and the `append_trace` MCP tool, both
// designed for the "running journal" / "fire-and-forget log" pattern
// where you want to extend a trace without reading and replacing its
// full current body.
//
// Body content IS in this modal (unlike CreateTraceModal, which
// deliberately punts body to the editor). The reason is shape: append
// content is bounded — usually a sentence or a paragraph — and the
// whole point of the operation is to commit it in one shot without a
// round-trip through the editor's read/write cycle. A textarea is
// the right fit.
//
// The target trace is fixed at construction time from the active
// editor; the modal does not include a trace picker. If the user
// wants to append to a different trace they switch files first and
// run the command again. This keeps the modal small and forecloses
// the failure mode where someone has the right textarea content but
// the wrong trace selected.
export class AppendModal extends Modal {
	private contentInput!: HTMLTextAreaElement;
	private submitBtn!: HTMLButtonElement;
	private errorEl!: HTMLElement;
	private submitting = false;

	constructor(
		app: App,
		private plugin: NoemaPlugin,
		private targetFile: TFile,
		private targetId: string,
		private targetTitle: string,
		private targetTier: string
	) {
		super(app);
	}

	onOpen(): void {
		const { contentEl } = this;
		contentEl.empty();
		contentEl.addClass("noema-append-modal");

		this.titleEl.setText("Append to trace");

		// Header showing which trace we're appending to. Read-only
		// summary, not a picker — the modal binds to the active
		// trace at open time and stays bound for the duration.
		const targetEl = contentEl.createEl("div", { cls: "noema-append-target" });
		const tierEl = targetEl.createEl("span", { cls: "noema-append-target-glyph" });
		tierEl.setText(`[${tierGlyph(this.targetTier as any)}]`);
		const titleEl = targetEl.createEl("span", { cls: "noema-append-target-title" });
		titleEl.setText(this.targetTitle);
		const idEl = targetEl.createEl("div", { cls: "noema-append-target-id" });
		idEl.setText(this.targetId);

		// Textarea for the content to append. Sized for short-form
		// content (a few lines) but resizable so longer appends are
		// fine — that's the user's call.
		const labelRow = contentEl.createEl("div", { cls: "noema-append-label" });
		labelRow.setText("Content to append");

		this.contentInput = contentEl.createEl("textarea", {
			cls: "noema-append-content",
			attr: { rows: "6", placeholder: "Type or paste content to append…" },
		});

		const hint = contentEl.createEl("div", { cls: "noema-append-hint" });
		hint.setText(
			"A newline is inserted automatically between the existing body and your content if needed. ⌘+Enter to submit."
		);

		this.errorEl = contentEl.createEl("div", { cls: "noema-append-error" });
		this.errorEl.style.display = "none";

		const buttonRow = contentEl.createEl("div", { cls: "noema-append-buttons" });
		const cancelBtn = buttonRow.createEl("button", { text: "Cancel" });
		cancelBtn.addEventListener("click", () => this.close());
		this.submitBtn = buttonRow.createEl("button", {
			text: "Append",
			cls: "mod-cta",
		});
		this.submitBtn.addEventListener("click", () => this.submit());

		// ⌘+Enter / Ctrl+Enter to submit — common power-user shortcut
		// for "send" actions in textareas. Plain Enter inserts a
		// newline as expected.
		this.contentInput.addEventListener("keydown", (evt) => {
			if (evt.key === "Enter" && (evt.metaKey || evt.ctrlKey)) {
				evt.preventDefault();
				this.submit();
			}
		});

		setTimeout(() => this.contentInput.focus(), 0);
	}

	onClose(): void {
		this.contentEl.empty();
	}

	private async submit(): Promise<void> {
		if (this.submitting) return;
		this.errorEl.style.display = "none";
		this.errorEl.empty();

		const content = this.contentInput.value;
		// Trim only trailing whitespace — leading whitespace might
		// be intentional (e.g. continuing a code block, or formatting
		// a list item). The cortex's Append handles its own newline
		// normalisation between the existing body and our content.
		const trimmed = content.replace(/\s+$/, "");
		if (!trimmed) {
			this.showError("Content is required.");
			this.contentInput.focus();
			return;
		}

		const client = this.plugin.client;
		if (!client) {
			this.showError(
				"No noema endpoint configured. Set one in Settings → Noema."
			);
			return;
		}

		this.submitting = true;
		this.submitBtn.disabled = true;
		this.submitBtn.setText("Appending…");

		try {
			await client.appendTrace(this.targetId, trimmed);
			this.close();
			new Notice(`Appended to ${this.targetId}`);
			// Don't re-open the file — Obsidian's watcher will pick up
			// the change once the cortex's filesystem watcher writes
			// the new content_hash and updated_at, and the editor
			// already shows the file. A manual focus shift would
			// fight whatever the user does next.
		} catch (err) {
			const msg = err instanceof Error ? err.message : String(err);
			this.showError(`Couldn't append: ${msg}`);
			this.submitBtn.disabled = false;
			this.submitBtn.setText("Append");
		} finally {
			this.submitting = false;
		}
	}

	private showError(text: string): void {
		this.errorEl.setText(text);
		this.errorEl.style.display = "";
	}
}

// openAppendModalFromActive resolves the currently-active editor leaf
// to a trace and opens the modal pointed at it. Returns false if the
// active leaf isn't a trace — the caller (a command checkCallback)
// uses that to grey out the command in the palette so the operator
// sees "no trace open" by absence rather than by error message.
export function openAppendModalFromActive(plugin: NoemaPlugin): boolean {
	const file = plugin.app.workspace.getActiveFile();
	if (!(file instanceof TFile)) return false;
	const meta = readTraceMetadata(plugin.app, file);
	if (!meta?.id) return false;
	new AppendModal(
		plugin.app,
		plugin,
		file,
		meta.id,
		meta.title ?? "(untitled)",
		meta.tier
	).open();
	return true;
}
