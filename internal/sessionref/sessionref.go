package sessionref

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var plainID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)

// Sanitize extracts a resumable session reference from Herdr's session value.
// It accepts plain IDs and paths contained in the named harness's session store.
func Sanitize(agent, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || raw == "{}" {
		return ""
	}
	var scalar string
	if json.Unmarshal([]byte(raw), &scalar) == nil {
		return validReference(agent, scalar)
	}
	var value struct {
		Value       string `json:"value"`
		SessionID   string `json:"session_id"`
		SessionPath string `json:"session_path"`
	}
	if json.Unmarshal([]byte(raw), &value) == nil {
		for _, candidate := range []string{value.Value, value.SessionID, value.SessionPath} {
			if reference := validReference(agent, candidate); reference != "" {
				return reference
			}
		}
		return ""
	}
	if json.Valid([]byte(raw)) {
		return ""
	}
	return validReference(agent, raw)
}

func validReference(agent, candidate string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" || strings.ContainsAny(candidate, "\x00\r\n") {
		return ""
	}
	if plainID.MatchString(candidate) && candidate != "." && candidate != ".." {
		return candidate
	}
	if !filepath.IsAbs(candidate) {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, root := range sessionDirectories(agent, home) {
		relative, err := filepath.Rel(root, filepath.Clean(candidate))
		if err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return filepath.Clean(candidate)
		}
	}
	return ""
}

func sessionDirectories(agent, home string) []string {
	root := func(variable, fallback string) string {
		value := os.Getenv(variable)
		if value == "" {
			return filepath.Join(home, fallback)
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(home, value)
		}
		return filepath.Clean(value)
	}
	claude := filepath.Join(root("CLAUDE_CONFIG_DIR", ".claude"), "projects")
	codex := filepath.Join(root("CODEX_HOME", ".codex"), "sessions")
	pi := filepath.Join(root("PI_CODING_AGENT_DIR", filepath.Join(".pi", "agent")), "sessions")
	switch strings.ToLower(agent) {
	case "claude", "claude-code":
		return []string{claude}
	case "codex", "codex-cli":
		return []string{codex}
	case "pi":
		return []string{pi}
	default:
		return []string{claude, codex, pi}
	}
}
