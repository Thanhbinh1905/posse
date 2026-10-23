package usage

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadRealHarnessUsageRecords(t *testing.T) {
	tests := []struct {
		kind, contents string
		want           Counts
	}{
		{"claude", `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":12,"output_tokens":5,"cache_read_input_tokens":4,"cache_creation_input_tokens":2}}}`, Counts{Input: 12, Output: 5, CacheRead: 4, CacheWrite: 2, Known: true}},
		{"codex", `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":20,"cached_input_tokens":5,"cache_write_input_tokens":2,"output_tokens":8,"reasoning_output_tokens":1,"total_tokens":28}}}}` + "\n" + `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":30,"cached_input_tokens":7,"cache_write_input_tokens":3,"output_tokens":12,"reasoning_output_tokens":2,"total_tokens":42}}}}`, Counts{Input: 30, Output: 12, CacheRead: 7, CacheWrite: 3, Known: true}},
		{"pi", `{"type":"message","message":{"role":"assistant","usage":{"input":12,"output":3,"cacheRead":4,"cacheWrite":2,"totalTokens":21,"cost":{"total":0.01}}}}`, Counts{Input: 12, Output: 3, CacheRead: 4, CacheWrite: 2, Known: true}},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			if got := Read(tt.kind, writeFixture(t, tt.contents), ""); got != tt.want {
				t.Fatalf("Read() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestSessionWithoutUsageIsUnknown(t *testing.T) {
	tests := []struct{ kind, contents string }{
		{"claude", `{"type":"assistant","message":{"id":"m1","content":[]}}`},
		{"codex", `{"type":"event_msg","payload":{"type":"turn_started"}}`},
		{"pi", `{"type":"message","message":{"role":"assistant","content":[]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			if got := Read(tt.kind, writeFixture(t, tt.contents), ""); got.Known {
				t.Fatalf("Read() = %#v, want unknown", got)
			}
		})
	}
}

func TestReadUsesRecordedSessionAndCWD(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	cwd := filepath.Join(string(filepath.Separator), "home", "worker", "sandbox")
	project := filepath.Join(root, "projects", "-home-worker-sandbox")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "session-id.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":8,"output_tokens":2}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Read("claude", "session-id", cwd); got != (Counts{Input: 8, Output: 2, Known: true}) {
		t.Fatalf("Read() = %#v", got)
	}
}

func TestUnknownWhenReaderOrSessionIsUnavailable(t *testing.T) {
	for _, tt := range []struct{ kind, session string }{{"other", "session"}, {"pi", "missing"}, {"", ""}} {
		if got := Read(tt.kind, tt.session, ""); got.Known {
			t.Fatalf("Read(%q, %q) = %#v, want unknown", tt.kind, tt.session, got)
		}
	}
}
