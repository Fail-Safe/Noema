import { App, Modal, Notice, TFile } from "obsidian";
import type NoemaPlugin from "./main";
import type { SearchMode, SearchResult } from "./mcp-client";

export class SearchModal extends Modal {
	private queryInput!: HTMLInputElement;
	private modeSelect!: HTMLSelectElement;
	private limitSelect!: HTMLSelectElement;
	private resultEl!: HTMLElement;
	private errorEl!: HTMLElement;
	private searchBtn!: HTMLButtonElement;
	private searching = false;

	constructor(app: App, private plugin: NoemaPlugin) {
		super(app);
	}

	onOpen(): void {
		const { contentEl } = this;
		contentEl.empty();
		contentEl.addClass("noema-search-modal");

		this.titleEl.setText("Search traces");

		const controls = contentEl.createEl("div", { cls: "noema-search-controls" });
		this.queryInput = controls.createEl("input", {
			cls: "noema-search-query",
			attr: {
				type: "search",
				placeholder: "Search traces",
			},
		});
		this.modeSelect = controls.createEl("select", { cls: "noema-search-mode" });
		for (const mode of ["hybrid", "semantic", "lexical"] as SearchMode[]) {
			const opt = this.modeSelect.createEl("option", { text: mode });
			opt.value = mode;
		}
		this.modeSelect.value = this.plugin.settings.searchMode;
		this.limitSelect = controls.createEl("select", { cls: "noema-search-limit" });
		for (const n of [5, 10]) {
			const opt = this.limitSelect.createEl("option", { text: `${n} results` });
			opt.value = String(n);
		}
		this.limitSelect.value = "5";
		this.searchBtn = controls.createEl("button", {
			text: "Search",
			cls: "mod-cta noema-search-submit",
		});

		this.errorEl = contentEl.createEl("div", { cls: "noema-search-error" });
		this.errorEl.style.display = "none";
		this.resultEl = contentEl.createEl("div", { cls: "noema-search-results" });

		this.searchBtn.addEventListener("click", () => this.search());
		this.queryInput.addEventListener("keydown", (evt) => {
			if (evt.key === "Enter") {
				evt.preventDefault();
				this.search();
			}
		});

		setTimeout(() => this.queryInput.focus(), 0);
	}

	onClose(): void {
		this.contentEl.empty();
	}

	private async search(): Promise<void> {
		if (this.searching) return;
		this.hideError();
		this.resultEl.empty();

		const query = this.queryInput.value.trim();
		if (!query) {
			this.showError("Search query is required.");
			this.queryInput.focus();
			return;
		}
		const client = this.plugin.client;
		if (!client) {
			this.showError("No noema endpoint configured. Set one in Settings -> Noema.");
			return;
		}

		this.searching = true;
		this.searchBtn.disabled = true;
		this.searchBtn.setText("Searching...");
		try {
			const mode = this.modeSelect.value as SearchMode;
			const rows = await client.searchTraces(query, mode);
			const limit = Number.parseInt(this.limitSelect.value, 10) || 5;
			this.renderResults(rows.slice(0, limit), rows.length);
		} catch (err) {
			const msg = err instanceof Error ? err.message : String(err);
			this.showError(`Search failed: ${msg}`);
		} finally {
			this.searching = false;
			this.searchBtn.disabled = false;
			this.searchBtn.setText("Search");
		}
	}

	private renderResults(rows: SearchResult[], total: number): void {
		this.resultEl.empty();
		if (rows.length === 0) {
			this.resultEl.createEl("div", {
				cls: "noema-search-empty",
				text: "No matching traces.",
			});
			return;
		}
		if (total > rows.length) {
			this.resultEl.createEl("div", {
				cls: "noema-search-count",
				text: `Showing ${rows.length} of ${total} results`,
			});
		}
		for (const row of rows) {
			const item = this.resultEl.createEl("button", { cls: "noema-search-result" });
			item.type = "button";
			const title = this.localTitle(row.id) ?? row.title;
			item.createEl("div", { cls: "noema-search-result-title", text: title });
			item.createEl("div", { cls: "noema-search-result-id", text: row.id });
			const meta = item.createEl("div", { cls: "noema-search-result-meta" });
			if (row.type) meta.createEl("span", { text: row.type });
			if (row.author) meta.createEl("span", { text: row.author });
			if (row.created) meta.createEl("span", { text: row.created });
			item.addEventListener("click", () => this.openTrace(row.id));
		}
	}

	private async openTrace(traceId: string): Promise<void> {
		const folder = this.plugin.settings.tracesFolder.replace(/\/+$/, "");
		const file = this.app.vault.getFileByPath(`${folder}/${traceId}.md`);
		if (!(file instanceof TFile)) {
			new Notice(`Noema: ${traceId}.md was not found in ${folder}.`);
			return;
		}
		await this.app.workspace.getLeaf(false).openFile(file);
		this.close();
	}

	private localTitle(traceId: string): string | null {
		const folder = this.plugin.settings.tracesFolder.replace(/\/+$/, "");
		const file = this.app.vault.getFileByPath(`${folder}/${traceId}.md`);
		if (!(file instanceof TFile)) return null;
		const cache = this.app.metadataCache.getFileCache(file);
		const title = cache?.frontmatter?.title;
		return typeof title === "string" && title.trim() ? title.trim() : null;
	}

	private showError(text: string): void {
		this.errorEl.setText(text);
		this.errorEl.style.display = "";
	}

	private hideError(): void {
		this.errorEl.empty();
		this.errorEl.style.display = "none";
	}
}
