import { App, normalizePath, Plugin, TFile } from "obsidian";
import { readTraceMetadata, tierGlyph, tierLabel } from "./tier-status";

const FILE_EXPLORER_VIEW_TYPE = "file-explorer";
const FILE_ROW_SELECTOR = ".nav-file-title[data-path]";
const BADGE_CLASS = "noema-file-tier-badge";

// FileExplorerTierBadges adds tier visibility to Obsidian's native file
// explorer without taking ownership of its tree. Obsidian does not expose a
// public row-render hook, so the DOM selectors are deliberately isolated in
// this class. Metadata and lifecycle updates still use supported APIs.
export class FileExplorerTierBadges {
	private readonly observers = new Map<HTMLElement, MutationObserver>();
	private active = false;
	private refreshFrame: number | null = null;

	constructor(
		private readonly app: App,
		private readonly plugin: Plugin,
		private readonly getTracesFolder: () => string,
		private readonly getEnabled: () => boolean
	) {}

	start(): void {
		this.active = true;
		this.app.workspace.onLayoutReady(() => {
			if (!this.active) return;
			this.bindExplorerViews();

			this.plugin.registerEvent(
				this.app.workspace.on("layout-change", () => this.bindExplorerViews())
			);
			this.plugin.registerEvent(
				this.app.metadataCache.on("changed", (file) => this.refreshFile(file.path))
			);
			this.plugin.registerEvent(this.app.vault.on("create", () => this.refresh()));
			this.plugin.registerEvent(this.app.vault.on("rename", () => this.refresh()));
			this.plugin.registerEvent(this.app.vault.on("delete", () => this.refresh()));
		});
	}

	stop(): void {
		this.active = false;
		if (this.refreshFrame !== null) {
			window.cancelAnimationFrame(this.refreshFrame);
			this.refreshFrame = null;
		}
		for (const [root, observer] of this.observers) {
			observer.disconnect();
			this.removeBadges(root);
		}
		this.observers.clear();
	}

	refresh(): void {
		if (!this.active || this.refreshFrame !== null) return;
		this.refreshFrame = window.requestAnimationFrame(() => {
			this.refreshFrame = null;
			for (const root of this.observers.keys()) {
				root
					.querySelectorAll<HTMLElement>(FILE_ROW_SELECTOR)
					.forEach((row) => this.decorateRow(row));
			}
		});
	}

	private bindExplorerViews(): void {
		const currentRoots = new Set(
			this.app.workspace
				.getLeavesOfType(FILE_EXPLORER_VIEW_TYPE)
				.map((leaf) => leaf.view.containerEl)
		);

		for (const [root, observer] of this.observers) {
			if (currentRoots.has(root)) continue;
			observer.disconnect();
			this.removeBadges(root);
			this.observers.delete(root);
		}

		for (const root of currentRoots) {
			if (this.observers.has(root)) continue;
			const observer = new MutationObserver(() => this.refresh());
			observer.observe(root, { childList: true, subtree: true });
			this.observers.set(root, observer);
		}

		this.refresh();
	}

	private refreshFile(path: string): void {
		if (!this.active) return;
		for (const root of this.observers.keys()) {
			root.querySelectorAll<HTMLElement>(FILE_ROW_SELECTOR).forEach((row) => {
				if (row.dataset.path === path) this.decorateRow(row);
			});
		}
	}

	private decorateRow(row: HTMLElement): void {
		if (!this.getEnabled()) {
			this.removeBadge(row);
			return;
		}

		const path = row.dataset.path;
		const folder = normalizePath(this.getTracesFolder().trim() || "traces").replace(/\/$/, "");
		if (!path || !path.startsWith(`${folder}/`)) {
			this.removeBadge(row);
			return;
		}

		const file = this.app.vault.getAbstractFileByPath(path);
		if (!(file instanceof TFile) || file.extension !== "md") {
			this.removeBadge(row);
			return;
		}

		const metadata = readTraceMetadata(this.app, file);
		if (!metadata?.id) {
			this.removeBadge(row);
			return;
		}

		let badge = row.querySelector<HTMLElement>(`.${BADGE_CLASS}`);
		if (!badge) {
			badge = document.createElement("span");
			badge.classList.add(BADGE_CLASS);
			const title = row.querySelector<HTMLElement>(".nav-file-title-content");
			if (title) title.before(badge);
			else row.prepend(badge);
		}

		const glyph = `[${tierGlyph(metadata.tier)}]`;
		const label = tierLabel(metadata.tier);
		badge.className = `${BADGE_CLASS} noema-file-tier-${metadata.tier}`;
		if (badge.textContent !== glyph) badge.textContent = glyph;
		badge.dataset.tier = metadata.tier;
		badge.setAttribute("title", label);
		badge.setAttribute("aria-label", label);
	}

	private removeBadge(row: HTMLElement): void {
		row.querySelector(`.${BADGE_CLASS}`)?.remove();
	}

	private removeBadges(root: HTMLElement): void {
		root.querySelectorAll(`.${BADGE_CLASS}`).forEach((badge) => badge.remove());
	}
}
