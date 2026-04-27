import type { TFile, App } from "obsidian";

// Tier names the cortex emits in frontmatter. Anything else is
// rendered as "unknown" — defensive against schema drift.
export type Tier = "short" | "mid" | "long" | "unknown";

export interface TraceMetadata {
	id?: string;
	title?: string;
	type?: string;
	tier: Tier;
}

// readTraceMetadata pulls the cortex-relevant fields out of a file's
// cached frontmatter. Uses Obsidian's metadataCache so we don't have
// to re-parse YAML on every status update; the cache already has it
// indexed and updates as the user edits. Returns null if the file
// has no frontmatter at all (probably not a trace).
export function readTraceMetadata(app: App, file: TFile): TraceMetadata | null {
	const cache = app.metadataCache.getFileCache(file);
	const fm = cache?.frontmatter;
	if (!fm) return null;
	return {
		id: typeof fm.id === "string" ? fm.id : undefined,
		title: typeof fm.title === "string" ? fm.title : undefined,
		type: typeof fm.type === "string" ? fm.type : undefined,
		tier: normalizeTier(fm.tier),
	};
}

// normalizeTier maps the YAML value to one of the known tiers, with
// "unknown" as a safe fallback so the UI never has to think about
// nulls or unexpected strings. Defaults to short — the cortex emits
// short on creation and only promotes upward, so a trace with a
// missing tier field is most likely an old short-tier trace.
export function normalizeTier(raw: unknown): Tier {
	if (typeof raw !== "string") return "short";
	const lc = raw.toLowerCase();
	if (lc === "short" || lc === "mid" || lc === "long") return lc;
	return "unknown";
}

// tierGlyph returns the single-letter badge used in the status bar.
// Matches the TUI's badge convention so users moving between
// surfaces see the same shorthand: lowercase s/m for short/mid,
// capital L for long (the visually loudest one because long-tier
// traces are immutable).
export function tierGlyph(tier: Tier): string {
	switch (tier) {
		case "short":
			return "s";
		case "mid":
			return "m";
		case "long":
			return "L";
		default:
			return "?";
	}
}

// tierLabel is the human-readable name used in tooltips and warning
// text. Centralized so we don't drift between the status bar and any
// future surfaces (file-explorer decoration, sidebar header, etc.)
// that might need to render the same word.
export function tierLabel(tier: Tier): string {
	switch (tier) {
		case "short":
			return "short-term";
		case "mid":
			return "mid-term";
		case "long":
			return "long-term (immutable)";
		default:
			return "unknown tier";
	}
}
