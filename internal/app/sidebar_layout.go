package app

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/thanhbinh1905/posse/internal/atomicfile"
)

func herdrConfigPath() (string, error) {
	root := os.Getenv("XDG_CONFIG_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".config")
	}
	return filepath.Join(root, "herdr", "config.toml"), nil
}

const sidebarLayoutSnippet = `[ui.sidebar.agents]
rows = [
  ["state_icon", "machine", { token = "workspace", rules = [{ starts_with = "Lead:", hide = true }] }, { token = "$posse_row", bold = true, dim = false }],
  ["agent"],
]
`

const legacySidebarLayoutSnippet = `[ui.sidebar.agents]
rows = [
  ["state_icon", "machine", { token = "workspace", rules = [{ starts_with = "Lead:", hide = true }] }, { token = "$posse_row", bold = true }],
  ["agent"],
]
`

func removeSidebarLayout(path string) (string, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text := string(data)
	for _, snippet := range []string{sidebarLayoutSnippet, legacySidebarLayoutSnippet} {
		if strings.Contains(text, snippet) {
			updated := strings.Replace(text, snippet, "", 1)
			if err := atomicfile.Write(path, []byte(updated), info.Mode().Perm()); err != nil {
				return "", err
			}
			return "removed", nil
		}
	}
	var document map[string]any
	if _, err := toml.Decode(text, &document); err != nil {
		return "", err
	}
	ui, _ := document["ui"].(map[string]any)
	sidebar, _ := ui["sidebar"].(map[string]any)
	if _, exists := sidebar["agents"]; exists {
		return "custom", nil
	}
	return "", nil
}
