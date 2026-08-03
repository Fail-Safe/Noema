//go:build !windows

package plugin

import "os"

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
