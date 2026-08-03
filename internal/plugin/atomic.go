package plugin

import (
	"fmt"
	"os"
	"path/filepath"
)

type atomicFileOps struct {
	replace func(string, string) error
}

func writeAtomicFile(path string, data []byte, ops atomicFileOps) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".noema-plugin-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if ops.replace == nil {
		return fmt.Errorf("atomic replacement is not configured")
	}
	return ops.replace(tmp.Name(), path)
}
