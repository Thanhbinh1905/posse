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

	"github.com/thanhbinh1905/posse/internal/store"
)

// The Lead triages a trivial CI failure on a merging PR by itself: it inspects
// the PR, reruns the failed run and answers the reviewer. The original Worker
// receives nothing, every operation is recorded with actor and head, an
// ambiguous forge response is observed instead of repeated, and none of it
// grants merge authority.
func TestLeadStewardsTrivialCIFailureWithoutWakingWorker(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	brief := filepath.Join(fixture.root, "change.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nCreate a committed change for the stewardship E2E.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.startAndComplete(t, brief, "t1")
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t1"); !strings.Contains(output, "merging") {
		t.Fatalf("PR did not reach merging: %s", output)
	}
	stopBackgroundWatch(t, fixture)
	task := fixture.mustTask(t, "t1")
	workerHead := strings.TrimSpace(gitTest(t, fixture.env, task.WorktreePath, "rev-parse", "HEAD"))
	if workerHead != task.GatedSHA {
		t.Fatalf("Worker head %s differs from gated head %s", workerHead, task.GatedSHA)
	}
	runState := filepath.Join(fixture.root, "gh-run-4242.json")
	writeRun := func(attempt int, status string) {
		t.Helper()
		encoded, err := json.Marshal(map[string]any{"attempt": attempt, "status": status, "headSha": task.GatedSHA})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(runState, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeRun(1, "completed")
	failing := fakePull{alias: "pr1", state: "OPEN", checks: "FAILURE", review: "REVIEW_REQUIRED", mergeable: "MERGEABLE", head: task.GatedSHA, detailsURL: "https://github.com/acme/shop/actions/runs/4242/job/77"}
	fixture.writeGraphQLPulls(t, failing)
	marker := func(name string, present bool) {
		t.Helper()
		path := filepath.Join(fixture.root, name)
		if !present {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			return
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ghCalls := func(prefix string) int {
		t.Helper()
		count := 0
		for _, line := range strings.Split(readTestFile(t, fixture.ghLog), "\n") {
			if strings.HasPrefix(line, prefix) {
				count++
			}
		}
		return count
	}
	expectFailure := func(code string, args ...string) string {
		t.Helper()
		command := exec.Command(fixture.binary, args...)
		command.Dir, command.Env = fixture.repo, fixture.leadEnv
		output, err := command.CombinedOutput()
		if err == nil || !strings.Contains(string(output), `"`+code+`"`) {
			t.Fatalf("posse %v: want %s, got err=%v output=%s", args, code, err, output)
		}
		return string(output)
	}

	inspected := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "pr", "inspect", "t1")
	for _, want := range []string{"https://github.com/acme/shop/pull/17", "lead", "publish", task.GatedSHA, "unit", "4242", "pr rerun t1 --run"} {
		if !strings.Contains(inspected, want) {
			t.Fatalf("inspect output lacks %q:\n%s", want, inspected)
		}
	}

	// Stewardship reruns only an existing failed run on the current head.
	expectFailure("pr_rerun_refused", "pr", "rerun", "t1", "--run", "999")
	if calls := ghCalls("run rerun") + ghCalls("run view 999"); calls != 0 {
		t.Fatalf("refused rerun reached the forge %d times", calls)
	}
	expectFailure("usage", "pr", "merge", "t1")
	workerEnv := setEnv(fixture.leadEnv, "HERDR_PANE_ID", task.PaneID)
	workerEnv = setEnv(workerEnv, "HERDR_WORKSPACE_ID", task.HerdrWorkspaceID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", fixture.home)
	worker := exec.Command(fixture.binary, "pr", "rerun", "t1", "--run", "4242")
	worker.Dir, worker.Env = task.WorktreePath, workerEnv
	if output, err := worker.CombinedOutput(); err == nil || !strings.Contains(string(output), `"worker_forbidden"`) {
		t.Fatalf("a Worker ran a steward operation: err=%v output=%s", err, output)
	}

	// The forge starts the rerun but the response is lost. The next attempt
	// observes the started run instead of requesting it again.
	marker("gh-rerun-ambiguous", true)
	expectFailure("forge_outcome_unknown", "pr", "rerun", "t1", "--run", "4242")
	marker("gh-rerun-ambiguous", false)
	if calls := ghCalls("run rerun"); calls != 1 {
		t.Fatalf("ambiguous rerun issued %d forge reruns", calls)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "pr", "rerun", "t1", "--run", "4242"); !strings.Contains(output, "observed") {
		t.Fatalf("repeat after an ambiguous rerun did not observe it: %s", output)
	}
	if calls := ghCalls("run rerun"); calls != 1 {
		t.Fatalf("repeat after an ambiguous rerun requested it again: %d reruns", calls)
	}
	// The rerun failed again; one more rerun is allowed, then the limit holds.
	writeRun(2, "completed")
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "pr", "rerun", "t1", "--run", "4242"); !strings.Contains(output, "requested") {
		t.Fatalf("second rerun was not requested: %s", output)
	}
	writeRun(3, "completed")
	expectFailure("pr_rerun_limit", "pr", "rerun", "t1", "--run", "4242")
	if calls := ghCalls("run rerun"); calls != 2 {
		t.Fatalf("rerun limit allowed %d forge reruns", calls)
	}

	// An ambiguous reply is found on the PR before it could be posted twice.
	reply := "The unit failure was a runner outage; the failed run has been rerun."
	marker("gh-comment-ambiguous", true)
	expectFailure("forge_outcome_unknown", "pr", "reply", "t1", "--body", reply)
	marker("gh-comment-ambiguous", false)
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "pr", "reply", "t1", "--body", reply); !strings.Contains(output, "observed") {
		t.Fatalf("repeat after an ambiguous reply did not observe it: %s", output)
	}
	expectFailure("pr_reply_duplicate", "pr", "reply", "t1", "--body", reply)
	var comments []map[string]string
	if err := json.Unmarshal([]byte(readTestFile(t, filepath.Join(fixture.root, "gh-comments.json"))), &comments); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || !strings.HasPrefix(comments[0]["body"], reply) || !strings.Contains(comments[0]["body"], "posse:steward-operation:") {
		t.Fatalf("reply was not posted exactly once with its operation marker: %#v", comments)
	}

	// A forge outage is reported as retryable uncertainty, never as current state.
	marker("gh-outage", true)
	outage := expectFailure("forge_unavailable", "pr", "inspect", "t1")
	if !strings.Contains(outage, "true") || !strings.Contains(outage, task.GatedSHA) {
		t.Fatalf("forge outage did not name the last observation or retryability: %s", outage)
	}
	expectFailure("forge_unavailable", "pr", "rerun", "t1", "--run", "4242")
	marker("gh-outage", false)
	if calls := ghCalls("run rerun"); calls != 2 {
		t.Fatalf("forge outage issued a rerun: %d reruns", calls)
	}

	// Every operation records the Lead as actor and the head it acted on.
	delivery := struct {
		id              int64
		steward, origin string
	}{}
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT id,steward,origin FROM deliveries WHERE task_id=? AND pr_url=?`, task.ID, task.PRURL).Scan(&delivery.id, &delivery.steward, &delivery.origin); err != nil {
		t.Fatal(err)
	}
	if delivery.steward != "lead" || delivery.origin != "publish" {
		t.Fatalf("delivery record = %#v", delivery)
	}
	operations, err := fixture.db.DeliveryOperations(context.Background(), delivery.id, 0)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[string][]string{}
	for _, operation := range operations {
		if operation.Actor != "lead" || operation.ActorPane != fixture.project.LeadPaneID {
			t.Fatalf("operation %d actor = %s/%s", operation.ID, operation.Actor, operation.ActorPane)
		}
		if operation.HeadSHA != task.GatedSHA && operation.Operation != "inspect" {
			t.Fatalf("operation %d head = %s, want %s", operation.ID, operation.HeadSHA, task.GatedSHA)
		}
		outcomes[operation.Operation] = append(outcomes[operation.Operation], operation.Outcome)
	}
	if got := strings.Join(outcomes["rerun"], ","); got != "succeeded,succeeded" {
		t.Fatalf("rerun outcomes newest first = %s", got)
	}
	if got := strings.Join(outcomes["reply"], ","); got != "succeeded" {
		t.Fatalf("reply outcomes = %s", got)
	}

	// Stewardship never grants merge authority.
	fixture.writeGraphQL(t, "pr1", "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", task.GatedSHA)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "pr", "inspect", "t1")
	expectFailure("merge_approval_required", "merge", "t1", "--execute")
	if _, err := os.Stat(fixture.ghMerge); !os.IsNotExist(err) {
		t.Fatalf("steward operations led to a forge merge: %v", err)
	}

	// The original Worker was never woken.
	var messages int
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE task_id=?`, task.ID).Scan(&messages); err != nil || messages != 0 {
		t.Fatalf("triage queued %d Worker messages: %v", messages, err)
	}
	if head := strings.TrimSpace(gitTest(t, fixture.env, task.WorktreePath, "rev-parse", "HEAD")); head != workerHead {
		t.Fatalf("Worker committed during triage: %s -> %s", workerHead, head)
	}
	if _, err := os.Stat(filepath.Join(task.WorktreePath, "e2e-fix-t1.txt")); !os.IsNotExist(err) {
		t.Fatalf("Worker received a fix instruction: %v", err)
	}
	if current := fixture.mustTask(t, "t1"); current.State != store.StateMerging || current.GatedSHA != task.GatedSHA {
		t.Fatalf("triage changed the Task: %s %s", current.State, current.GatedSHA)
	}
}
