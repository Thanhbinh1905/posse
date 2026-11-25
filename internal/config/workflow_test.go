package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStagedWorkflowConfigNamesRemoval(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	writeConfig(t, path, `[workflows.feature]
stages = [{ name = "Build", frame = "implement", gate = "user", task_type = "change", workers = 1 }]
`)
	_, err := Load(home, "")
	if err == nil || !strings.Contains(err.Error(), "staged workflows were removed") {
		t.Fatalf("Load() error = %v, want removal note", err)
	}
}
