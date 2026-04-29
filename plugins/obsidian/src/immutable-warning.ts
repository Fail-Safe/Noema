import { App, MarkdownView, TFile } from "obsidian";
import type NoemaPlugin from "./main";

// ImmutableWarning manages a banner injected above the editor for any
// trace whose state means the cortex will reject in-editor edits:
//
//   - tier=long: long-tier traces are immutable except via tier_votes.
//     Editing the body and saving produces a content_hash mismatch
//     that the cortex's content-locked guard refuses.
//
//   - source_locked=true AND origin != local cortex name: the trace
//     came from a peer and is marked source-locked; the local cortex
//     will refuse Update/Trash/Remove. Edits made in Obsidian get
//     rolled back the next time the watcher reconciles.
//
// The banner is intentionally non-dismissable — these are persistent
// invariants of the trace, not transient notifications. A dismiss
// affordance would invite the user to forget and edit anyway, then
// be confused when their changes silently revert.
//
// Lifecycle: on active-leaf-change and on metadata-cache changes we
// re-evaluate the active file and either inject, update, or remove
// the banner. On plugin unload, removeAll() cleans up any lingering
// banner DOM so a reload doesn't leave orphan elements behind.
export class ImmutableWarning {
	private bannerEl: HTMLElement | null = null;
	private currentFile: TFile | null = null;

	constructor(private app: App, private plugin: NoemaPlugin) {}

	// refresh re-evaluates the currently active file and updates the
	// banner state. Safe to call as often as the host triggers UI
	// events; we cheaply skip work when nothing changed.
	refresh(): void {
		const view = this.app.workspace.getActiveViewOfType(MarkdownView);
		if (!view) {
			this.removeBanner();
			return;
		}
		const file = view.file;
		if (!file) {
			this.removeBanner();
			return;
		}
		const reason = this.shouldWarn(file);
		if (!reason) {
			this.removeBanner();
			return;
		}
		this.showBanner(view, file, reason);
	}

	removeAll(): void {
		this.removeBanner();
	}

	private shouldWarn(file: TFile): WarningReason | null {
		const cache = this.app.metadataCache.getFileCache(file);
		const fm = cache?.frontmatter;
		if (!fm) return null;

		// tier=long check first because it doesn't depend on knowing
		// our own cortex identity — the warning is correct regardless
		// of MCP connection state.
		const tier = typeof fm.tier === "string" ? fm.tier.toLowerCase() : "";
		if (tier === "long") {
			return { kind: "long-tier" };
		}

		// source_locked check requires knowing the local cortex name
		// to determine "is this origin foreign?". When disconnected,
		// plugin.cortexName is empty and we conservatively skip rather
		// than risk a misleading "from peer" label that points at our
		// own cortex.
		const sourceLocked = fm.source_locked === true || fm.source_locked === "true";
		const origin = typeof fm.origin === "string" ? fm.origin : "";
		const localCortex = this.plugin.cortexName;
		if (sourceLocked && origin && localCortex && origin !== localCortex) {
			return { kind: "source-locked", origin };
		}
		return null;
	}

	private showBanner(view: MarkdownView, file: TFile, reason: WarningReason): void {
		// If we already have a banner for this file with the same
		// reason, leave it alone — re-rendering on every event would
		// produce visible flicker. Cheap by-reference equality plus
		// reason kind covers the common case.
		if (
			this.bannerEl &&
			this.currentFile === file &&
			this.bannerEl.dataset.kind === reason.kind
		) {
			return;
		}
		this.removeBanner();

		const target = view.containerEl.querySelector(".view-content");
		if (!(target instanceof HTMLElement)) {
			// Layout shape changed in some Obsidian version we don't
			// recognize. Better to skip silently than throw — the
			// banner is a nice-to-have, not load-bearing.
			return;
		}

		const banner = document.createElement("div");
		banner.className = "noema-immutable-warning";
		banner.dataset.kind = reason.kind;

		const glyph = banner.createSpan({ cls: "noema-immutable-warning-glyph" });
		glyph.setText(reason.kind === "long-tier" ? "[L]" : "[locked]");

		const text = banner.createSpan({ cls: "noema-immutable-warning-text" });
		if (reason.kind === "long-tier") {
			text.setText(
				"Long-tier trace — immutable except via tier_votes. Edits will not be accepted by the cortex."
			);
		} else {
			text.setText(
				`Source-locked trace from peer "${reason.origin}" — local edits will not be accepted by the cortex.`
			);
		}

		target.prepend(banner);
		this.bannerEl = banner;
		this.currentFile = file;
	}

	private removeBanner(): void {
		if (this.bannerEl) {
			this.bannerEl.remove();
			this.bannerEl = null;
		}
		this.currentFile = null;
	}
}

// WarningReason is an intentional sum-type so showBanner can render
// reason-specific copy without a chain of optional fields. New reason
// kinds (e.g. archived, divergence, content_hash mismatch) plug in
// here without changing the call site.
type WarningReason =
	| { kind: "long-tier" }
	| { kind: "source-locked"; origin: string };
