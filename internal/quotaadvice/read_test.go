package quotaadvice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadUsesOnlySafeProvider(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n[ \"$*\" = '--json --full --no-credential-refresh --provider claude' ] || exit 9\nprintf '{\"schemaVersion\":5,\"generatedAt\":\"%s\",\"providers\":[]}' \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"\n"
	if err := os.WriteFile(filepath.Join(dir, "quota-axi"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	got := Read(context.Background())
	if got.GeneratedAt == "" {
		t.Fatalf("safe invocation failed: %+v", got)
	}
}

func TestParsePreservesSanitizedRows(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	text := `{"schemaVersion":5,"generatedAt":"` + now.Format(time.RFC3339) + `","providers":[{"provider":"claude","account":{"email":"private-canary","accountId":"private-canary","identityStatus":"verified"},"state":{"status":"fresh","refreshedAt":"` + now.Format(time.RFC3339) + `"},"windows":[{"id":"seven_day","label":"private-canary","kind":"weekly","percentRemaining":31,"resetsAt":"` + now.Add(time.Hour).Format(time.RFC3339) + `"}]},{"provider":"claude","state":{"status":"auth_required","error":"private-canary"},"windows":[]}]}`
	got := Parse([]byte(text), now)
	encoded, _ := json.Marshal(got)
	if !strings.Contains(string(encoded), `"scope":"all_models"`) {
		t.Fatalf("missing account window scope: %s", encoded)
	}
	if len(got.Observations) != 2 || got.Observations[0].Status != "fresh" || len(got.Observations[0].Windows) != 1 || got.Observations[1].Status != "auth_required" {
		t.Fatalf("lost compatible rows: %+v", got)
	}
	data, _ := json.Marshal(got)
	if strings.Contains(string(data), "private-canary") || got.Status != "unknown" {
		t.Fatalf("identity leak or false mapping: %s", data)
	}
}

func TestParseUnknowns(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct{ name, text, reason string }{
		{"malformed", "{", "invalid_output"},
		{"unsupported", `{"schemaVersion":6}`, "unsupported_schema"},
		{"old", `{"schemaVersion":5,"generatedAt":"2001-01-01T00:00:00Z"}`, "stale_report"},
		{"bad clock", `{"schemaVersion":5,"generatedAt":"bad"}`, "stale_report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse([]byte(tc.text), now); got.Status != "unknown" || got.Reason != tc.reason {
				t.Fatalf("%+v", got)
			}
		})
	}
}
