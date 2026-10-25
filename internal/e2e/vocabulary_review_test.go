//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// Running 0.14 adapters retain these argv arrays after the binary is replaced.
func Test014LeadAdapterVocabularyHandoffAndReceipt(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	stopBackgroundWatch(t, f)
	for _, adapter := range []string{"pi", "opencode"} {
		t.Run(adapter, func(t *testing.T) {
			id, err := f.db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, Kind: "worker_error", Summary: "Retained adapter upgrade notice"})
			if err != nil {
				t.Fatal(err)
			}
			output := runPosse(t, f.binary, f.repo, f.leadEnv, "lookout", "--json", "--quiet-routine", "--handoff", "--destination", adapter+":retained-session")
			var batch struct {
				Notices []struct {
					ID int64 `json:"id"`
				} `json:"notices"`
				Delivery struct {
					ID    string `json:"delivery_id"`
					Token string `json:"owner_token"`
				} `json:"delivery"`
			}
			if err := json.Unmarshal([]byte(output), &batch); err != nil {
				t.Fatalf("decode retained adapter result: %s: %v", output, err)
			}
			found := false
			for _, notice := range batch.Notices {
				if notice.ID == id {
					found = true
				}
			}
			if !found || batch.Delivery.ID == "" || batch.Delivery.Token == "" {
				t.Fatalf("retained adapter lost its Notice: %s", output)
			}
			wrongOwner := exec.Command(f.binary, "lookout", "--json", "--receipt", batch.Delivery.ID, "--receipt-outcome", "accepted", "--receipt-token", "wrong-owner")
			wrongOwner.Dir, wrongOwner.Env = f.repo, f.leadEnv
			if output, err := wrongOwner.CombinedOutput(); err == nil {
				t.Fatalf("compatibility path accepted the wrong receipt owner: %s", output)
			}
			before, err := f.db.NoticesByIDs(context.Background(), f.project.ID, []int64{id})
			if err != nil || len(before) != 1 || before[0].DeliveredAt != 0 {
				t.Fatalf("wrong owner changed delivery state: %#v %v", before, err)
			}
			runPosse(t, f.binary, f.repo, f.leadEnv, "lookout", "--json", "--receipt", batch.Delivery.ID, "--receipt-outcome", "accepted", "--receipt-token", batch.Delivery.Token)
			notices, err := f.db.NoticesByIDs(context.Background(), f.project.ID, []int64{id})
			if err != nil || len(notices) != 1 || notices[0].DeliveredAt == 0 {
				t.Fatalf("retained receipt was not settled: %#v %v", notices, err)
			}
		})
	}
}

// Exercise the actual exec boundary with the argv retained by a pre-upgrade
// observer. Adapter handoff/receipt acceptance is covered above.
func Test014LookoutReexecReportsReplacement(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	stopBackgroundWatch(t, f)
	launcher := filepath.Join(f.root, "legacy-observer-reexec")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec "+shellQuote(f.binary)+" lookout \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--poll-only"}, {"--timeout", "1"}, {"--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			command := exec.Command(launcher, args...)
			command.Dir, command.Env = f.repo, f.leadEnv
			output, err := command.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 2 || !strings.Contains(string(output), "posse lookout was removed; use posse watch") || strings.Contains(string(output), "internal_error") {
				t.Fatalf("legacy re-exec did not report its replacement: %v: %s", err, output)
			}
		})
	}
	if pids := watchPIDs(f.root); len(pids) != 0 {
		t.Fatalf("removed observer invocation started a Watch: %v", pids)
	}
}

func Test014ProjectShorthandConfigVocabularyUpgrade(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	stopBackgroundWatch(t, f)
	path := filepath.Join(f.home, "projects", f.project.Name, "config.toml")
	for _, mode := range []string{"local", "no-mistakes"} {
		t.Run(mode, func(t *testing.T) {
			// v0.14.0 docs/spec.md Project example, plus its supported
			// auto_unsaddle shorthand and terminal value.
			original := "# Project configuration\nlanding_mode = '" + mode + "'\nauto_unsaddle = 'landed'\ngate = ['just test']\n[defaults]\nforge = 'gitlab'\n[repositories.worker]\nlanding_mode = 'local'\ntarget_branch = 'staging'\ngate = ['make test']\nforge = 'github'\n[autonomy]\nyolo = false\nreview = 'ask'\nland = 'ask'\n"
			for _, suffix := range []string{".vocabulary", ".v014"} {
				if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			if mode == "no-mistakes" {
				// The rename still runs; the removed value then needs the User's replacement.
				code, out := runReleaseCLI(t, f.binary, f.repo, f.leadEnv, "config", "show", "--project", f.project.Name, "--effective", "--json")
				if code != 1 || !strings.Contains(out, "config_migration_required") || !strings.Contains(out, path+": defaults.merge_mode: no-mistakes was removed; use pr") {
					t.Fatalf("upgraded no-mistakes shorthand did not require migration: %d %s", code, out)
				}
				if data, err := os.ReadFile(path + ".v014"); err != nil || string(data) != original {
					t.Fatalf("original spec config not preserved: %q %v", data, err)
				}
				runPosse(t, f.binary, f.repo, f.leadEnv, "config", "set", "defaults.merge_mode", "pr", "--project", f.project.Name)
				if code, out := runReleaseCLI(t, f.binary, f.repo, f.leadEnv, "config", "show", "--project", f.project.Name, "--effective", "--json"); code != 0 || !strings.Contains(out, `"merge_mode":"pr"`) {
					t.Fatalf("printed replacement did not repair the upgraded config: %d %s", code, out)
				}
				return
			}
			output := runPosse(t, f.binary, f.repo, f.leadEnv, "config", "show", "--project", f.project.Name, "--effective", "--json")
			var result struct {
				Effective config.Config `json:"effective"`
			}
			if err := json.Unmarshal([]byte(output), &result); err != nil {
				t.Fatal(err)
			}
			if result.Effective.Defaults.MergeMode != mode || result.Effective.Defaults.AutoTeardown != "merged" || result.Effective.Repositories["worker"].MergeMode != "local" {
				t.Fatalf("Project shorthand changed meaning: %s", output)
			}
			if data, err := os.ReadFile(path + ".v014"); err != nil || string(data) != original {
				t.Fatalf("original spec config not preserved: %q %v", data, err)
			}
			for i := 0; i < 2; i++ {
				runPosse(t, f.binary, f.repo, f.leadEnv, "config", "show", "--project", f.project.Name, "--json")
			}
		})
	}
}

func Test015ReviewModeUpgradeMigratesLocalWithoutTouchingMergeMode(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	stopBackgroundWatch(t, f)
	globalPath := filepath.Join(f.home, "config.toml")
	projectPath := filepath.Join(f.home, "projects", f.project.Name, "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o700); err != nil {
		t.Fatal(err)
	}
	globalOriginal := "# posse vocabulary: 1\n[defaults]\nreview = 'local' # global review\nmerge_mode = 'local' # delivery stays local\n"
	projectOriginal := "# posse vocabulary: 1\nreview = 'local' # Project shorthand\n[repositories.api]\nreview = 'local' # member review\n"
	if err := os.WriteFile(globalPath, []byte(globalOriginal), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte(projectOriginal), 0o600); err != nil {
		t.Fatal(err)
	}

	output := runPosse(t, f.binary, f.repo, f.leadEnv, "config", "show", "--project", f.project.Name, "--effective", "--json")
	var result struct {
		Effective config.Config `json:"effective"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.Effective.Defaults.Review != "quick" || result.Effective.Defaults.MergeMode != "local" || result.Effective.Repositories["api"].Review != "quick" {
		t.Fatalf("0.15 review migration changed effective policies: %s", output)
	}
	for path, want := range map[string]string{
		globalPath:  "# posse vocabulary: 2\n[defaults]\nreview = 'quick' # global review\nmerge_mode = 'local' # delivery stays local\n",
		projectPath: "# posse vocabulary: 2\nreview = 'quick' # Project shorthand\n[repositories.api]\nreview = 'quick' # member review\n",
	} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("migrated %s = %q, %v; want %q", path, got, err, want)
		}
	}
	for path, want := range map[string]string{globalPath + ".v015": globalOriginal, projectPath + ".v015": projectOriginal} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("backup %s = %q, %v; want original bytes %q", path, got, err, want)
		}
	}
	code, output := runReleaseCLI(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", "local")
	if code == 0 || !strings.Contains(output, "use quick") {
		t.Fatalf("local remained a new-config alias: code=%d output=%s", code, output)
	}
}

func Test014MalformedConfigRetainsFilenameAndErrorCode(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	stopBackgroundWatch(t, f)
	path := filepath.Join(f.home, "projects", f.project.Name, "config.toml")
	if err := os.WriteFile(path, []byte("[defaults\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".vocabulary"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	command := exec.Command(f.binary, "config", "show", "--project", f.project.Name, "--json")
	command.Dir, command.Env = f.repo, f.leadEnv
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), `"code":"config_invalid"`) || !strings.Contains(string(output), path) || !strings.Contains(string(output), "Edit the named config file") {
		t.Fatalf("malformed upgrade config lost diagnostics: %v %s", err, output)
	}
}
