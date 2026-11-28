//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestGreenhouseRepublishRetainsEarlierPREvidence(t *testing.T) {
	f := newWorkerTabsFixtureWithManualWatchOwner(t)
	remote := filepath.Join(f.root, "origin.git")
	origin := "https://github.com/acme/shop.git"
	gitTest(t, f.env, f.repo, "remote", "set-url", "origin", origin)
	gitTest(t, f.env, f.repo, "config", "url.file://"+remote+".insteadOf", origin)
	forge := readTestFile(t, "testdata/t205_fake_forge.py")
	for _, file := range []struct {
		path, content string
		mode          os.FileMode
	}{
		{filepath.Join(f.root, "bin", "gh"), forge, 0o700},
		{filepath.Join(f.root, "quiet-agent"), "", 0o600},
		{filepath.Join(f.home, "projects", "shop", "config.toml"), `review_provider = "greenhouse"
[defaults]
review = "full-pipeline"
merge_mode = "pr"
require_current_target = true
`, 0o600},
	} {
		if err := os.WriteFile(file.path, []byte(file.content), file.mode); err != nil {
			t.Fatal(err)
		}
	}
	const title = "Retain PR evidence"
	const prURL = "https://github.com/acme/shop/pull/17"
	task := f.start(t, "t1", title, "retain-pr-evidence")
	workerEnv := setEnv(workerSignalEnv(f, task), "POSSE_TEST_REMOTE", remote)
	watchEnv := setEnv(setEnv(f.leadEnv, "POSSE_WATCH_TOKEN", ""), "POSSE_TEST_REMOTE", remote)
	worker := func(args ...string) string {
		t.Helper()
		return runPosse(t, f.binary, task.WorktreePath, workerEnv, args...)
	}
	watch := func() {
		t.Helper()
		runPosse(t, f.binary, f.repo, watchEnv, "watch", "--poll-only", "--timeout", "1")
	}
	ctx := context.Background()
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	recordPass := func(head, target, grant string) {
		t.Helper()
		verdict := store.Verdict{TaskID: task.ID, Repository: "shop", Provider: store.GreenhouseReviewProvider, Protocol: "2", Grant: grant, Head: head, Target: target, ReviewBase: target, Outcome: "clean", Mode: "gate"}
		if recorded, err := db.RecordGreenhouseVerdict(ctx, verdict, func(context.Context) (string, error) { return head, nil }); err != nil || !recorded {
			t.Fatalf("record fixture Verdict: recorded=%v err=%v", recorded, err)
		}
		now := time.Now().UnixMilli()
		if err := db.RecordGreenhouseReviewRequest(ctx, store.GreenhouseReviewRequest{ProjectID: f.projectID, TaskID: task.ID, Repository: "shop", RequestedHead: head, Provider: store.GreenhouseReviewProvider, RequestedAt: now, SettledAt: now, Outcome: "verdict", Grant: grant}); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(path, name string) string {
		t.Helper()
		gitTest(t, f.env, path, "commit", "--allow-empty", "-m", name)
		return strings.TrimSpace(gitTest(t, f.env, path, "rev-parse", "HEAD"))
	}
	target := strings.TrimSpace(gitTest(t, f.env, f.repo, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(task.WorktreePath, "change.txt"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, task.WorktreePath, "add", "change.txt")
	oldHead := commit(task.WorktreePath, "Add the change")
	recordPass(oldHead, target, "shop/t1/shop/initial")
	worker("publish", "Initial publication", "--verify", "test original -> passed", "--proof", "Original test log", "--risk", "Risk: original risk\nRollback: revert the original change")
	worker("signal", "done", "Initial publication", "--pr", prURL, "--turn", "launch:1")
	writeJSONFile(t, filepath.Join(f.root, "gh-state.json"), map[string]any{"data": map[string]any{"repo1": map[string]any{"pr1": map[string]any{
		"url": prURL, "state": "OPEN", "headRefOid": oldHead, "baseRefName": "main", "mergeable": "MERGEABLE",
		"statusCheckRollup": map[string]any{"state": "PENDING"},
	}}}})

	newTarget := commit(f.repo, "Advance the target")
	gitTest(t, f.env, f.repo, "push", "origin", "main")
	worker("signal", "working", "Refreshing the target")
	gitTest(t, f.env, task.WorktreePath, "fetch", "origin", "main")
	gitTest(t, f.env, task.WorktreePath, "merge", "--no-edit", "origin/main")
	newHead := strings.TrimSpace(gitTest(t, f.env, task.WorktreePath, "rev-parse", "HEAD"))
	publishArgs := []string{"publish", title}
	runPosseExpectingFailure(t, f, task.WorktreePath, workerEnv, "requires a passing Greenhouse Verdict for this head", publishArgs...)
	if got := strings.TrimSpace(gitTest(t, f.env, remote, "rev-parse", "refs/heads/"+task.Branch)); got != oldHead {
		t.Fatalf("unreviewed head reached origin: %s", got)
	}
	worker("signal", "done", "Request review of the refreshed head", "--head", newHead, "--turn", greenhouseTaskTurn(t, f, task.ID))
	recordPass(newHead, newTarget, "shop/t1/shop/refreshed")
	if _, err := f.client.Run(ctx, "pane", "report-agent", task.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	watch()
	messages, err := db.TaskMessagesAfter(ctx, task.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	command := "`posse publish \"" + title + "\"`"
	var instructed bool
	for _, message := range messages {
		if message.Status != "withdrawn" && strings.Contains(message.Body, "Greenhouse passed Task head "+newHead[:12]) && strings.Contains(message.Body, command) {
			instructed = true
		}
	}
	if !instructed {
		t.Fatalf("no generated plain publish instruction for %s: %+v", newHead, messages)
	}
	if output := worker(publishArgs...); !strings.Contains(output, prURL) {
		t.Fatalf("republish did not reuse the PR: %s", output)
	}
	if got := strings.TrimSpace(gitTest(t, f.env, remote, "rev-parse", "refs/heads/"+task.Branch)); got != newHead {
		t.Fatalf("republish pushed %s, want reviewed head %s", got, newHead)
	}
	body := func() string {
		t.Helper()
		var state struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal([]byte(readTestFile(t, filepath.Join(f.root, "t199-forge.json"))), &state); err != nil {
			t.Fatal(err)
		}
		return state.Body
	}
	assertBody := func(body string, expected, forbidden []string) {
		t.Helper()
		for _, text := range expected {
			if !strings.Contains(body, text) {
				t.Errorf("PR body missing %q", text)
			}
		}
		for _, text := range forbidden {
			if strings.Contains(body, text) {
				t.Errorf("PR body unexpectedly contains %q", text)
			}
		}
	}
	const historical = "Do not treat this as evidence for a new head."
	retained := body()
	assertBody(retained, []string{"test original -> passed", "Original test log", "Risk: original risk", "Rollback: revert the original change", historical, "- Head: `" + newHead[:12] + "`", "- CI: pending"}, []string{"Not run; reason: not supplied", "No proof supplied", "Risk: not stated"})
	worker(publishArgs...)
	if body() != retained {
		t.Error("retry changed the PR body")
	}
	worker("publish", title, "--verify", "test refreshed -> passed")
	assertBody(body(), []string{"## Verification\n\n- [x] test refreshed -> passed", "Original test log", "Risk: original risk", historical}, []string{"test original -> passed"})
	worker("publish", title, "--verify", "test refreshed -> passed", "--proof", "Refreshed test log", "--risk", "Risk: refreshed risk\nRollback: revert this change")
	assertBody(body(), []string{"test refreshed -> passed", "Refreshed test log", "Risk: refreshed risk", "Rollback: revert this change"}, []string{"test original -> passed", "Original test log", "original risk", historical})
}
