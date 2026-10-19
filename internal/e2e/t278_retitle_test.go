//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRetitleThenPublishKeepsTheConventionalPRTitle(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()

	brief := filepath.Join(fixture.root, "change.md")
	initialTitle := "PR lifecycle change"
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: "+initialTitle+"\ndone_when: committed change exists\n---\nCreate a committed change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", brief, "--name", "pr-lifecycle-change")
	task := fixture.mustTask(t, "t1")
	if !waitForCondition(15*time.Second, func() bool {
		count := strings.TrimSpace(gitTest(t, fixture.env, task.WorktreePath, "rev-list", "--count", task.BaseRef+".."+task.Branch))
		return count == "1"
	}) {
		t.Fatal("Worker did not commit before publish")
	}
	workerEnv := setEnv(fixture.leadEnv, "HERDR_PANE_ID", task.PaneID)
	workerEnv = setEnv(workerEnv, "HERDR_WORKSPACE_ID", task.HerdrWorkspaceID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", fixture.home)
	publish := func(summary string) {
		t.Helper()
		command := exec.Command(fixture.binary, "publish", summary)
		command.Dir, command.Env = task.WorktreePath, workerEnv
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("posse publish: %v\n%s", err, output)
		}
	}
	publish("Initial publication")
	if got := readTestFile(t, fixture.ghTitle); got != initialTitle {
		t.Fatalf("initial PR title=%q want %q", got, initialTitle)
	}

	retitled := "fix(publish): preserve the explicitly retitled PR"
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "retitle", "t1", retitled); !strings.Contains(output, retitled) {
		t.Fatalf("retitle did not report the new PR title: %s", output)
	}
	stored := fixture.mustTask(t, "t1")
	if stored.Title != initialTitle || stored.PRTitle != retitled {
		t.Fatalf("retitle changed the Brief title or failed to save the PR title: %#v", stored)
	}

	invalid := exec.Command(fixture.binary, "retitle", "t1", "Not a conventional title")
	invalid.Dir, invalid.Env = fixture.repo, fixture.leadEnv
	if output, err := invalid.CombinedOutput(); err == nil || !strings.Contains(string(output), "pr_title_invalid") {
		t.Fatalf("non-conventional PR title was not rejected: err=%v output=%s", err, output)
	}
	if stored = fixture.mustTask(t, "t1"); stored.Title != initialTitle || stored.PRTitle != retitled {
		t.Fatalf("invalid retitle changed the saved titles: %#v", stored)
	}

	publish("Refresh after Lead retitle")
	if got := readTestFile(t, fixture.ghTitle); got != retitled {
		t.Fatalf("publish refresh changed the retitled PR title to %q, want %q", got, retitled)
	}
	calls := readTestFile(t, fixture.ghLog)
	if strings.Count(calls, "pr create") != 1 || strings.Count(calls, "pr edit") != 1 {
		t.Fatalf("publish did not refresh the existing PR exactly once: %s", calls)
	}
}
