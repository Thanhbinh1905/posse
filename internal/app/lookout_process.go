package app

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Check the process rather than trusting a restored shell pane's label.
func lookoutProcessRunning(paneID, home string) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		root := filepath.Join("/proc", entry.Name())
		environment, err := os.ReadFile(filepath.Join(root, "environ"))
		if err != nil || !strings.Contains(string(environment), "HERDR_PANE_ID="+paneID+"\x00") || !strings.Contains(string(environment), "POSSE_HOME="+home+"\x00") {
			continue
		}
		command, err := os.ReadFile(filepath.Join(root, "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(command), "\x00")
		for i := 1; i+1 < len(args); i++ {
			if args[i] == "lookout" && args[i+1] == "--poll-only" {
				return true
			}
		}
	}
	return false
}
