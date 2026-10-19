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
	if insidePoolSandbox(home, resolvedRoot) {
		return "Project root is inside a Pool Sandbox: " + root
	}
	if pathWithin(doctorResolvedPath(os.TempDir()), resolvedRoot) {
		return "Project root is under a temporary directory: " + root
	}
	return ""
}

func insidePoolSandbox(home, root string) bool {
	for _, directory := range []string{"pool", "remuda"} {
		poolRoot := doctorResolvedPath(filepath.Join(home, directory))
		relative, err := filepath.Rel(poolRoot, root)
		if err != nil {
			continue
		}
		parts := strings.Split(relative, string(filepath.Separator))
		if len(parts) >= 2 && parts[0] != ".." {
			if _, ok := sandboxDirectoryNumber(parts[1]); ok {
				return true
			}
		}
	}
	return false
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
