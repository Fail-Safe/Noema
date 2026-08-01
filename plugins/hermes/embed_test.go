package hermesplugin

import (
	"io/fs"
	"reflect"
	"testing"
)

func TestEmbeddedRuntimeManifest(t *testing.T) {
	want := []string{"__init__.py", "plugin.yaml", "transport.py"}
	if !reflect.DeepEqual(ManagedFiles, want) {
		t.Fatalf("managed files = %v, want %v", ManagedFiles, want)
	}
	for _, name := range ManagedFiles {
		info, err := fs.Stat(Files, name)
		if err != nil {
			t.Fatalf("embedded %s: %v", name, err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("embedded %s is not regular", name)
		}
	}
}
