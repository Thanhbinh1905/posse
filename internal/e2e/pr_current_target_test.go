//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

// requireCurrentTargetConfig enables ADR 0019's current-target rule, which is
// off by default.
const requireCurrentTargetConfig = "[defaults]\nrequire_current_target = true\n"

func writePRLifecycleProjectConfig(t *testing.T, fixture *prLifecycleFixture, content string) {
	t.Helper()
	projectConfig := filepath.Join(fixture.home, "projects", "shop", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfig, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// With defaults.require_current_target off (the default), a green, mergeable
// PR behind its target merges without a Worker update, and only a conflicting
// PR is asked to merge the target, outside the merge queue.
func TestPRMergeWithoutCurrentTargetRuleMergesBehindPR(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	writePRLifecycleProjectConfig(t, fixture, "[autonomy]\nland = \"auto\"\n")
	ids := []string{"t1", "t2", "t3"}
	for _, id := range ids {
		brief := filepath.Join(fixture.root, "behind-"+id+".md")
		if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Behind change "+id+"\ndone_when: committed change exists\n---\nMake a change.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		fixture.startAndComplete(t, brief, id)
		if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", id); !strings.Contains(output, "merging") {
			t.Fatalf("%s PR did not open: %s", id, output)
		}
	}
	tasks := map[string]store.Task{}
	for _, id := range ids {
		tasks[id] = fixture.mustTask(t, id)
	}
	openPull := func(id, mergeable string) fakePull {
		return fakePull{alias: "pr" + strings.TrimPrefix(id, "t"), state: "OPEN", checks: "SUCCESS", review: "APPROVED", mergeable: mergeable, head: tasks[id].GatedSHA}
	}
	fixture.writeGraphQLPulls(t, openPull("t1", "MERGEABLE"), openPull("t2", "MERGEABLE"), openPull("t3", "MERGEABLE"))
	fixture.showUntilGreen(t, "t1", tasks["t1"].GatedSHA)
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t1", "--execute"); !strings.Contains(output, "merging") {
		t.Fatalf("t1 PR did not merge: %s", output)
	}
	firstMerge := fixture.mergeOnLocalOrigin(t, tasks["t1"], "t1")
	if err := os.Remove(fixture.ghMerge); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCommand(fixture.env, fixture.remote, "merge-base", "--is-ancestor", firstMerge, tasks["t2"].GatedSHA); err == nil {
		t.Fatal("t2 already contains the new target tip; the scenario needs a behind PR")
	}

	merged := fakePull{alias: "pr1", state: "MERGED", checks: "SUCCESS", review: "APPROVED", mergeable: "MERGEABLE", mergeCommit: firstMerge, head: tasks["t1"].GatedSHA}
	fixture.writeGraphQLPulls(t, merged, openPull("t2", "MERGEABLE"), openPull("t3", "CONFLICTING"))
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t2")

	// The conflicting PR is asked to merge the target without queueing.
	fixture.waitTaskState(t, "t3", store.StateWorking)
	var instruction string
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT body FROM messages WHERE task_id=? ORDER BY id DESC LIMIT 1`, tasks["t3"].ID).Scan(&instruction); err != nil || !strings.Contains(instruction, "Merge origin/main") {
		t.Fatalf("conflicting PR got no merge-target instruction: body=%q err=%v", instruction, err)
	}
	fixture.requireNotice(t, "t3", "pr_conflict")

	// The clean-but-behind PR stays merging with no instruction and merges.
	if current := fixture.mustTask(t, "t2"); current.State != store.StateMerging {
		t.Fatalf("clean-but-behind t2 left merging: %s", current.State)
	}
	assertNoValidationFixRound(t, fixture, "t2", "freshness")
	var queued int
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM merge_queue_entries`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("merge queue used without the current-target rule: entries=%d err=%v", queued, err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t2"); strings.Contains(output, "merge_queue") || strings.Contains(output, "STALE") {
		t.Fatalf("clean-but-behind t2 was treated as stale: %s", output)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t2", "--execute"); !strings.Contains(output, "merging") {
		t.Fatalf("clean-but-behind t2 did not merge: %s", output)
	}
	if _, err := os.Stat(fixture.ghMerge); err != nil {
		t.Fatalf("clean-but-behind t2 did not reach the forge merge command: %v", err)
	}

	// A conflicting PR still cannot merge: the forge reports it unmergeable.
	command := exec.Command(fixture.binary, "merge", "t3", "--execute")
	command.Dir, command.Env = fixture.repo, fixture.leadEnv
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("conflicting t3 merged: %s", output)
	}
}
