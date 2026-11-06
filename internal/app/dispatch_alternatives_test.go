package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/quotaadvice"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestQuotaAdviceDirectCLI(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	for _, tc := range []struct{ name, script string }{
		{"missing", "exit 127"},
		{"nonzero", "printf 'private-canary' >&2; exit 1"},
		{"malformed", "printf '{'"},
		{"unsupported", `printf '{"schemaVersion":99}'`},
		{"stale", `printf '{"schemaVersion":5,"generatedAt":"2001-01-01T00:00:00Z"}'`},
		{"partial", `printf '{"schemaVersion":5,"generatedAt":"` + now + `","providers":[{"provider":"claude","state":{"status":"auth_required","error":"private-canary"}}]}'`},
		{"mismatched account and model", `printf '{"schemaVersion":5,"generatedAt":"` + now + `","providers":[{"provider":"claude","account":{"identityStatus":"verified","email":"private-canary"},"state":{"status":"fresh","refreshedAt":"` + now + `"},"windows":[{"kind":"model","label":"wrong-model","percentRemaining":99}]}]}'`},
		{"timeout", "sleep 10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStartFixture(t)
			dir := t.TempDir()
			script := "#!/bin/sh\n[ \"$*\" = '--json --full --no-credential-refresh --provider claude' ] || exit 8\n" + tc.script + "\n"
			if err := os.WriteFile(filepath.Join(dir, "quota-axi"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			original := readQuotaEvidence
			readQuotaEvidence = quotaadvice.Read
			t.Cleanup(func() { readQuotaEvidence = original })
			configText := "[defaults]\nmerge_mode='local'\n[profiles.pinned]\nkind='claude'\n[profiles.alternative]\nkind='pi'\n[[dispatch]]\ntype='change'\nuse=['alternative','pinned']\n"
			if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(configText), 0600); err != nil {
				t.Fatal(err)
			}
			output := outcomeCLI(t, f.service, 0, "start", "--brief", f.brief, "--profile", "pinned", "--json")
			db, err := store.Open(f.home)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			task, err := db.Task(context.Background(), f.project.ID, "t1")
			if err != nil || task.Profile != "pinned" {
				t.Fatalf("explicit choice lost: %+v %v %s", task, err, output)
			}
			if count, err := db.ActiveWorkerCount(context.Background(), f.project.ID); err != nil || count != 1 {
				t.Fatalf("Worker count=%d err=%v", count, err)
			}
			notices, err := db.Notices(context.Background(), f.project.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, notice := range notices {
				if notice.Kind == "quota_exhausted" || notice.Kind == "quota_fallback" {
					t.Fatalf("advice triggered exhaustion: %+v", notice)
				}
				if notice.Kind == "quota_advice" {
					found = true
					var advice profileQuotaAdvice
					if err := json.Unmarshal([]byte(notice.DataJSON), &advice); err != nil {
						t.Fatal(err)
					}
					if advice.Phase != "launch" || advice.Evidence.Status != "unknown" || !strings.Contains(advice.Evidence.Codex, "excluded") {
						t.Fatalf("bad advice: %+v", advice)
					}
					if strings.Contains(notice.DataJSON, "private-canary") {
						t.Fatal("private output leaked")
					}
				}
			}
			if !found {
				t.Fatal("missing launch advice")
			}
		})
	}
}

func TestDispatchRejectsInvalidAlternativesCLI(t *testing.T) {
	for _, use := range []string{"[]", "['']", "[1]", "['one','unknown']"} {
		t.Run(use, func(t *testing.T) {
			f := newFirstOutcomeFixture(t)
			text := "[profiles.one]\nkind='pi'\n[[dispatch]]\ntype='change'\nuse=" + use + "\n"
			if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			output := outcomeCLI(t, f.service, 1, "dispatch", "--brief", f.brief, "--json")
			if !strings.Contains(output, "config_invalid") || !strings.Contains(output, "use") {
				t.Fatal(output)
			}
		})
	}
}

func TestDispatchAlternativesCLI(t *testing.T) {
	f := newFirstOutcomeFixture(t)
	text := "[profiles.one]\nkind='pi'\n[profiles.two]\nkind='pi'\n[[dispatch]]\ntype='change'\nuse=['one','two']\n"
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	output := outcomeCLI(t, f.service, 1, "dispatch", "--brief", f.brief, "--json")
	for _, want := range []string{"profile_required", "one", "two"} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %s: %s", want, output)
		}
	}
	output = outcomeCLI(t, f.service, 0, "dispatch", "--brief", f.brief, "--profile", "two", "--json")
	if !strings.Contains(output, `"quota_advice"`) {
		t.Fatalf("quota selection evidence missing: %s", output)
	}
	if !strings.Contains(output, `"profile":"two"`) {
		t.Fatal(output)
	}
}
