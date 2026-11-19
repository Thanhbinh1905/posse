package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func requireGreenhouseRepositorySafe(repository string) error {
	config := exec.Command("git", "config", "--get-regexp", `^(filter\..*|core\.(hookspath|fsmonitor|attributesfile)|commit\.gpgsign)$`)
	config.Dir = repository
	output, err := config.CombinedOutput()
	if err == nil && len(strings.TrimSpace(string(output))) > 0 {
		return errors.New("configured hooks, filters, signing, attributes or fsmonitor requirements unsupported")
	}
	var exitErr *exec.ExitError
	if err != nil && (!errors.As(err, &exitErr) || exitErr.ExitCode() != 1) {
		return fmt.Errorf("inspect Git configuration: %w", err)
	}

	hooksCommand := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	hooksCommand.Dir = repository
	hooks, err := hooksCommand.Output()
	if err != nil {
		return fmt.Errorf("locate Git hooks: %w", err)
	}
	entries, err := os.ReadDir(strings.TrimSpace(string(hooks)))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read Git hooks: %w", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sample") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect Git hook %s: %w", entry.Name(), err)
		}
		if info.Mode().Perm()&0111 != 0 {
			return errors.New("executable Git hooks unsupported; requirements are not bypassed")
		}
	}
	return nil
}
