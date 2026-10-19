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
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path + ".vocabulary"); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
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
