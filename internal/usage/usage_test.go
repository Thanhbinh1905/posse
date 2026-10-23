package usage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadHarnessSessionFiles(t *testing.T) {
	tests := []struct {
		kind, filename, contents string
		want                     Counts
	}{
		{"claude", "claude.jsonl", `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":12,"output_tokens":3,"cache_read_input_tokens":4,"cache_creation_input_tokens":2}}}` + "\n" + `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":12,"output_tokens":5,"cache_read_input_tokens":4,"cache_creation_input_tokens":2}}}`, Counts{Input: 12, Output: 5, CacheRead: 4, CacheWrite: 2, Known: true}},
		{"codex", "codex.jsonl", `{"type":"turn.completed","usage":{"input_tokens":12,"cached_input_tokens":4,"output_tokens":3}}`, Counts{Input: 12, Output: 3, CacheRead: 4, Known: true}},
		{"pi", "pi.jsonl", `{"type":"message_end","message":{"role":"assistant","usage":{"input":12,"output":3,"cacheRead":4,"cacheWrite":2}}}`, Counts{Input: 12, Output: 3, CacheRead: 4, CacheWrite: 2, Known: true}},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tt.filename)
			if err := os.WriteFile(path, []byte(tt.contents+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := Read(tt.kind, path); got != tt.want {
				t.Fatalf("Read() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestReadUsesRecordedSessionID(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", root)
	sessions := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sessions, "2025-01-01T00-00-00-000Z_session-id.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"message_end","message":{"role":"assistant","usage":{"input":8,"output":2}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Read("pi", "session-id"); got != (Counts{Input: 8, Output: 2, Known: true}) {
		t.Fatalf("Read() = %#v", got)
	}
}

func TestUnknownWhenReaderOrSessionIsUnavailable(t *testing.T) {
	for _, tt := range []struct{ kind, session string }{{"other", "session"}, {"pi", "missing"}, {"", ""}} {
		if got := Read(tt.kind, tt.session); got.Known {
			t.Fatalf("Read(%q, %q) = %#v, want unknown", tt.kind, tt.session, got)
		}
	}
}
