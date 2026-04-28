import { Plugin, TFile, WorkspaceLeaf } from "obsidian";
import { DEFAULT_SETTINGS, NoemaSettings, NoemaSettingTab } from "./settings";
import { LineageView, LINEAGE_VIEW_TYPE } from "./lineage-view";
import { McpClient } from "./mcp-client";
import { readTraceMetadata, tierGlyph, tierLabel } from "./tier-status";
import { ImmutableWarning } from "./immutable-warning";

const STATUS_PING_INTERVAL_MS = 30_000;

// NoemaPlugin is the Obsidian-side entry point. It wires up:
//
//   - Settings panel (endpoint + bearer key + traces folder)
//   - Status bar item showing connection state and the active
//     trace's tier
//   - Lineage sidebar view (registered + opened on demand)
//   - A periodic cortex_identity ping so the connection state
//     stays accurate without the user having to re-trigger anything
//
// The plugin is fully degraded when the endpoint is unset — settings
// panel still works, status bar shows "noema: unconfigured", lineage
// view shows a hint to configure the endpoint. We never block plugin
// load on a successful connection.
export default class NoemaPlugin extends Plugin {
	settings: NoemaSettings = DEFAULT_SETTINGS;
	client: McpClient | null = null;
	// cortexName is exposed (not private) because immutable-warning
	// needs it to decide whether a source_locked trace's origin is
	// "foreign" (warn) or local (don't warn).
	cortexName = "";
	private statusBarEl: HTMLElement | null = null;
	private connected = false;
	private immutableWarning: ImmutableWarning | null = null;

	async onload(): Promise<void> {
		await this.loadSettings();
		this.refreshClient();

		this.statusBarEl = this.addStatusBarItem();
		this.statusBarEl.addClass("noema-status");
		this.renderStatus();

		this.addSettingTab(new NoemaSettingTab(this.app, this));

		this.registerView(LINEAGE_VIEW_TYPE, (leaf) => new LineageView(leaf, this));

		this.addCommand({
			id: "open-lineage-view",
			name: "Open lineage view",
			callback: () => this.activateLineageView(),
		});

		this.addCommand({
			id: "refresh-lineage",
			name: "Refresh lineage view",
			callback: async () => {
				const leaves = this.app.workspace.getLeavesOfType(LINEAGE_VIEW_TYPE);
				for (const leaf of leaves) {
					const view = leaf.view;
					if (view instanceof LineageView) {
						await view.refreshFromActive();
					}
				}
			},
		});

		this.immutableWarning = new ImmutableWarning(this.app, this);

		// Re-render the status bar AND immutable-warning banner when
		// the active file changes (tier glyph follows the user) or
		// when frontmatter changes (a tier promotion to long would
		// flip the warning state without an active-leaf-change).
		this.registerEvent(
			this.app.workspace.on("active-leaf-change", () => {
				this.renderStatus();
				this.immutableWarning?.refresh();
			})
		);
		this.registerEvent(
			this.app.metadataCache.on("changed", () => {
				this.renderStatus();
				this.immutableWarning?.refresh();
			})
		);
		// Initial render in case a trace file is already open at
		// plugin start.
		this.immutableWarning.refresh();

		// Initial connection probe + periodic ping. We don't block
		// onload on this — a missing or unreachable endpoint at
		// startup should still let the plugin load cleanly so the
		// settings UI is reachable.
		this.pingConnection();
		this.registerInterval(
			window.setInterval(() => this.pingConnection(), STATUS_PING_INTERVAL_MS)
		);
	}

	onunload(): void {
		// Don't manually detach the lineage leaf here — Obsidian
		// preserves view state across plugin reloads when the plugin
		// re-registers the same view type, which is a better UX than
		// closing the panel on every plugin restart during dev.

		// Clean up any lingering banner DOM so a plugin reload during
		// active development doesn't leave orphan elements behind.
		this.immutableWarning?.removeAll();
	}

	async loadSettings(): Promise<void> {
		const stored = (await this.loadData()) as Partial<NoemaSettings> | null;
		this.settings = { ...DEFAULT_SETTINGS, ...(stored ?? {}) };
	}

	async saveSettings(): Promise<void> {
		await this.saveData(this.settings);
	}

	// refreshClient is called from the settings tab when the
	// endpoint or bearer key changes. We keep a single McpClient
	// instance so the JSON-RPC id counter stays monotonic across
	// config edits — useful for any future debugging that correlates
	// request IDs across a session.
	refreshClient(): void {
		if (!this.settings.endpoint) {
			this.client = null;
			this.connected = false;
			this.renderStatus();
			return;
		}
		if (this.client) {
			this.client.updateConfig(this.settings.endpoint, this.settings.bearerKey);
		} else {
			this.client = new McpClient(this.settings.endpoint, this.settings.bearerKey);
		}
		// Probe immediately on config change so the user sees
		// success or failure feedback in the status bar without
		// waiting up to 30s for the next interval tick.
		this.pingConnection();
	}

	private async pingConnection(): Promise<void> {
		if (!this.client) {
			this.connected = false;
			this.cortexName = "";
			this.renderStatus();
			return;
		}
		try {
			const id = await this.client.cortexIdentity();
			this.connected = true;
			this.cortexName = id.name;
		} catch {
			this.connected = false;
		}
		this.renderStatus();
	}

	private renderStatus(): void {
		if (!this.statusBarEl) return;
		this.statusBarEl.empty();

		// Tier glyph for the currently-open trace, if any. Tier
		// state lives entirely in the local file's frontmatter, so
		// we can render it even when disconnected from the MCP
		// endpoint — that's by design, since tier visibility is
		// useful offline.
		const file = this.app.workspace.getActiveFile();
		if (file instanceof TFile) {
			const meta = readTraceMetadata(this.app, file);
			if (meta?.id) {
				const glyph = this.statusBarEl.createSpan({ cls: "noema-status-glyph" });
				glyph.setText(`[${tierGlyph(meta.tier)}]`);
				glyph.setAttr("title", tierLabel(meta.tier));
				this.statusBarEl.createSpan({
					cls: "noema-status-divider",
					text: " ",
				});
			}
		}

		const conn = this.statusBarEl.createSpan({ cls: "noema-status-conn" });
		if (!this.settings.endpoint) {
			conn.setText("noema: unconfigured");
			conn.setAttr("title", "Set an HTTP endpoint in settings to connect.");
			return;
		}
		if (this.connected) {
			conn.setText(`noema: ${this.cortexName || "connected"}`);
			conn.setAttr("title", `Connected to ${this.settings.endpoint}`);
			conn.addClass("noema-status-ok");
		} else {
			conn.setText("noema: disconnected");
			conn.setAttr("title", `Couldn't reach ${this.settings.endpoint}`);
			conn.addClass("noema-status-err");
		}
	}

	private async activateLineageView(): Promise<void> {
		const { workspace } = this.app;
		let leaf: WorkspaceLeaf | null = workspace.getLeavesOfType(LINEAGE_VIEW_TYPE)[0] ?? null;
		if (!leaf) {
			leaf = workspace.getRightLeaf(false);
			if (!leaf) {
				leaf = workspace.getLeaf(true);
			}
			await leaf.setViewState({ type: LINEAGE_VIEW_TYPE, active: true });
		}
		workspace.revealLeaf(leaf);
	}
}
