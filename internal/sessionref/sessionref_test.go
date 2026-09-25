package sessionref

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeAcceptsPlainIDsAndHarnessSessionPaths(t *testing.T) {
	useDefaultSessionRoots(t)
	for _, agent := range []string{"claude", "codex", "pi"} {
		t.Run(agent, func(t *testing.T) {
			if got := Sanitize(agent, `{"session_id":"thread-123"}`); got != "thread-123" {
				t.Fatalf("plain session ID = %q", got)
			}
			home, err := os.UserHomeDir()
			if err != nil {
				t.Fatal(err)
			}
			var directory string
			switch agent {
			case "claude":
				directory = filepath.Join(".claude", "projects")
			case "codex":
				directory = filepath.Join(".codex", "sessions")
			case "pi":
				directory = filepath.Join(".pi", "agent", "sessions")
			}
			path := filepath.Join(home, directory, "session.jsonl")
			value, err := json.Marshal(map[string]string{"session_path": path})
			if err != nil {
				t.Fatal(err)
			}
			if got := Sanitize(agent, string(value)); got != path {
				t.Fatalf("session path = %q, want %q", got, path)
			}
		})
	}
}

func TestSanitizeRejectsMalformedAndOutOfDirectoryReferences(t *testing.T) {
	useDefaultSessionRoots(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(home, ".pi", "agent", "config.json")
	for _, input := range []string{
		`{"session_id":"../../tmp/session"}`,
		`{"session_id":"--danger"}`,
		`{"session_id":"{\"session_id\":\"nested\"}"}`,
		`{"session_path":"` + outside + `"}`,
		`{"session_path":"/tmp/session.jsonl"}`,
		`{"value":""}`,
		`false`,
		`123`,
	} {
		if got := Sanitize("pi", input); got != "" {
			t.Errorf("Sanitize(%q) = %q, want empty", input, got)
		}
	}
}

func TestSanitizeRejectsPathsFromAnotherHarness(t *testing.T) {
	useDefaultSessionRoots(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".pi", "agent", "sessions", "session.jsonl")
	value, err := json.Marshal(map[string]string{"session_path": path})
	if err != nil {
		t.Fatal(err)
	}
	if got := Sanitize("claude", string(value)); got != "" {
		t.Fatalf("Claude session path from pi directory = %q", got)
	}
}

func TestSanitizeUsesConfiguredHarnessSessionDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pi-data")
	t.Setenv("PI_CODING_AGENT_DIR", root)
	path := filepath.Join(root, "sessions", "session.jsonl")
	value, err := json.Marshal(map[string]string{"session_path": path})
	if err != nil {
		t.Fatal(err)
	}
	if got := Sanitize("pi", string(value)); got != path {
		t.Fatalf("configured pi session path = %q, want %q", got, path)
	}
}

func useDefaultSessionRoots(t *testing.T) {
	t.Helper()
	for _, variable := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "PI_CODING_AGENT_DIR"} {
		t.Setenv(variable, "")
	}
}
