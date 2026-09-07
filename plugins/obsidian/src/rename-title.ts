export function titleFromFilenameRename(traceId: string, basename: string): string {
	const prefixedId = `${traceId}-`;
	if (basename.startsWith(prefixedId)) {
		const suffix = basename.slice(prefixedId.length).trim();
		if (suffix) return suffix;
	}
	return basename.trim();
}
