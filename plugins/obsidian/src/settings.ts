import { App, PluginSettingTab, Setting } from "obsidian";
import type NoemaPlugin from "./main";

export interface NoemaSettings {
	endpoint: string;
	bearerKey: string;
	tracesFolder: string;
	defaultAuthor: string;
	searchMode: "lexical" | "semantic" | "hybrid";
}

// DEFAULT_SETTINGS deliberately leave endpoint and bearerKey empty so
// the plugin starts in "disconnected" state on a fresh install rather
// than blindly trying to reach a localhost URL. tracesFolder defaults
// to "traces" because that's the cortex layout convention; users with
// a non-standard layout (rare) can override. defaultAuthor is empty
// by default — the create-trace flow omits the author field entirely
// when the setting is empty, letting the cortex's own author logic
// decide what to record.
export const DEFAULT_SETTINGS: NoemaSettings = {
	endpoint: "",
	bearerKey: "",
	tracesFolder: "traces",
	defaultAuthor: "",
	searchMode: "hybrid",
};

export class NoemaSettingTab extends PluginSettingTab {
	constructor(app: App, private plugin: NoemaPlugin) {
		super(app, plugin);
	}

	display(): void {
		const { containerEl } = this;
		containerEl.empty();

		containerEl.createEl("h2", { text: "Noema" });
		containerEl.createEl("p", {
			text: "Connect this Obsidian vault to a running noema serve --transport http endpoint to see lineage and tier metadata for traces. The vault should be opened on the cortex root (the directory containing cortex.md).",
		});

		new Setting(containerEl)
			.setName("HTTP endpoint")
			.setDesc("Base URL of the noema HTTP server, e.g. https://noema.local:3000")
			.addText((text) =>
				text
					.setPlaceholder("https://hostname:3000")
					.setValue(this.plugin.settings.endpoint)
					.onChange(async (value) => {
						this.plugin.settings.endpoint = value.trim();
						await this.plugin.saveSettings();
						this.plugin.refreshClient();
					})
			);

		new Setting(containerEl)
			.setName("Bearer key")
			.setDesc("Shared MCP access key (NOEMA_MCP_KEY). Required for keyed-mode servers; leave empty for open-mode (loopback only).")
			.addText((text) => {
				text
					.setPlaceholder("(secret)")
					.setValue(this.plugin.settings.bearerKey)
					.onChange(async (value) => {
						this.plugin.settings.bearerKey = value;
						await this.plugin.saveSettings();
						this.plugin.refreshClient();
					});
				// Mask the input — bearer keys are sensitive and the
				// settings tab is a regular textarea by default.
				text.inputEl.type = "password";
			});

		// An explicit probe button. The plugin already probes on every
		// endpoint/key edit and every 30s, but a button gives confirmation
		// on demand — including the "still wrong" case, where the passive
		// transition-gated notice stays silent.
		new Setting(containerEl)
			.setName("Test connection")
			.setDesc("Probe the endpoint now with the current key and report the result.")
			.addButton((btn) =>
				btn.setButtonText("Test connection").onClick(async () => {
					btn.setDisabled(true);
					btn.setButtonText("Testing…");
					try {
						await this.plugin.testConnection();
					} finally {
						btn.setButtonText("Test connection");
						btn.setDisabled(false);
					}
				})
			);

		new Setting(containerEl)
			.setName("Traces folder")
			.setDesc("Folder inside the vault that contains trace markdown files. Default: traces")
			.addText((text) =>
				text
					.setPlaceholder("traces")
					.setValue(this.plugin.settings.tracesFolder)
					.onChange(async (value) => {
						this.plugin.settings.tracesFolder = value.trim() || "traces";
						await this.plugin.saveSettings();
					})
			);

		new Setting(containerEl)
			.setName("Default author")
			.setDesc(
				"Stamped on traces created via the 'New trace' command. Leave empty to let the cortex's default apply. Free-form — typical values are a username, agent name, or 'agent/handle'."
			)
			.addText((text) =>
				text
					.setPlaceholder("e.g. alice or agent/researcher-1")
					.setValue(this.plugin.settings.defaultAuthor)
					.onChange(async (value) => {
						this.plugin.settings.defaultAuthor = value.trim();
						await this.plugin.saveSettings();
					})
			);

		new Setting(containerEl)
			.setName("Noema search mode")
			.setDesc("Used by the 'Search traces' command. Hybrid uses the cortex's server-side hybrid_weight.")
			.addDropdown((dropdown) =>
				dropdown
					.addOption("hybrid", "Hybrid")
					.addOption("semantic", "Semantic")
					.addOption("lexical", "Lexical")
					.setValue(this.plugin.settings.searchMode)
					.onChange(async (value) => {
						this.plugin.settings.searchMode =
							value as NoemaSettings["searchMode"];
						await this.plugin.saveSettings();
					})
			);
	}
}
