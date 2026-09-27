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
	if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "manual", nil
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		return "", statErr
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
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
		var expected map[string]any
		_, _ = toml.Decode(sidebarLayoutSnippet, &expected)
		want, _ := sidebarRows(expected)
		if reflect.DeepEqual(rows, want) {
			return "keep", nil
		}
		return "manual", nil
	}
	return "offer", nil
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
	if err != nil || state != "offer" {
		return false, err
	}
	original, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	text := string(original)
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
	updated := text[:position] + insert + text[position:]
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
