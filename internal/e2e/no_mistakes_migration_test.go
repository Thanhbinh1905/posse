//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

type noMistakesMigrationOutput struct {
	Readiness []struct {
		Code string `json:"code"`
		Fix  string `json:"fix"`
	} `json:"readiness"`
	Migration *struct {
		Scopes []struct {
			Project         string   `json:"project"`
			Set             []string `json:"set"`
			LighterReview   []string `json:"lighter_review"`
			TrustedConfig   string   `json:"trusted_config"`
			TrustedCommands []string `json:"trusted_commands"`
		} `json:"scopes"`
		Notes []string `json:"notes"`
	} `json:"no_mistakes_migration"`
}

func decodeNoMistakesMigration(t *testing.T, output string) noMistakesMigrationOutput {
	t.Helper()
	var result noMistakesMigrationOutput
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode %v: %s", err, output)
	}
	return result
}

func configFiles(t *testing.T, home string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, path := range []string{filepath.Join(home, "config.toml"), filepath.Join(home, "projects", "shop", "config.toml")} {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		files[path] = data
	}
	return files
}

func requireConfigUnchanged(t *testing.T, home string, before map[string][]byte, command string) {
	t.Helper()
	for path, data := range configFiles(t, home) {
		if !bytes.Equal(data, before[path]) {
			t.Fatalf("%s changed %s:\nbefore:\n%s\nafter:\n%s", command, path, before[path], data)
		}
	}
}

// A no-mistakes Project gets copyable replacement settings from doctor and up;
// neither writes config, and a Task admitted under no-mistakes still merges
// through its receipt after the User applies the replacement.
func TestNoMistakesMigrationSuggestsSettingsAndKeepsInFlightTask(t *testing.T) {
	f := reviewEvidenceFixture(t, "no-mistakes")
	receipt, _ := installFakeNoMistakes(t, f)
	f.env = setEnv(f.env, "NM_HOME", filepath.Join(f.root, "nm-home"))
	f.callerEnv = setEnv(f.callerEnv, "NM_HOME", filepath.Join(f.root, "nm-home"))
	if err := os.WriteFile(filepath.Join(f.repo, ".no-mistakes.yaml"), []byte("commands:\n  test: \"go test ./...\"\n  lint: \"make lint\"\nauto_fix:\n  ci: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, f.repo, "add", ".no-mistakes.yaml")
	gitTest(t, f.env, f.repo, "commit", "-m", "no-mistakes config")
	gitTest(t, f.env, f.repo, "push", "origin", "main")

	before := configFiles(t, f.home)
	wantSet := []string{
		"posse config set defaults.review full-pipeline --project shop",
		"posse config set ci_loop.max_fix_rounds 1 --project shop",
	}
	// posse down refuses while Tasks are open, so restart the Lead first. Down
	// closes the Lead workspace; the User runs up from a fresh one.
	runPosse(t, f.binary, f.repo, f.env, "down")
	workspace, err := createWorkspace(herdr.NewWithEnv("herdr", f.env), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	f.callerEnv = setEnv(f.callerEnv, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	f.callerEnv = setEnv(f.callerEnv, "HERDR_WORKSPACE_ID", workspace.Workspace.WorkspaceID)
	f.callerEnv = setEnv(f.callerEnv, "HERDR_TAB_ID", workspace.RootPane.TabID)
	f.leadEnv = f.callerEnv
	command := exec.Command(f.binary, "up", "--name", "shop", "--yes", "--json")
	command.Dir, command.Env = f.repo, f.callerEnv
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("posse up: %v\n%s", err, output)
	}
	requireConfigUnchanged(t, f.home, before, "posse up")
	up := decodeNoMistakesMigration(t, string(output))
	if up.Migration == nil || len(up.Migration.Scopes) != 1 || strings.Join(up.Migration.Scopes[0].Set, "\n") != strings.Join(wantSet, "\n") {
		t.Fatalf("up migration: %s", output)
	}
	gap := false
	for _, readiness := range up.Readiness {
		gap = gap || readiness.Code == "no_mistakes_migration" && strings.Contains(readiness.Fix, wantSet[0])
	}
	if !gap {
		t.Fatalf("up readiness omitted no_mistakes_migration: %s", output)
	}

	change := publishCandidate(t, f, "in-flight change", "author", 17)
	head := strings.TrimSpace(gitTest(t, f.env, change.WorktreePath, "rev-parse", "HEAD"))

	doctor := decodeNoMistakesMigration(t, runPosse(t, f.binary, f.repo, f.env, "doctor", "--json"))
	requireConfigUnchanged(t, f.home, before, "posse doctor")
	if doctor.Migration == nil || len(doctor.Migration.Scopes) != 1 {
		t.Fatalf("doctor migration = %#v", doctor.Migration)
	}
	scope := doctor.Migration.Scopes[0]
	if scope.Project != "shop" || strings.Join(scope.Set, "\n") != strings.Join(wantSet, "\n") ||
		strings.Join(scope.LighterReview, "\n") != "posse config set defaults.review quick --project shop\nposse config set defaults.review none --project shop" ||
		scope.TrustedConfig != "origin/main:.no-mistakes.yaml" || len(scope.TrustedCommands) != 2 ||
		!strings.Contains(scope.TrustedCommands[0], `posse validate test --verify "go test ./... -> <result>"`) {
		t.Fatalf("doctor scope = %#v", scope)
	}
	if notes := strings.Join(doctor.Migration.Notes, "\n"); !strings.Contains(notes, "frozen no-mistakes plan") {
		t.Fatalf("doctor notes = %s", notes)
	}

	// The User chooses the recommended replacement.
	for _, line := range scope.Set {
		runPosse(t, f.binary, f.repo, f.env, strings.Fields(line)[1:]...)
	}
	if after := decodeNoMistakesMigration(t, runPosse(t, f.binary, f.repo, f.env, "doctor", "--json")); after.Migration != nil {
		t.Fatalf("doctor still suggests a migration after the User applied it: %#v", after.Migration)
	}

	// The in-flight Task keeps its frozen no-mistakes policy: only a receipt
	// for its exact head lets it merge.
	requireRefusal(t, f, "review_receipt_unsupported", "merge", "t1")
	writeReceipt(t, receipt, head, "completed", "passed", "model-r")
	if code, out := runReviewIdentityCLI(t, f, "merge", "t1"); code != 0 || !strings.Contains(out, "merging") {
		t.Fatalf("passing receipt refused after migration: %d %s", code, out)
	}
	observePR(t, f, change, 17, "OPEN", "SUCCESS", head, "")
	if code, out := runReviewIdentityCLI(t, f, "merge", "t1", "--execute"); code != 0 {
		t.Fatalf("in-flight no-mistakes Task did not merge: %d %s", code, out)
	}
	if _, err := os.Stat(f.ghMerge); err != nil {
		t.Fatalf("merge was not issued: %v", err)
	}
}
