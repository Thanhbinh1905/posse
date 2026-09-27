package app

import (
	_ "embed"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

//go:embed claude_lowkey/verified-versions.txt
var claudeLowkeyVerifiedVersions string

const claudeLowkeyVerificationCommand = "POSSE_CLAUDE_LOWKEY_LIVE_E2E=1 scripts/claude-lowkey-live-e2e.sh"

func claudeLowkeyVerified(version string) bool {
	current, ok := parseClaudeCodeVersion(version)
	if !ok {
		return false
	}
	for _, verified := range strings.Fields(claudeLowkeyVerifiedVersions) {
		known, ok := parseClaudeCodeVersion(verified + " (Claude Code)")
		if ok && current == known {
			return true
		}
	}
	return false
}

func claudeLowkeyVersionStatus(version string) string {
	if claudeLowkeyVerified(version) {
		return "ok"
	}
	current, ok := parseClaudeCodeVersion(version)
	if !ok {
		return "warn"
	}
	latest := [3]int{}
	for _, verified := range strings.Fields(claudeLowkeyVerifiedVersions) {
		known, ok := parseClaudeCodeVersion(verified + " (Claude Code)")
		if ok && compareClaudeVersions(known, latest) > 0 {
			latest = known
		}
	}
	if compareClaudeVersions(current, latest) > 0 {
		return "info"
	}
	return "warn"
}

func parseClaudeCodeVersion(version string) ([3]int, bool) {
	var parsed [3]int
	version, ok := strings.CutSuffix(version, " (Claude Code)")
	if !ok {
		return parsed, false
	}
	parts := strings.Split(version, ".")
	if len(parts) != len(parsed) {
		return parsed, false
	}
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return [3]int{}, false
		}
		parsed[i] = value
	}
	return parsed, true
}

func compareClaudeVersions(left, right [3]int) int {
	for i := range left {
		if left[i] < right[i] {
			return -1
		}
		if left[i] > right[i] {
			return 1
		}
	}
	return 0
}

func staleProjectRootReason(root, home string) string {
	resolvedRoot := doctorResolvedPath(root)
	info, err := os.Stat(resolvedRoot)
	if os.IsNotExist(err) {
		return "Project root is missing: " + root
	}
	if err == nil && !info.IsDir() {
		return "Project root is not a directory: " + root
	}
	if insideRemudaMount(home, resolvedRoot) {
		return "Project root is inside a Remuda Mount: " + root
	}
	if pathWithin(doctorResolvedPath(os.TempDir()), resolvedRoot) {
		return "Project root is under a temporary directory: " + root
	}
	return ""
}

func insideRemudaMount(home, root string) bool {
	remudaRoot := doctorResolvedPath(filepath.Join(home, "remuda"))
	relative, err := filepath.Rel(remudaRoot, root)
	if err != nil {
		return false
	}
	parts := strings.Split(relative, string(filepath.Separator))
	return len(parts) >= 2 && parts[0] != ".." && strings.HasPrefix(parts[1], "mount-")
}

func doctorResolvedPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(absolute)
}
