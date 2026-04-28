import { App, PluginSettingTab, Setting } from "obsidian";
import type NoemaPlugin from "./main";

export interface NoemaSettings {
	endpoint: string;
	bearerKey: string;
	tracesFolder: string;
}

// DEFAULT_SETTINGS deliberately leave endpoint and bearerKey empty so
// the plugin starts in "disconnected" state on a fresh install rather
// than blindly trying to reach a localhost URL. tracesFolder defaults
// to "traces" because that's the cortex layout convention; users with
// a non-standard layout (rare) can override.
export const DEFAULT_SETTINGS: NoemaSettings = {
	endpoint: "",
	bearerKey: "",
	tracesFolder: "traces",
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
	}
}
