package obsidianplugin

import "embed"

// Files contains only the built Obsidian files required at runtime.
//
//go:embed main.js manifest.json styles.css
var Files embed.FS

var ManagedFiles = []string{"main.js", "manifest.json", "styles.css"}
