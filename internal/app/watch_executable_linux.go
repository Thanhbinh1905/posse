//go:build linux

package app

import (
	"os"
	"path/filepath"
	"strconv"
)

func processExecutablePath(pid int) (string, bool) {
	path := filepath.Join(watchProcRoot, strconv.Itoa(pid), "exe")
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	return path, true
}
