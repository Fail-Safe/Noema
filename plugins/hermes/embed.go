package hermesplugin

import "embed"

// Files contains only the Hermes files required at runtime.
//
//go:embed __init__.py plugin.yaml transport.py
var Files embed.FS

var ManagedFiles = []string{"__init__.py", "plugin.yaml", "transport.py"}
