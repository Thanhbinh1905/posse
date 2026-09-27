package app

import (
	"os"
	"path/filepath"
	"strings"
)

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
