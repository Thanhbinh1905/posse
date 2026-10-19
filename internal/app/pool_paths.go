package app

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Keep existing Pool directories in place: moving an active git worktree would
// invalidate Git registrations and running process working directories.
func poolProjectPath(home, project string) string {
	legacy := filepath.Join(home, "remuda", project)
	if _, err := os.Lstat(legacy); err == nil {
		return legacy
	}
	return filepath.Join(home, "pool", project)
}

func sandboxDirectoryNumber(name string) (int, bool) {
	for _, prefix := range []string{"sandbox-", "mount-"} {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		number, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
		return number, err == nil && number > 0
	}
	return 0, false
}
