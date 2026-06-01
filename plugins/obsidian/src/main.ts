import { Notice, Plugin, TFile, WorkspaceLeaf } from "obsidian";
import { DEFAULT_SETTINGS, NoemaSettings, NoemaSettingTab } from "./settings";
import { LineageView, LINEAGE_VIEW_TYPE } from "./lineage-view";
import { McpClient, UnauthorizedError } from "./mcp-client";
import { readTraceMetadata, tierGlyph, tierLabel } from "./tier-status";
import { CreateTraceModal } from "./create-modal";
import { ImmutableWarning } from "./immutable-warning";
import { openAppendModalFromActive } from "./append-modal";

const STATUS_PING_INTERVAL_MS = 30_000;

// ConnState is the outcome of the most recent cortex_identity probe.
// "unauthorized" is a first-class state rather than a flavor of
// "disconnected" because the remedy is different and we want to nudge
// the user toward it (see setConnState's one-shot Notice).
type ConnState = "connected" | "disconnected" | "unauthorized";

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
	// connState tracks the last probe result. "unauthorized" is split
	// out from "disconnected" so a rejected/missing bearer key reads as
	// a credential problem (actionable: fix the key) rather than as an
	// unreachable server (actionable: check the endpoint/network).
	private connState: ConnState = "disconnected";
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

		this.addCommand({
			id: "create-trace",
			name: "New trace",
			callback: () => {
				new CreateTraceModal(this.app, this).open();
			},
		});

		// Append-to-trace uses checkCallback so the command is greyed
		// out in the palette unless the active editor is a trace.
		// That's the right UX hint: "no trace open" reads as an
		// absent/disabled command rather than as an error message
		// after the user pulls the trigger.
		this.addCommand({
			id: "append-to-trace",
			name: "Append to current trace",
			checkCallback: (checking: boolean) => {
				const file = this.app.workspace.getActiveFile();
				if (!file) return false;
				// Defer the metadata read until we know we'll act on it.
				if (checking) {
					// Cheap approximation — let the modal helper do the
					// authoritative metadata check. We only need to
					// know "is the active file a candidate?" here.
					return file.extension === "md";
				}
				if (!openAppendModalFromActive(this)) {
					new Notice("Open a trace first to append to it.");
				}
				return true;
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
			this.connState = "disconnected";
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

	// probe runs one cortex_identity round-trip and classifies the
	// outcome, updating cortexName as a side effect. It does NOT touch
	// the status bar or fire notices — callers decide how to surface the
	// result. Both the background ping and the settings "Test
	// connection" button build on it so they classify failures
	// identically.
	private async probe(): Promise<ConnState> {
		if (!this.client) {
			this.cortexName = "";
			return "disconnected";
		}
		try {
			const id = await this.client.cortexIdentity();
			this.cortexName = id.name;
			return "connected";
		} catch (err) {
			this.cortexName = "";
			// A bearer-key rejection is distinct from "server's not
			// there" — the AuthMiddleware 401 surfaces as
			// UnauthorizedError, everything else (DNS, TLS, refused
			// connection, 5xx) is a plain disconnect.
			return err instanceof UnauthorizedError ? "unauthorized" : "disconnected";
		}
	}

	private async pingConnection(): Promise<void> {
		this.setConnState(await this.probe());
		this.renderStatus();
	}

	// testConnection is the explicit, user-triggered probe behind the
	// settings "Test connection" button. Unlike the passive ping, it
	// reports an outcome on every invocation — that's the whole point of
	// a test button — so it writes connState directly rather than
	// through setConnState, whose Notice fires only on transitions (which
	// would leave a repeat click on an already-unauthorized server
	// silent). The status bar still stays in sync.
	async testConnection(): Promise<void> {
		if (!this.settings.endpoint) {
			new Notice("Noema: set an HTTP endpoint first.");
			return;
		}
		// The settings onChange handlers keep the client current, but a
		// freshly-opened settings tab with a saved endpoint may not have
		// constructed one yet if the endpoint was unset at load.
		if (!this.client) {
			this.client = new McpClient(this.settings.endpoint, this.settings.bearerKey);
		}
		const state = await this.probe();
		this.connState = state;
		this.renderStatus();
		switch (state) {
			case "connected":
				new Notice(`Noema: connected to ${this.cortexName || this.settings.endpoint}.`);
				break;
			case "unauthorized":
				new Notice(
					this.settings.bearerKey
						? `Noema: ${this.settings.endpoint} rejected the bearer key (HTTP 401). Check the key below.`
						: `Noema: ${this.settings.endpoint} requires a bearer key (HTTP 401). Set one below.`,
					8000
				);
				break;
			case "disconnected":
				new Notice(`Noema: couldn't reach ${this.settings.endpoint}.`);
				break;
		}
	}

	// setConnState updates the cached state and, on the *transition*
	// into "unauthorized", fires a single Notice. Gating on the
	// transition (rather than the state) keeps the 30s ping loop from
	// re-toasting the same rejection every tick — the user gets one
	// actionable nudge when the auth failure first appears, and again
	// only if they recover and then break it again.
	private setConnState(next: ConnState): void {
		const prev = this.connState;
		this.connState = next;
		if (next === "unauthorized" && prev !== "unauthorized") {
			const detail = this.settings.bearerKey
				? "the bearer key was rejected"
				: "this server requires a bearer key";
			new Notice(
				`Noema: ${detail} (HTTP 401). Update it in the Noema plugin settings.`,
				8000
			);
		}
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
		if (this.connState === "connected") {
			conn.setText(`noema: ${this.cortexName || "connected"}`);
			conn.setAttr("title", `Connected to ${this.settings.endpoint}`);
			conn.addClass("noema-status-ok");
		} else if (this.connState === "unauthorized") {
			conn.setText("noema: unauthorized");
			conn.setAttr(
				"title",
				`${this.settings.endpoint} rejected the bearer key (HTTP 401). Check the key in Noema settings.`
			);
			conn.addClass("noema-status-err");
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
