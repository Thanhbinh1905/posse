package app

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/thanhbinh1905/posse/internal/atomicfile"
)

const sidebarCustomNote = "custom [ui.sidebar.agents] kept; Posse no longer uses it"

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

func removeSidebarLayout(path string, write bool) (string, error) {
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
		if index := wholeSnippetIndex(text, snippet); index >= 0 {
			if !write {
				return "remove", nil
			}
			updated := text[:index] + text[index+len(snippet):]
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

func wholeSnippetIndex(text, snippet string) int {
	for offset := 0; ; {
		found := strings.Index(text[offset:], snippet)
		if found < 0 {
			return -1
		}
		index := offset + found
		offset = index + 1
		if index > 0 && text[index-1] != '\n' {
			continue
		}
		rest := text[index+len(snippet):]
		whole := true
		for _, line := range strings.Split(rest, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "[") {
				table := strings.ReplaceAll(trimmed, " ", "")
				whole = table != "[ui.sidebar.agents]" && !strings.HasPrefix(table, "[ui.sidebar.agents.")
				break
			}
			if trimmed != "" {
				whole = false
				break
			}
		}
		if whole {
			return index
		}
	}
}
