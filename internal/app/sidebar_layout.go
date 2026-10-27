package app

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/thanhbinh1905/posse/internal/atomicfile"
)

const sidebarLayoutSnippet = `[ui.sidebar.agents]
rows = [
  ["state_icon", "machine", { token = "workspace", rules = [{ starts_with = "Lead:", hide = true }] }, { token = "$posse_row", bold = true, dim = false }],
  ["agent"],
]
`

// legacySidebarLayoutSnippet is the layout Posse offered before dim = false; Herdr
// dims custom tokens by default, so Posse upgrades it in place.
const legacySidebarLayoutSnippet = `[ui.sidebar.agents]
rows = [
  ["state_icon", "machine", { token = "workspace", rules = [{ starts_with = "Lead:", hide = true }] }, { token = "$posse_row", bold = true }],
  ["agent"],
]
`

var agentsTableHeader = regexp.MustCompile(`(?m)^\s*\[ui\.sidebar\.agents\]\s*(?:#.*)?$`)
var agentsChildHeader = regexp.MustCompile(`(?m)^\s*\[ui\.sidebar\.agents\.[^\]]+\]\s*(?:#.*)?$`)

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

// sidebarLayoutState reads the actual Herdr config, not Posse's setup manifest.
// A custom rows value is always left alone, even when it resembles our layout.
func sidebarLayoutState(path string) (string, error) {
	info, statErr := os.Lstat(path)
	isSymlink := statErr == nil && info.Mode()&os.ModeSymlink != 0
	if statErr != nil && !os.IsNotExist(statErr) {
		return "", statErr
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if isSymlink {
			return "symlink", nil
		}
		return "offer", nil
	}
	if err != nil {
		return "", err
	}
	var document map[string]any
	if _, err := toml.Decode(string(data), &document); err != nil {
		return "invalid", err
	}
	if rows, exists := sidebarRows(document); exists {
		if reflect.DeepEqual(rows, snippetRows(sidebarLayoutSnippet)) {
			return "keep", nil
		}
		if reflect.DeepEqual(rows, snippetRows(legacySidebarLayoutSnippet)) {
			if isSymlink {
				return "symlink", nil
			}
			return "upgrade", nil
		}
		return "manual", nil
	}
	if isSymlink {
		return "symlink", nil
	}
	return "offer", nil
}

func snippetRows(snippet string) any {
	var document map[string]any
	_, _ = toml.Decode(snippet, &document)
	rows, _ := sidebarRows(document)
	return rows
}

func sidebarRows(document map[string]any) (any, bool) {
	for _, key := range []string{"ui", "sidebar", "agents"} {
		nested, ok := document[key].(map[string]any)
		if !ok {
			return nil, false
		}
		document = nested
	}
	rows, exists := document["rows"]
	return rows, exists
}

// installSidebarLayout adds the single setting without reserializing or losing
// any comments. Reinspect immediately before writing to respect newer edits.
func installSidebarLayout(path string) (bool, error) {
	state, err := sidebarLayoutState(path)
	if err != nil || state != "offer" && state != "upgrade" {
		return false, err
	}
	original, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	text := string(original)
	if state == "upgrade" {
		rows := strings.TrimPrefix(legacySidebarLayoutSnippet, "[ui.sidebar.agents]\n")
		if !strings.Contains(text, rows) {
			return false, fmt.Errorf("cannot upgrade the Posse sidebar layout: rows are formatted differently from the layout Posse wrote")
		}
		return writeSidebarLayout(path, strings.Replace(text, rows, strings.TrimPrefix(sidebarLayoutSnippet, "[ui.sidebar.agents]\n"), 1))
	}
	insert := sidebarLayoutSnippet
	position := len(text)
	if match := agentsTableHeader.FindStringIndex(text); match != nil {
		position = match[1]
		if position < len(text) && text[position] == '\r' {
			position++
		}
		if position < len(text) && text[position] == '\n' {
			position++
		}
		insert = strings.TrimPrefix(sidebarLayoutSnippet, "[ui.sidebar.agents]\n")
	} else if child := agentsChildHeader.FindStringIndex(text); child != nil {
		position = child[0]
	}
	if position == len(text) && len(text) > 0 && !strings.HasSuffix(text, "\n") {
		insert = "\n" + insert
	}
	return writeSidebarLayout(path, text[:position]+insert+text[position:])
}

func writeSidebarLayout(path, updated string) (bool, error) {
	var check map[string]any
	if _, err := toml.Decode(updated, &check); err != nil {
		return false, fmt.Errorf("cannot add sidebar layout without changing existing Herdr config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	if err := atomicfile.Write(path, []byte(updated), fileMode(path, 0o600)); err != nil {
		return false, err
	}
	return true, nil
}
