import { App, Modal, Notice } from "obsidian";
import type NoemaPlugin from "./main";

export class EditTraceTitleModal extends Modal {
	private titleInput!: HTMLInputElement;
	private submitBtn!: HTMLButtonElement;
	private errorEl!: HTMLElement;
	private submitting = false;

	constructor(
		app: App,
		private plugin: NoemaPlugin,
		private traceId: string,
		private currentTitle: string
	) {
		super(app);
	}

	onOpen(): void {
		const { contentEl } = this;
		contentEl.empty();
		contentEl.addClass("noema-edit-title-modal");
		this.titleEl.setText("Edit trace title");

		contentEl.createEl("div", {
			cls: "noema-edit-title-identity",
			text: this.traceId,
		});
		contentEl.createEl("div", {
			cls: "noema-edit-title-hint",
			text: "The trace ID and filename stay fixed so lineage and federation references remain valid.",
		});

		this.titleInput = contentEl.createEl("input", {
			cls: "noema-edit-title-input",
			type: "text",
			value: this.currentTitle,
		});
		this.titleInput.addEventListener("keydown", (event) => {
			if (event.key === "Enter") {
				event.preventDefault();
				this.submit();
			}
		});

		this.errorEl = contentEl.createEl("div", { cls: "noema-edit-title-error" });
		this.errorEl.style.display = "none";

		const buttons = contentEl.createEl("div", { cls: "noema-edit-title-buttons" });
		const cancelBtn = buttons.createEl("button", { text: "Cancel" });
		cancelBtn.addEventListener("click", () => this.close());
		this.submitBtn = buttons.createEl("button", { text: "Save", cls: "mod-cta" });
		this.submitBtn.addEventListener("click", () => this.submit());

		setTimeout(() => {
			this.titleInput.focus();
			this.titleInput.select();
		}, 0);
	}

	onClose(): void {
		this.contentEl.empty();
	}

	private async submit(): Promise<void> {
		if (this.submitting) return;
		const title = this.titleInput.value.trim();
		if (!title) {
			this.showError("Title is required.");
			this.titleInput.focus();
			return;
		}
		const client = this.plugin.client;
		if (!client) {
			this.showError("No Noema endpoint configured. Set one in Settings → Noema.");
			return;
		}

		this.submitting = true;
		this.submitBtn.disabled = true;
		this.submitBtn.setText("Saving…");
		try {
			await client.updateTraceTitle(this.traceId, title);
			this.close();
			new Notice("Trace title updated. Its stable ID filename is unchanged.");
		} catch (error) {
			const message = error instanceof Error ? error.message : String(error);
			this.showError(`Couldn't update trace title: ${message}`);
			this.submitBtn.disabled = false;
			this.submitBtn.setText("Save");
		} finally {
			this.submitting = false;
		}
	}

	private showError(text: string): void {
		this.errorEl.setText(text);
		this.errorEl.style.display = "";
	}
}
