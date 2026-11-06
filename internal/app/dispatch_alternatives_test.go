package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func forbidQuotaCollection(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "quota-called")
	script := "#!/bin/sh\ntouch '" + marker + "'\nexit 99\n"
	if err := os.WriteFile(filepath.Join(dir, "quota-axi"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return marker
}

func assertNoQuotaCollection(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("core invoked quota-axi: %v", err)
	}
}

func TestDispatchAndDirectStartDoNotCollectQuota(t *testing.T) {
	f := newStartFixture(t)
	marker := forbidQuotaCollection(t)
	text := "[defaults]\nmerge_mode='local'\n[profiles.pinned]\nkind='claude'\n[profiles.alternative]\nkind='pi'\n[[dispatch]]\ntype='change'\nuse=['alternative','pinned']\n"
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"dispatch", "--brief", f.brief, "--json"}, {"dispatch", "--brief", f.brief, "--profile", "pinned", "--json"}, {"start", "--brief", f.brief, "--profile", "pinned", "--json"}} {
		code := 0
		if len(args) == 4 {
			code = 1
		}
		output := outcomeCLI(t, f.service, code, args...)
		if strings.Contains(output, "quota_advice") {
			t.Fatalf("unexpected core evidence: %s", output)
		}
	}
	assertNoQuotaCollection(t, marker)
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	task, err := db.Task(context.Background(), f.project.ID, "t1")
	if err != nil || task.Profile != "pinned" {
		t.Fatalf("explicit choice lost: %+v %v", task, err)
	}
	if count, err := db.ActiveWorkerCount(context.Background(), f.project.ID); err != nil || count != 1 {
		t.Fatalf("Worker count=%d err=%v", count, err)
	}
	notices, err := db.Notices(context.Background(), f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "quota_advice" {
			t.Fatalf("unexpected quota Notice: %+v", notice)
		}
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
	if !strings.Contains(output, `"profile":"two"`) {
		t.Fatal(output)
	}
}
