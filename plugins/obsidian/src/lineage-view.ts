import { ItemView, WorkspaceLeaf, TFile, Notice } from "obsidian";
import type NoemaPlugin from "./main";
import { readTraceMetadata, tierGlyph, tierLabel, Tier } from "./tier-status";
import type { Lineage } from "./mcp-client";

export const LINEAGE_VIEW_TYPE = "noema-lineage-view";

// LineageView is a sidebar showing the trace's derivation graph:
// ancestors (`derived_from`) above, the current trace in the middle,
// descendants below. Each entry is clickable and opens the
// corresponding trace file. Updates automatically when the active
// editor changes to a different trace.
//
// We deliberately don't re-render anything when the user is editing
// trace content — only when the active file itself changes. The
// user's typing shouldn't cause re-fetches against the MCP server.
export class LineageView extends ItemView {
	private currentTraceId: string | null = null;
	private bodyEl: HTMLElement | null = null;

	constructor(leaf: WorkspaceLeaf, private plugin: NoemaPlugin) {
		super(leaf);
	}

	getViewType(): string {
		return LINEAGE_VIEW_TYPE;
	}

	getDisplayText(): string {
		return "Noema lineage";
	}

	getIcon(): string {
		return "git-merge";
	}

	async onOpen(): Promise<void> {
		this.contentEl.empty();
		this.contentEl.addClass("noema-lineage-view");

		const header = this.contentEl.createEl("div", { cls: "noema-lineage-header" });
		header.createEl("h3", { text: "Lineage" });

		this.bodyEl = this.contentEl.createEl("div", { cls: "noema-lineage-body" });
		this.renderEmpty("Open a trace to see its lineage.");

		// Re-render when the active editor changes file. We intentionally
		// don't subscribe to "file-modified" events — typing shouldn't
		// kick off MCP calls. Frontmatter changes that affect lineage
		// (derived_from edits) DO take effect on file save, but the
		// user can refresh manually with the command palette command.
		this.registerEvent(
			this.app.workspace.on("active-leaf-change", () => {
				this.refreshFromActive();
			})
		);

		// Initial render based on whichever file is open right now.
		await this.refreshFromActive();
	}

	async refreshFromActive(): Promise<void> {
		const active = this.app.workspace.getActiveFile();
		if (!active) {
			this.renderEmpty("No file is open.");
			return;
		}
		const meta = readTraceMetadata(this.app, active);
		if (!meta?.id) {
			this.renderEmpty("This file isn't a trace (no `id` in frontmatter).");
			this.currentTraceId = null;
			return;
		}
		if (meta.id === this.currentTraceId) {
			// Already rendered this trace; avoid a redundant fetch
			// when the user toggles between this and a non-trace
			// file (e.g. README.md) and back.
			return;
		}
		this.currentTraceId = meta.id;
		await this.fetchAndRender(meta.id, meta.title ?? "(untitled)", meta.tier);
	}

	private async fetchAndRender(traceId: string, title: string, tier: Tier): Promise<void> {
		if (!this.bodyEl) return;
		this.bodyEl.empty();
		this.bodyEl.createEl("div", {
			cls: "noema-lineage-loading",
			text: `Loading lineage for ${traceId}…`,
		});

		const client = this.plugin.client;
		if (!client) {
			this.renderEmpty("Configure the noema HTTP endpoint in settings to load lineage.");
			return;
		}

		let lineage: Lineage;
		try {
			lineage = await client.traceLineage(traceId);
		} catch (err) {
			this.renderError(`Couldn't load lineage: ${err instanceof Error ? err.message : String(err)}`);
			return;
		}

		this.renderLineage(traceId, title, tier, lineage);
	}

	private renderLineage(traceId: string, title: string, tier: Tier, lineage: Lineage): void {
		if (!this.bodyEl) return;
		this.bodyEl.empty();

		// Ancestors first — they're "above" this trace in the
		// derivation tree, so render them at the top to match the
		// reader's intuition for "where did this come from".
		this.renderSection("Derived from", lineage.derivedFrom);

		// Current trace as a centerpiece. Not clickable — you're
		// already looking at it.
		const center = this.bodyEl.createEl("div", { cls: "noema-lineage-current" });
		center.createEl("div", { cls: "noema-lineage-current-glyph", text: `[${tierGlyph(tier)}]` });
		center.createEl("div", { cls: "noema-lineage-current-title", text: title });
		center.createEl("div", { cls: "noema-lineage-current-id", text: traceId });
		center.setAttr("title", tierLabel(tier));

		this.renderSection("Derived by", lineage.derivedBy);

		if (lineage.derivedFrom.length === 0 && lineage.derivedBy.length === 0) {
			this.bodyEl.createEl("div", {
				cls: "noema-lineage-empty",
				text: "This trace has no recorded derivations.",
			});
		}
	}

	private renderSection(label: string, ids: string[]): void {
		if (!this.bodyEl) return;
		const section = this.bodyEl.createEl("div", { cls: "noema-lineage-section" });
		section.createEl("div", { cls: "noema-lineage-section-label", text: label });
		if (ids.length === 0) {
			section.createEl("div", { cls: "noema-lineage-section-empty", text: "(none)" });
			return;
		}
		const list = section.createEl("div", { cls: "noema-lineage-section-list" });
		for (const id of ids) {
			const file = this.findTraceFile(id);
			const meta = file ? readTraceMetadata(this.app, file) : null;
			const tier = meta?.tier ?? "unknown";
			const title = meta?.title ?? "(unknown)";

			const row = list.createEl("div", { cls: "noema-lineage-row" });
			row.createEl("span", { cls: "noema-lineage-glyph", text: `[${tierGlyph(tier)}]` });
			const link = row.createEl("a", {
				cls: "noema-lineage-link",
				text: title,
				href: "#",
			});
			link.setAttr("data-trace-id", id);
			link.setAttr("title", `${id} (${tierLabel(tier)})`);
			row.appendChild(document.createTextNode(" "));
			row.createEl("span", { cls: "noema-lineage-id", text: id });

			link.addEventListener("click", (evt) => {
				evt.preventDefault();
				if (file) {
					this.app.workspace.getLeaf(false).openFile(file);
				} else {
					new Notice(`Trace ${id} not found in this vault.`);
				}
			});
		}
	}

	// findTraceFile resolves a trace ID to its TFile by looking under
	// the configured traces folder. Returns null if the file isn't in
	// the vault, which can happen when a derived_from points at a
	// trace that lives only on a remote peer (federation). The UI
	// surfaces that as a non-clickable row with a notice.
	private findTraceFile(traceId: string): TFile | null {
		const folder = this.plugin.settings.tracesFolder;
		const path = `${folder.replace(/\/+$/, "")}/${traceId}.md`;
		const f = this.app.vault.getFileByPath(path);
		return f instanceof TFile ? f : null;
	}

	private renderEmpty(text: string): void {
		if (!this.bodyEl) return;
		this.bodyEl.empty();
		this.bodyEl.createEl("div", { cls: "noema-lineage-empty", text });
	}

	private renderError(text: string): void {
		if (!this.bodyEl) return;
		this.bodyEl.empty();
		this.bodyEl.createEl("div", { cls: "noema-lineage-error", text });
	}
}
