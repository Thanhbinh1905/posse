//go:build e2e && linux

package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestGreenhouseScopeAmendmentReusesPublishedPR(t *testing.T) {
	f := newWorkerTabsFixtureWithManualWatchOwner(t)
	c := newGreenhouseContract(t, f)
	remote := filepath.Join(f.root, "origin.git")
	origin := "https://github.com/acme/shop.git"
	gitTest(t, f.env, f.repo, "remote", "set-url", "origin", origin)
	gitTest(t, f.env, f.repo, "config", "url.file://"+remote+".insteadOf", origin)
	for path, text := range map[string]string{
		filepath.Join(f.root, "bin", "gh"): readTestFile(t, "testdata/t205_fake_forge.py"),
		filepath.Join(f.home, "projects", "shop", "config.toml"): `review_provider = "greenhouse"
[defaults]
review = "full-pipeline"
merge_mode = "pr"
`,
	} {
		if err := os.WriteFile(path, []byte(text), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	c.env = setEnv(setEnv(c.env, "GREENHOUSE_HOME", c.home), "POSSE_TEST_REMOTE", remote)
	const title = "Rotate the guest image"
	const scope = "Add a MySQL filter to the guest image rotation"
	const quote = "tôi muốn chung với !61 để deploy 1 thể luôn"
	const pr = "https://github.com/acme/shop/pull/17"
	lead := func(args ...string) string {
		t.Helper()
		return runPosse(t, f.binary, f.repo, c.env, args...)
	}
	brief := filepath.Join(f.root, "guest-image.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: "+title+"\ndone_when: Rotate the guest image on the regular schedule\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lead("start", "--brief", brief, "--name", "guest-image")
	f.waitState(t, "t1", store.StateWorking)
	c.change = f.task(t, "t1")
	task := c.change
	original, err := task.AdmittedPlan()
	if err != nil || original == nil {
		t.Fatalf("original admitted plan: %v, %v", original, err)
	}
	workerEnv := taskAuthorGenerationEnv(setEnv(setEnv(c.env, "POSSE_TASK_ID", task.ContainmentMarker), "HERDR_PANE_ID", task.PaneID))
	worker := func(args ...string) string {
		t.Helper()
		return runPosse(t, f.binary, task.WorktreePath, workerEnv, args...)
	}
	oldHead := c.authorCommit(t)
	c.requireRecordedGate(t, c.review(t), oldHead, "clean")
	worker("publish", title, "--verify", "fixture tests -> passed", "--proof", "fixture review", "--risk", "Revert the fixture change")
	worker("signal", "done", "Initial publication", "--pr", pr, "--turn", "launch:1")
	forgeState := func(head, ci string) {
		t.Helper()
		writeJSONFile(t, filepath.Join(f.root, "gh-state.json"), map[string]any{"data": map[string]any{"repo1": map[string]any{"pr1": map[string]any{
			"url": pr, "state": "OPEN", "headRefOid": head, "baseRefName": "main", "mergeable": "MERGEABLE",
			"statusCheckRollup": map[string]any{"state": ci},
		}}}})
	}
	forgeState(oldHead, "SUCCESS")
	worker("signal", "working", "Extend the original change")
	lead("send", "--queue", "t1", scope+". User: "+quote)
	if err := os.WriteFile(filepath.Join(task.WorktreePath, "mysql-filter.txt"), []byte("mysql filter\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, task.WorktreePath, "add", "mysql-filter.txt")
	gitTest(t, f.env, task.WorktreePath, "commit", "-m", "Add the MySQL filter")
	newHead := strings.TrimSpace(gitTest(t, f.env, task.WorktreePath, "rev-parse", "HEAD"))
	c.setMode(t, "scope")
	c.requireRecordedGate(t, c.review(t), newHead, "findings")
	intentPath := filepath.Join(c.ctl, "review-intent")
	if got := readTestFile(t, intentPath); !strings.Contains(got, title) || strings.Contains(got, scope) {
		t.Fatalf("review after Lead instruction should reproduce missing scope: %q", got)
	} else {
		t.Logf("review after User-approved Lead instruction raised a P1 at %s; its intent was %q", newHead, got)
	}

	c.setMode(t, "clean")
	c.requireRecordedGate(t, c.review(t), newHead, "clean")
	c.setMode(t, "scope")
	amend := []string{"task", "amend", "t1", "shop", "--head", newHead, "--scope", scope}
	runPosseExpectingFailure(t, f, f.repo, c.env, "user_approval_required", amend...)
	approved := append(append([]string(nil), amend...), "--user-approved", quote)
	runPosseExpectingFailure(t, f, task.WorktreePath, workerEnv, "worker_forbidden", approved...)
	wrongRepo := append([]string(nil), approved...)
	wrongRepo[3] = "other"
	runPosseExpectingFailure(t, f, f.repo, c.env, "scope_repository_mismatch", wrongRepo...)
	wrongHead := append([]string(nil), approved...)
	wrongHead[5] = oldHead
	runPosseExpectingFailure(t, f, f.repo, c.env, "scope_head_mismatch", wrongHead...)
	lead(approved...)
	lead(approved...)
	current, err := f.task(t, "t1").AdmittedPlan()
	if err != nil || current == nil || current.Revision != original.Revision+1 || current.PreviousSHA256 != original.SHA256() || !bytes.Equal(current.Inputs[0].Bytes, original.Inputs[0].Bytes) || len(current.ScopeAmendments) != 1 {
		t.Fatalf("amendment did not append one revision to the original Brief: %+v, %v", current, err)
	}
	approval := current.ScopeAmendments[0]
	if approval.Repository != "shop" || approval.Actor != "lead" || approval.Quote != quote || approval.Scope != scope || approval.PRURL != pr || approval.OldHead != oldHead || approval.NewHead != newHead {
		t.Fatalf("incorrect approval audit: %+v", approval)
	}
	runPosseExpectingFailure(t, f, task.WorktreePath, workerEnv, "requires a passing Greenhouse Verdict for this head", "publish", title)
	remoteHead := func() string {
		return strings.TrimSpace(gitTest(t, f.env, remote, "rev-parse", "refs/heads/"+task.Branch))
	}
	if got := remoteHead(); got != oldHead {
		t.Fatalf("unreviewed head reached the forge: %s", got)
	}
	ctx := context.Background()
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	history, err := db.TaskPlanRevisions(ctx, task.ID)
	if err != nil || len(history) != 2 || history[0] != task.PlanJSON {
		t.Fatalf("the original plan is missing from history: %d revisions, %v", len(history), err)
	}
	poll := func() {
		t.Helper()
		runPosse(t, f.binary, f.repo, setEnv(c.env, "POSSE_WATCH_TOKEN", ""), "watch", "--poll-only", "--timeout", "1")
	}
	idle := func() {
		t.Helper()
		if _, err := f.client.Run(ctx, "pane", "report-agent", task.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
			t.Fatal(err)
		}
		poll()
	}
	idle()
	worker("signal", "done", "Review the amended scope", "--head", newHead, "--turn", greenhouseTaskTurn(t, f, task.ID))
	poll()
	if !waitForCondition(20*time.Second, func() bool {
		verdicts := c.verdicts(t)
		last := verdicts[len(verdicts)-1]
		return last.ScopeRevision == approval.Revision && last.Qualifies(newHead)
	}) {
		t.Fatalf("Watch did not record a passing Verdict for the amended head: %+v", c.verdicts(t))
	}
	if got := readTestFile(t, intentPath); !strings.Contains(got, title) || !strings.Contains(got, scope) || !strings.Contains(got, quote) {
		t.Fatalf("amended reviewer intent lost Brief or approval: %q", got)
	}
	idle()
	readyCount := func(head string) int {
		t.Helper()
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='merge_ready' AND (?='' OR json_extract(data_json,'$.head_sha')=?)`, task.ID, head, head).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	before := readyCount("")
	if output := worker("publish", title); !strings.Contains(output, pr) || remoteHead() != newHead {
		t.Fatalf("publish did not advance the same PR to the reviewed head: %s", output)
	}
	worker("signal", "done", "Report the amended publication", "--pr", pr, "--turn", greenhouseTaskTurn(t, f, task.ID))
	inspect := func(head, checks string) {
		t.Helper()
		output := lead("pr", "inspect", "t1")
		observation, err := db.LatestPRObservation(ctx, task.ID)
		if err != nil || observation.PRURL != pr || observation.HeadSHA != head || !strings.Contains(observation.Checks, `"`+checks+`"`) {
			t.Fatalf("incorrect CI evidence: %+v, %v; inspect: %s", observation, err, output)
		}
	}
	inspect(oldHead, "SUCCESS")
	if readyCount("") != before {
		t.Fatal("green CI on the old head made the amended PR merge-ready")
	}
	forgeState(newHead, "PENDING")
	inspect(newHead, "PENDING")
	if readyCount("") != before {
		t.Fatal("pending CI on the amended head made the PR merge-ready")
	}
	forgeState(newHead, "SUCCESS")
	inspect(newHead, "SUCCESS")
	if readyCount(newHead) != 1 {
		t.Fatal("the reviewed PR did not become merge-ready with exact-head green CI")
	}
	t.Logf("same PR %s became merge-ready at %s after amended review and exact-head green CI", pr, newHead)
}

func (c *greenhouseContract) requireRecordedGate(t *testing.T, result map[string]any, head, outcome string) {
	t.Helper()
	verdicts := c.verdicts(t)
	if len(verdicts) == 0 {
		t.Fatalf("no Verdict for %s: %v", head, result)
	}
	last := verdicts[len(verdicts)-1]
	if result["recorded"] != true || last.Mode != "gate" || last.Head != head || last.Outcome != outcome || (outcome == "findings" && last.Blocking != 1) {
		t.Fatalf("expected %s gate Verdict for %s: result=%v verdict=%+v", outcome, head, result, last)
	}
}
