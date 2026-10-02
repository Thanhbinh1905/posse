//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestHistoricalMultiIssuePublishKeepsClosingLinks(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	briefPath := filepath.Join(fixture.root, "ship.md")
	initial := "---\ntype: ship\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nCreate a committed change.\n"
	if err := os.WriteFile(briefPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "ride", "--brief", briefPath, "--name", "pr-lifecycle-change")

	stored := filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "brief.md")
	historical := strings.Replace(initial, "done_when: committed change exists\n", "done_when: committed change exists\nissues: [12, 16]\nrefs: [18]\n", 1)
	if err := os.WriteFile(stored, []byte(historical), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "signal-gates", "t1"), []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.waitTaskState(t, "t1", store.StateDone)
	if after, err := os.ReadFile(stored); err != nil || !bytes.Equal(after, []byte(historical)) {
		t.Fatalf("historical Brief was rewritten: %s %v", after, err)
	}
	calls, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Closes #12", "Closes #16", "Refs #18"} {
		if !strings.Contains(string(calls), expected) {
			t.Errorf("historical publish lost %q: %s", expected, calls)
		}
	}
}

func TestPRLandingLifecycleAndExternalMerge(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	brief := filepath.Join(fixture.root, "ship.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nCreate a committed change for the PR lifecycle E2E.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	first := fixture.rideAndComplete(t, brief, "t1")
	if first.PRURL != "https://github.com/acme/shop/pull/17" || first.GatedSHA != "" || first.Branch != "posse/pr-lifecycle-change" || first.PaneLabel != "posse:shop:t1" || first.AgentName != "posse-shop-t1-1" {
		t.Fatalf("Worker did not publish its PR before the Lead's Gate: %#v", first)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.env, fixture.remote, "show-ref", "--hash", "refs/heads/posse/pr-lifecycle-change")); got != strings.TrimSpace(gitTest(t, fixture.env, first.WorktreePath, "rev-parse", "HEAD")) {
		t.Fatalf("Worker did not push its own Task branch: %s", got)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t1"); !strings.Contains(output, "landing") {
		t.Fatalf("first PR did not open: %s", output)
	}
	first = fixture.mustTask(t, "t1")
	createdCalls, err := os.ReadFile(fixture.ghLog)
	if err != nil || !strings.Contains(string(createdCalls), "--title PR lifecycle change") || strings.Contains(string(createdCalls), "--title t1") {
		t.Fatalf("forge title did not state the Task work: %q, %v", createdCalls, err)
	}
	if first.PRURL != "https://github.com/acme/shop/pull/17" || first.GatedSHA == "" {
		t.Fatalf("first gated PR was not recorded: %#v", first)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.env, fixture.remote, "show-ref", "--hash", "refs/heads/posse/pr-lifecycle-change")); got != first.GatedSHA {
		t.Fatalf("PR push head=%s, gated SHA=%s", got, first.GatedSHA)
	}

	fixture.writeGraphQL(t, "pr1", "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", first.GatedSHA)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	fixture.requireNotice(t, "t1", "pr_checks_failed")

	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Fix the failing unit check and report done again."); !strings.Contains(output, "delivered") {
		t.Fatalf("PR fix instruction was not delivered: %s", output)
	}
	if err := os.WriteFile(filepath.Join(fixture.fixGate, "t1"), []byte("continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.waitTaskState(t, "t1", store.StateDone)
	first = fixture.mustTask(t, "t1")
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t1"); !strings.Contains(output, "landing") {
		t.Fatalf("fixed PR did not pass the Gate and return to landing: %s", output)
	}
	first = fixture.mustTask(t, "t1")
	fixture.writeGraphQL(t, "pr1", "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", first.GatedSHA)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	fixture.requireNotice(t, "t1", "land_ready")
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t1", "--merge", "--user-approved", "User approved the PR merge"); !strings.Contains(output, "landing") {
		t.Fatalf("approved PR merge did not call GitHub: %s", output)
	}
	if _, err := os.Stat(fixture.ghMerge); err != nil {
		t.Fatalf("fake GitHub did not record the approved merge call: %v", err)
	}
	mergeOne := fixture.mergeOnLocalOrigin(t, first, "t1")
	fixture.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", mergeOne, first.GatedSHA)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	first = fixture.mustTask(t, "t1")
	if first.State != store.StateTornDown || first.LandedRef != mergeOne {
		t.Fatalf("merged PR did not land and auto-teardown: %#v, merge=%s", first, mergeOne)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.env, fixture.repo, "rev-parse", "refs/heads/main")); got != mergeOne {
		t.Fatalf("Project checkout head=%s, merge commit=%s", got, mergeOne)
	}
	fixture.requireNotice(t, "t1", "pr_merged")

	secondBrief := filepath.Join(fixture.root, "ship-second.md")
	if err := os.WriteFile(secondBrief, []byte("---\ntype: ship\ntitle: External merge change\ndone_when: second committed change exists\n---\nCreate a second committed change for external merge observation.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secondPublished := fixture.rideAndComplete(t, secondBrief, "t2")
	if secondPublished.PRURL != "https://github.com/acme/shop/pull/18" || secondPublished.GatedSHA != "" || secondPublished.Branch != "posse/external-merge-change" {
		t.Fatalf("second Worker did not publish its own PR: %#v", secondPublished)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t2"); !strings.Contains(output, "landing") {
		t.Fatalf("second PR did not open: %s", output)
	}
	second := fixture.mustTask(t, "t2")
	mergeTwo := fixture.mergeOnLocalOrigin(t, second, "t2")
	fixture.writeGraphQL(t, "pr2", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", mergeTwo, second.GatedSHA)
	time.Sleep(5 * time.Millisecond)
	offlineEnv := setEnv(fixture.leadEnv, "HERDR_SOCKET_PATH", filepath.Join(fixture.root, "offline-herdr.sock"))
	runPosse(t, fixture.binary, fixture.repo, offlineEnv, "show", "t2")
	second = fixture.mustTask(t, "t2")
	if second.State != store.StateLanded || second.LandedRef != mergeTwo {
		t.Fatalf("merge observed without Herdr did not leave the Task landed: %#v, merge=%s", second, mergeTwo)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t2")
	second = fixture.mustTask(t, "t2")
	if second.State != store.StateTornDown || second.LandedRef != mergeTwo {
		t.Fatalf("external PR merge was not observed and torn down: %#v, merge=%s", second, mergeTwo)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.env, fixture.repo, "rev-parse", "refs/heads/main")); got != mergeTwo {
		t.Fatalf("Project checkout was not synced after external merge: got=%s want=%s", got, mergeTwo)
	}
	fixture.requireNotice(t, "t2", "pr_merged")
	ghCalls, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(ghCalls), "pr create") != 2 || strings.Count(string(ghCalls), "pr merge") != 1 {
		t.Fatalf("unexpected fake GitHub call sequence: %s", ghCalls)
	}
	if !strings.Contains(string(ghCalls), "--match-head-commit "+first.GatedSHA) {
		t.Fatalf("PR merge was not pinned to the gated head: %s", ghCalls)
	}
	if err := fixture.db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredQueuedMessageCrashRaisesNotice(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	brief := filepath.Join(fixture.root, "queued-crash.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nCommit one change for queued message crash recovery.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.rideAndComplete(t, brief, "t1")
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t1"); !strings.Contains(output, "landing") {
		t.Fatalf("Task did not enter landing: %s", output)
	}
	landing := fixture.mustTask(t, "t1")
	fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", landing.GatedSHA)

	client := herdr.NewWithEnv("herdr", fixture.env)
	awaitAgentState := func(want string) {
		t.Helper()
		if waitForCondition(10*time.Second, func() bool {
			snapshot, err := client.Snapshot(context.Background())
			if err != nil {
				return false
			}
			for _, pane := range snapshot.Panes {
				if pane.PaneID == landing.PaneID {
					return pane.AgentStatus == want || want == "idle" && pane.AgentStatus == "done"
				}
			}
			return false
		}) {
			return
		}
		snapshot, err := client.Snapshot(context.Background())
		t.Fatalf("Rider did not reach %s: snapshot=%#v err=%v", want, snapshot, err)
	}
	awaitAgentState("idle")
	if _, err := client.Run(context.Background(), "pane", "report-agent", landing.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "working"); err != nil {
		t.Fatal(err)
	}
	awaitAgentState("working")
	const body = "Check the queued crash recovery"
	queued := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", body, "--queue")
	if !strings.Contains(queued, "state: queued") {
		t.Fatalf("send did not finish with a queued instruction: %s", queued)
	}
	if _, err := client.Run(context.Background(), "pane", "report-agent", landing.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	awaitAgentState("idle")
	var queuedStatus string
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT status FROM messages WHERE task_id=? AND body=?`, landing.ID, body).Scan(&queuedStatus); err != nil || queuedStatus != "queued" {
		t.Fatalf("queued instruction changed before the controlled delivery owner: status=%q err=%v", queuedStatus, err)
	}

	owner := exec.Command(fixture.binary, "roster")
	owner.Dir = fixture.repo
	owner.Env = setEnv(fixture.leadEnv, "POSSE_INTENT_CRASH_AT", "send:before:message.prompt")
	output, err := owner.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 86 {
		var status string
		_ = fixture.db.QueryRowContext(context.Background(), `SELECT status FROM messages WHERE task_id=? AND body=?`, landing.ID, body).Scan(&status)
		snapshot, snapshotErr := client.Snapshot(context.Background())
		var paneState string
		var paneFocused bool
		for _, pane := range snapshot.Panes {
			if pane.PaneID == landing.PaneID {
				paneState, paneFocused = pane.AgentStatus, pane.Focused
			}
		}
		t.Fatalf("delivery owner did not crash before agent.prompt: err=%v output=%s message_status=%s pane_status=%s focused=%t snapshot_err=%v", err, output, status, paneState, paneFocused, snapshotErr)
	}
	message, err := fixture.db.OldestQueuedMessage(context.Background(), landing.ID)
	if !store.IsNotFound(err) {
		t.Fatalf("crashed message remained queued: %#v, %v", message, err)
	}
	var messageID int64
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT id FROM messages WHERE task_id=? AND body=? AND status='submitting'`, landing.ID, body).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE messages SET claimed_at=? WHERE id=?`, time.Now().Add(-3*time.Minute).UnixMilli(), messageID); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "roster")
	}
	show := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "--json", "show", "t1", "--full")
	for _, expected := range []string{"uncertain_messages", strconv.FormatInt(messageID, 10), body} {
		if !strings.Contains(show, expected) {
			t.Fatalf("Task inspection omitted uncertain instruction %q: %s", expected, show)
		}
	}
	lookout := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "lookout", "--timeout", "1")
	for _, expected := range []string{"message_delivery_uncertain", "#" + strconv.FormatInt(messageID, 10), body, "posse peek t1", "posse show t1 --full"} {
		if !strings.Contains(lookout, expected) {
			t.Fatalf("Lookout omitted actionable uncertainty %q: %s", expected, lookout)
		}
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	wantInstructions := map[int64]string{messageID: body}
	uncertain := 0
	for _, notice := range notices {
		if notice.Kind != "message_delivery_uncertain" || notice.TaskID != landing.ID {
			continue
		}
		uncertain++
		var data struct {
			MessageID   int64  `json:"message_id"`
			Instruction string `json:"instruction"`
		}
		if err := json.Unmarshal([]byte(notice.DataJSON), &data); err != nil {
			t.Fatalf("decode uncertainty Notice: %v", err)
		}
		if expected, ok := wantInstructions[data.MessageID]; !ok || expected != data.Instruction {
			t.Fatalf("uncertainty Notice identified the wrong instruction: %#v", notice)
		}
		delete(wantInstructions, data.MessageID)
	}
	if uncertain != 1 || len(wantInstructions) != 0 {
		t.Fatalf("uncertainty Notices = %d, missing instructions %#v", uncertain, wantInstructions)
	}
	var status string
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT status FROM messages WHERE id=?`, messageID).Scan(&status); err != nil || status != "submitting" {
		t.Fatalf("ambiguous submission %d was retried or lost: status=%q err=%v", messageID, status, err)
	}
	if _, err := os.Stat(filepath.Join(landing.WorktreePath, "e2e-fix-t1.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crashed instruction was delivered despite the no-retry guarantee: err=%v", err)
	}
}

func TestExternalMergeDuringFollowUpWithMovedTaskBranch(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	brief := filepath.Join(fixture.root, "follow-up.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR follow-up\ndone_when: committed change exists\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.rideAndComplete(t, brief, "t1")
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t1")
	landing := fixture.mustTask(t, "t1")
	if landing.PRURL != "https://github.com/acme/shop/pull/17" || landing.GatedSHA == "" {
		t.Fatalf("PR ownership was not recorded: %#v", landing)
	}
	fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", landing.GatedSHA)
	if err := os.WriteFile(filepath.Join(fixture.root, "pause-fix-commit"), []byte("wait"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Handle the follow-up.")
	if current := fixture.mustTask(t, "t1"); current.State != store.StateWorking || current.GatedSHA != "" || current.PRURL != landing.PRURL {
		t.Fatalf("follow-up did not return to working: %#v", current)
	}
	merge := fixture.mergeOnLocalOrigin(t, landing, "t1")
	// Other Tasks can Land on main before the Rider merges origin/main.
	extra := filepath.Join(fixture.root, "external-merge-t1")
	if err := os.WriteFile(filepath.Join(extra, "other-main-work.txt"), []byte("another Task\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.env, extra, "add", "other-main-work.txt")
	gitTest(t, fixture.env, extra, "commit", "-m", "other Task on main")
	gitTest(t, fixture.env, extra, "push", "origin", "HEAD:refs/heads/main")
	// Keep this test ref isolated from origin/main, which Project sync may fetch concurrently.
	gitTest(t, fixture.env, landing.WorktreePath, "fetch", "--no-write-fetch-head", "--refmap=", "origin", "main:refs/remotes/posse-test/main")
	gitTest(t, fixture.env, landing.WorktreePath, "merge", "--no-edit", "refs/remotes/posse-test/main")
	movedTip := strings.TrimSpace(gitTest(t, fixture.env, landing.WorktreePath, "rev-parse", "HEAD"))
	if movedTip == landing.GatedSHA {
		t.Fatal("follow-up did not move the Task branch tip")
	}
	fixture.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, landing.GatedSHA)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	current := fixture.mustTask(t, "t1")
	if verified, err := fixture.db.WasVerifiedPRHead(context.Background(), current.ID, current.PRURL, movedTip); err != nil || verified {
		t.Fatalf("moved local tip was recorded as verified PR head: %v %v", verified, err)
	}
	if current.State != store.StateTornDown || current.LandedRef != merge {
		status, _ := gitCommand(fixture.env, landing.WorktreePath, "status", "--porcelain", "--untracked-files=all", "--ignored")
		diff, diffErr := gitCommand(fixture.env, landing.WorktreePath, "diff", "--quiet", merge, "HEAD")
		t.Fatalf("externally merged PR was not Landed during follow-up: %#v status=%q diff=%q err=%v", current, status, diff, diffErr)
	}
	fixture.requireNotice(t, "t1", "pr_merged")
	if got := strings.TrimSpace(gitTest(t, fixture.env, fixture.repo, "rev-parse", "refs/heads/"+landing.Branch)); got != movedTip {
		t.Fatalf("Task branch moved during teardown: got=%s want=%s", got, movedTip)
	}
	if current.PRURL != landing.PRURL || current.Branch != landing.Branch {
		t.Fatalf("merged PR identity changed: %#v", current)
	}
	for i := 0; i < 2; i++ {
		runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	}
	// A new CLI process opens the same database after the first observation.
	calls, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "pr create") != 1 || strings.Count(string(calls), "pr merge") != 0 {
		t.Fatalf("watcher created or merged a PR: %s", calls)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	merged := 0
	for _, notice := range notices {
		if notice.TaskID == current.ID && notice.Kind == "pr_merged" {
			merged++
		}
	}
	if merged != 1 {
		t.Fatalf("merge Notices=%d want 1", merged)
	}
}

// A missing old PR must not prevent a merged PR in the same GraphQL batch
// from landing and releasing its Rider, even when gh exits unsuccessfully.
func TestPRPollPartialGraphQLFailureDoesNotStarveMergedRider(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	brief := filepath.Join(fixture.root, "partial-merge.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nObserve a merge despite an inaccessible old PR.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.rideAndComplete(t, brief, "t1")
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t1"); !strings.Contains(output, "landing") {
		t.Fatalf("valid PR did not enter landing: %s", output)
	}
	valid := fixture.mustTask(t, "t1")
	badID, err := fixture.db.CreateTask(context.Background(), fixture.project.ID, store.Task{
		Seq: 2, Type: "ship", Title: "inaccessible old PR", State: store.StateSpawning,
		LandingMode: "pr", Branch: "posse/old-pr", BaseRef: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct{ from, to store.State }{
		{store.StateSpawning, store.StateWorking}, {store.StateWorking, store.StateDone}, {store.StateDone, store.StateLanding},
	} {
		source := "cli"
		if transition.to == store.StateDone {
			source = "worker"
		}
		if err := fixture.db.Transition(context.Background(), badID, transition.from, transition.to, source, "old PR"); err != nil {
			t.Fatal(err)
		}
	}
	merge := fixture.mergeOnLocalOrigin(t, valid, "t1")
	fixture.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, valid.GatedSHA)
	var response map[string]any
	encoded, err := os.ReadFile(fixture.ghState)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	response["data"].(map[string]any)["repo1"].(map[string]any)["pr2"] = nil
	response["errors"] = []any{map[string]any{"message": "Could not resolve to a PullRequest with the number of 41.", "path": []string{"repo1", "pr2"}, "type": "NOT_FOUND"}}
	encoded, err = json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.ghState, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "gh-exit-nonzero"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Do not expose the old PR to Lookout until its GraphQL response is ready.
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET pr_url=? WHERE id=?`, "https://github.com/acme/shop/pull/41", badID); err != nil {
		t.Fatal(err)
	}
	beforePolls, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{{"show", "t1"}, {"show", "t1"}, {"roster"}} {
		time.Sleep(5 * time.Millisecond)
		runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, command...)
		landed := fixture.mustTask(t, "t1")
		if landed.State != store.StateTornDown || landed.LandedRef != merge {
			t.Fatalf("%v missed valid merge or safe teardown: %#v", command, landed)
		}
		mounts, err := fixture.db.Mounts(context.Background(), fixture.project.ID)
		if err != nil || len(mounts) != 1 || mounts[0].TaskID != 0 || mounts[0].State != "idle" {
			t.Fatalf("merged Rider still holds its Mount: %#v, %v", mounts, err)
		}
		fixture.requireNotice(t, "t1", "pr_merged")
		// Lookout can claim a poll before the old URL is exposed, then read
		// only pr1. Allow the next poll to see the old URL before checking the
		// invalid PR Decision; it must still be raised, not skipped.
		var decisions []store.Decision
		if !waitForCondition(10*time.Second, func() bool {
			var err error
			decisions, err = fixture.db.Decisions(context.Background(), fixture.project.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			for _, decision := range decisions {
				if decision.TaskID == badID && decision.Kind == "pr_closed" {
					return true
				}
			}
			runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
			return false
		}) {
			calls, _ := os.ReadFile(fixture.ghLog)
			notices, _ := fixture.db.Notices(context.Background(), fixture.project.ID, false)
			watch, _ := fixture.db.ProjectWatchState(context.Background(), fixture.project.ID)
			t.Fatalf("%v: invalid PR has no User Decision: decisions=%#v notices=%#v watch=%#v ghCalls=%q", command, decisions, notices, watch, calls)
		}
		watch, err := fixture.db.ProjectWatchState(context.Background(), fixture.project.ID)
		if err != nil || watch.PRConsecutiveFailures != 0 {
			t.Fatalf("target-specific failure blocked healthy project polling: %#v, %v", watch, err)
		}
		bad := fixture.mustTask(t, "t2")
		if bad.State != store.StateLanding {
			t.Fatalf("bad PR was changed despite failed observation: %#v", bad)
		}
	}
	afterPolls, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture sets pr_poll=1ms. Its live Lookout may legitimately poll
	// between these CLI calls, so an exact count races the background poller.
	if polls := strings.Count(string(afterPolls), "api graphql") - strings.Count(string(beforePolls), "api graphql"); polls < 3 {
		t.Fatalf("expected at least one poll per CLI restart, got %d", polls)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	mergeCount, failureCount := 0, 0
	for _, notice := range notices {
		if notice.TaskID == valid.ID && notice.Kind == "pr_merged" {
			mergeCount++
		}
		if notice.TaskID == badID && notice.Kind == "pr_watch_failing" {
			failureCount++
		}
	}
	if mergeCount != 1 || failureCount != 0 {
		t.Fatalf("repeated/restarted polls duplicated Notices: merge=%d failure=%d", mergeCount, failureCount)
	}
}

func TestMergedPRSnapshotsUnmergedFollowUp(t *testing.T) {
	for _, mode := range []string{"uncommitted", "committed"} {
		t.Run(mode, func(t *testing.T) {
			f := newPRLifecycleFixture(t)
			defer f.db.Close()
			brief := filepath.Join(f.root, "follow-up.md")
			if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR follow-up\ndone_when: committed change exists\n---\nMake a PR change.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			f.rideAndComplete(t, brief, "t1")
			runPosse(t, f.binary, f.repo, f.leadEnv, "land", "t1")
			landing := f.mustTask(t, "t1")
			f.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", landing.GatedSHA)
			if err := os.WriteFile(filepath.Join(f.root, "pause-fix-commit"), []byte("wait"), 0o600); err != nil {
				t.Fatal(err)
			}
			runPosse(t, f.binary, f.repo, f.leadEnv, "send", "t1", "Add the follow-up.")
			merge := f.mergeOnLocalOrigin(t, landing, "t1")
			work := filepath.Join(landing.WorktreePath, "follow-up-work.txt")
			if err := os.WriteFile(work, []byte("unmerged work\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if mode == "committed" {
				gitTest(t, f.env, landing.WorktreePath, "add", "follow-up-work.txt")
				gitTest(t, f.env, landing.WorktreePath, "commit", "-m", "unmerged follow-up")
			}
			f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, landing.GatedSHA)
			runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t1")
			if task := f.mustTask(t, "t1"); task.State != store.StateTornDown || task.LandedRef != merge {
				t.Fatalf("merged Task did not release its Rider: %#v", task)
			}
			assertMountUnlocked(t, f.env, f.repo, landing.WorktreePath)
			f.requireNotice(t, "t1", "pr_merged")
			if content := gitTest(t, f.env, f.repo, "show", "refs/heads/posse/pr-follow-up-leftover:follow-up-work.txt"); content != "unmerged work\n" {
				t.Fatalf("Leftover did not preserve %s work: %q", mode, content)
			}
			decisions, err := f.db.Decisions(context.Background(), f.project.ID, true)
			if err != nil || len(decisions) != 1 || decisions[0].Kind != "leftover" {
				t.Fatalf("Leftover Decision: %#v %v", decisions, err)
			}
			if mode == "committed" {
				// A squash merge has a different commit ID from the old Task.
				// The next PR must contain only the unmerged follow-up diff.
				if _, err := f.db.AnswerDecision(context.Background(), f.project.ID, decisions[0].ID, "open-task", "Open a follow-up Task"); err != nil {
					t.Fatal(err)
				}
				runPosse(t, f.binary, f.repo, f.leadEnv, "apply", strconv.FormatInt(decisions[0].ID, 10))
				gitTest(t, f.env, f.repo, "fetch", "origin")
				gitTest(t, f.env, f.repo, "merge", "--ff-only", "origin/main")
				followup := filepath.Join(f.root, "leftover-next.md")
				if err := os.WriteFile(followup, []byte("---\ntype: ship\ntitle: From Leftover\ndone_when: follow-up change exists\n---\nKeep only the follow-up.\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runPosse(t, f.binary, f.repo, f.leadEnv, "ride", "--brief", followup, "--name", "from-leftover", "--from-leftover", strconv.FormatInt(decisions[0].ID, 10))
				next := f.mustTask(t, "t2")
				diff := gitTest(t, f.env, next.WorktreePath, "diff", "--name-only", "origin/main...HEAD")
				if strings.Contains(diff, "e2e-worker-t1.txt") || !strings.Contains(diff, "follow-up-work.txt") {
					t.Fatalf("new PR includes old squash-merged work or loses the Leftover: %q", diff)
				}
			}
		})
	}
}

func TestMergedPRWithIgnoredArtifactStillLands(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	brief := filepath.Join(f.root, "ignored-artifact.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.rideAndComplete(t, brief, "t1")
	runPosse(t, f.binary, f.repo, f.leadEnv, "land", "t1")
	landing := f.mustTask(t, "t1")
	exclude := filepath.Join(f.root, "exclude")
	if err := os.WriteFile(exclude, []byte("build.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, landing.WorktreePath, "config", "core.excludesFile", exclude)
	artifact := filepath.Join(landing.WorktreePath, "build.log")
	if err := os.WriteFile(artifact, []byte("ignored build output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	merge := f.mergeOnLocalOrigin(t, landing, "t1")
	f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, landing.GatedSHA)
	runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t1")
	if task := f.mustTask(t, "t1"); task.State != store.StateTornDown || task.LandedRef != merge {
		t.Fatalf("ignored artifact blocked normal landing: %#v", task)
	}
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("ignored artifact was discarded by default clean mode: %v", err)
	}
}

func TestMergedPRAutoTearsDownEvenWhenAutoUnsaddleIsNever(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	configPath := filepath.Join(f.home, "config.toml")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(strings.Replace(string(configData), "auto_unsaddle = \"finished\"", "auto_unsaddle = \"never\"", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(f.root, "late-work.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.rideAndComplete(t, brief, "t1")
	runPosse(t, f.binary, f.repo, f.leadEnv, "land", "t1")
	landing := f.mustTask(t, "t1")
	merge := f.mergeOnLocalOrigin(t, landing, "t1")
	f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, landing.GatedSHA)
	runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t1")
	if task := f.mustTask(t, "t1"); task.State != store.StateTornDown {
		t.Fatalf("merged PR did not tear down despite auto_unsaddle=never: %#v", task)
	}
	f.requireNotice(t, "t1", "pr_merged")
}

func TestPRLandingAcceptsFollowUpBeforeFailureNotice(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	brief := filepath.Join(fixture.root, "follow-up.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR follow-up\ndone_when: follow-up is committed\n---\nAllow follow-up work while the PR is landing.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	published := fixture.rideAndComplete(t, brief, "t1")
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t1"); !strings.Contains(output, "landing") {
		t.Fatalf("PR did not enter landing: %s", output)
	}
	landing := fixture.mustTask(t, "t1")
	fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", landing.GatedSHA)

	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.TaskID == landing.ID && (notice.Kind == "pr_checks_failed" || notice.Kind == "pr_changes_requested" || notice.Kind == "pr_conflict") {
			t.Fatalf("failure Notice exists before follow-up send: %#v", notice)
		}
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Add the expanded follow-up work."); !strings.Contains(output, "delivered") {
		t.Fatalf("follow-up send before a failure Notice was not delivered: %s", output)
	}
	working := fixture.mustTask(t, "t1")
	if working.State != store.StateWorking || working.GatedSHA != "" || working.PRURL != landing.PRURL || published.PRURL != landing.PRURL {
		t.Fatalf("follow-up did not return the same PR Task to working and clear its gate: published=%#v landing=%#v working=%#v", published, landing, working)
	}

	if err := os.WriteFile(filepath.Join(fixture.fixGate, "t1"), []byte("continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.waitTaskState(t, "t1", store.StateDone)
	completed := fixture.mustTask(t, "t1")
	fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", strings.TrimSpace(gitTest(t, fixture.env, fixture.remote, "rev-parse", "refs/heads/posse/pr-follow-up")))
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t1"); !strings.Contains(output, "landing") {
		t.Fatalf("follow-up could not be re-gated: %s", output)
	}
	relanded := fixture.mustTask(t, "t1")
	if reLandedState := relanded.State; reLandedState != store.StateLanding || relanded.GatedSHA == "" || relanded.PRURL != landing.PRURL || completed.State != store.StateDone {
		t.Fatalf("Task did not return to landing after the follow-up: completed=%#v relanded=%#v", completed, relanded)
	}
}

func TestLandReadyBeforeMergeDoesNotRaiseLatePROpened(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	brief := filepath.Join(fixture.root, "ready-before-merge.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nExercise the order of PR lifecycle Notices.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := fixture.rideAndComplete(t, brief, "t1")
	head := strings.TrimSpace(gitTest(t, fixture.env, task.WorktreePath, "rev-parse", task.Branch))
	fixture.writeGraphQL(t, "pr1", "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", head)
	if !waitForCondition(30*time.Second, func() bool {
		notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
		if err != nil {
			return false
		}
		for _, notice := range notices {
			if notice.TaskID == task.ID && notice.Kind == "land_ready" {
				return true
			}
		}
		return false
	}) {
		t.Fatal("PR watcher did not raise land_ready")
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t1", "--merge", "--user-approved", "User approved the pull request")

	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var opened, ready []store.Notice
	for _, notice := range notices {
		if notice.TaskID != task.ID {
			continue
		}
		switch notice.Kind {
		case "pr_opened":
			opened = append(opened, notice)
		case "land_ready":
			ready = append(ready, notice)
		}
	}
	if len(opened) != 1 || len(ready) != 1 {
		t.Fatalf("expected one pr_opened and one land_ready Notice, got opened=%#v ready=%#v", opened, ready)
	}
	if opened[0].ID >= ready[0].ID {
		t.Fatalf("pr_opened was raised after land_ready: opened=%#v ready=%#v", opened[0], ready[0])
	}
	var openedData struct {
		URL  string `json:"url"`
		Head string `json:"head_sha"`
	}
	if err := json.Unmarshal([]byte(opened[0].DataJSON), &openedData); err != nil {
		t.Fatal(err)
	}
	if openedData.URL != task.PRURL || openedData.Head != head {
		t.Fatalf("pr_opened does not identify the observed PR head: %#v", openedData)
	}
}

func TestWorkerPublishRetriesLaggingOpenPRHead(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	brief := filepath.Join(fixture.root, "publish-follow-up.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR follow-up\ndone_when: committed change exists\n---\nExercise a stale PR head immediately after a push.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := fixture.rideAndComplete(t, brief, "t1")
	oldHead := strings.TrimSpace(gitTest(t, fixture.env, task.WorktreePath, "rev-parse", task.Branch))
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t1")
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Add the publish consistency follow-up.")
	if !waitForCondition(30*time.Second, func() bool {
		current := strings.TrimSpace(gitTest(t, fixture.env, task.WorktreePath, "rev-parse", task.Branch))
		return current != oldHead
	}) {
		t.Fatal("follow-up Worker did not commit a new Task head")
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "gh-list-stale-head"), []byte(oldHead), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "gh-list-stale-head-remaining"), []byte("4"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.fixGate, "t1"), []byte("continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	finished := waitForCondition(30*time.Second, func() bool {
		if fixture.mustTask(t, "t1").State == store.StateDone {
			return true
		}
		log, _ := os.ReadFile(filepath.Join(fixture.root, "worker-delivery.log"))
		return strings.Contains(string(log), "branch_moved") || strings.Contains(string(log), "pr_list_failed")
	})
	if !finished {
		log, _ := os.ReadFile(filepath.Join(fixture.root, "worker-delivery.log"))
		t.Fatalf("Worker did not finish or report publish failure: %s", log)
	}
	if current := fixture.mustTask(t, "t1"); current.State != store.StateDone {
		log, _ := os.ReadFile(filepath.Join(fixture.root, "worker-delivery.log"))
		t.Fatalf("publish rejected the temporarily stale PR head: task=%#v Worker delivery=%s", current, log)
	}
	if remaining, err := os.ReadFile(filepath.Join(fixture.root, "gh-list-stale-head-remaining")); err != nil || strings.TrimSpace(string(remaining)) != "0" {
		t.Fatalf("fake forge did not serve four stale reads before the current head: remaining=%q err=%v", remaining, err)
	}
	newHead := strings.TrimSpace(gitTest(t, fixture.env, task.WorktreePath, "rev-parse", task.Branch))
	remoteHead := strings.TrimSpace(gitTest(t, fixture.env, fixture.remote, "rev-parse", "refs/heads/"+task.Branch))
	if newHead == oldHead || remoteHead != newHead {
		t.Fatalf("publish did not push the new Task head: local=%s old=%s remote=%s", newHead, oldHead, remoteHead)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	openedHeads := map[string]int{}
	for _, notice := range notices {
		if notice.TaskID != task.ID || notice.Kind != "pr_opened" {
			continue
		}
		var data struct {
			Head string `json:"head_sha"`
		}
		if err := json.Unmarshal([]byte(notice.DataJSON), &data); err != nil {
			t.Fatal(err)
		}
		openedHeads[data.Head]++
	}
	if len(openedHeads) != 2 || openedHeads[oldHead] != 1 || openedHeads[newHead] != 1 {
		t.Fatalf("expected one pr_opened Notice per published head, got %#v", openedHeads)
	}
	calls, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "pr list") < 6 {
		t.Fatalf("publish did not re-read four stale PR heads before the update: %s", calls)
	}
}

func TestPRCreateCrashRecoveryAdoptsOpenPullRequest(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	brief := filepath.Join(fixture.root, "crash-ship.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR create recovery\ndone_when: committed change exists\n---\nExercise adoption of a PR created before Posse persisted its URL.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Simulate a Task completed before Workers published their own PRs.
	// The compatibility path still lets Land recover a create interrupted
	// after the forge accepted it but before Posse persisted its URL.
	fixture.rideAndCommitLegacy(t, brief, "t1")
	crashEnv := setEnv(fixture.leadEnv, "POSSE_INTENT_CRASH_AT", "land --open-pr:after:pr.create")
	command := exec.Command(fixture.binary, "land", "t1")
	command.Dir = fixture.repo
	command.Env = crashEnv
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("Land did not crash at the injected post-create boundary: err=%v output=%s", err, output)
	}
	task := fixture.mustTask(t, "t1")
	openPRs, err := json.Marshal([]map[string]string{{
		"url": "https://github.com/acme/shop/pull/17", "headRefName": task.Branch, "headRefOid": task.GatedSHA,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.ghOpenPRs, openPRs, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", task.GatedSHA)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	// The Herdr hook can claim this intent concurrently with show. Wait for its
	// state, URL and Notice to commit before checking the recovered result.
	if !waitForCondition(30*time.Second, func() bool {
		task := fixture.mustTask(t, "t1")
		if task.State != store.StateLanding || task.PRURL != "https://github.com/acme/shop/pull/17" {
			return false
		}
		notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
		if err != nil {
			return false
		}
		for _, notice := range notices {
			if notice.TaskID == task.ID && notice.Kind == "pr_opened" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("open PR was not fully adopted after the create crash: task=%#v", fixture.mustTask(t, "t1"))
	}
	calls, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "pr create") != 1 || strings.Count(string(calls), "pr list") < 2 {
		t.Fatalf("unexpected lookup/create recovery calls: %s", calls)
	}
	fixture.requireNotice(t, "t1", "pr_opened")
	if err := fixture.db.Close(); err != nil {
		t.Fatal(err)
	}
}

type prLifecycleFixture struct {
	root      string
	repo      string
	remote    string
	home      string
	binary    string
	env       []string
	leadEnv   []string
	fixGate   string
	ghLog     string
	ghState   string
	ghOpenPRs string
	ghMerge   string
	db        *store.DB
	project   store.Project
}

func newPRLifecycleFixture(t *testing.T) *prLifecycleFixture {
	return newPRLifecycleFixtureWithLeadAndShell(t, false, false)
}

func newPRLifecycleFixtureWithLead(t *testing.T, liveLead bool) *prLifecycleFixture {
	return newPRLifecycleFixtureWithLeadAndShell(t, liveLead, false)
}

func newPRLifecycleFixtureWithSlowLoginShell(t *testing.T) *prLifecycleFixture {
	return newPRLifecycleFixtureWithLeadAndShell(t, false, true)
}

func newPRLifecycleFixtureWithLeadAndShell(t *testing.T, liveLead, slowLoginShell bool) *prLifecycleFixture {
	t.Helper()
	root := newFixtureRoot(t, fixturePrefix("pr-"))
	binDir := filepath.Join(root, "bin")
	worktrees := filepath.Join(root, "posse", "remuda")
	fixGate := filepath.Join(root, "fix-gates")
	for _, directory := range []string{binDir, worktrees, fixGate} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, directory := range []string{filepath.Join(root, "claude", "skills"), filepath.Join(root, "codex")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := isolatedE2EEnv(t, root)
	env = setEnv(env, "POSSE_TEST_ROOT", root)
	env = setEnv(env, "POSSE_E2E_WORKTREES", worktrees)
	env = setEnv(env, "POSSE_E2E_FIX_GATE", fixGate)
	env = setEnv(env, "POSSE_E2E_SIGNAL_GATE", filepath.Join(root, "signal-gates"))
	env = setEnv(env, "POSSE_E2E_POSSE_BIN", filepath.Join(binDir, "posse"))
	env = setEnv(env, "POSSE_TEST_GH_LOG", filepath.Join(root, "gh.log"))
	env = setEnv(env, "POSSE_TEST_GH_STATE", filepath.Join(root, "gh-state.json"))
	env = setEnv(env, "POSSE_TEST_GH_OPEN_PRS", filepath.Join(root, "gh-open-prs.json"))
	env = setEnv(env, "POSSE_TEST_GH_MERGE", filepath.Join(root, "gh-merge-called"))
	env = setEnv(env, "POSSE_TEST_REMOTE", filepath.Join(root, "origin.git"))
	env = setEnv(env, "POSSE_TEST_GH_FAIL", "")
	env = setEnv(env, "PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for key, value := range map[string]string{
		"GIT_AUTHOR_NAME": "Posse E2E", "GIT_AUTHOR_EMAIL": "posse-e2e@example.test",
		"GIT_COMMITTER_NAME": "Posse E2E", "GIT_COMMITTER_EMAIL": "posse-e2e@example.test",
	} {
		env = setEnv(env, key, value)
	}
	if slowLoginShell {
		shell := filepath.Join(binDir, "slow-login-shell")
		marker := filepath.Join(root, "slow-login-enabled")
		starts := filepath.Join(root, "slow-login-starts")
		script := "#!/bin/sh\nif [ -e " + shellQuote(marker) + " ]; then printf 'started\\n' >> " + shellQuote(starts) + "; sleep 1.5; fi\nexec /bin/sh -l\n"
		if err := os.WriteFile(shell, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := herdr.WriteIsolatedConfigWithShell(root, shell); err != nil {
			t.Fatal(err)
		}
	} else if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}

	module := moduleRoot(t)
	binary := filepath.Join(binDir, "posse")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = module
	build.Env = env // isolatedE2EEnv shares the parent Go caches.
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse for PR E2E: %v\n%s", err, output)
	}
	claude := filepath.Join(binDir, "claude")
	agentScript := `#!/bin/sh
set -eu
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
case "$PWD/" in
  "$POSSE_E2E_WORKTREES/"*)
    IFS= read -r prompt || exit 0
    launch_path=${prompt#Read }
    launch_path=${launch_path% and follow it.}
    task_id=$(basename "$(dirname "$launch_path")")
    printf 'worker change %s\n' "$task_id" > "e2e-worker-$task_id.txt"
    git add "e2e-worker-$task_id.txt"
    git commit -m "worker change $task_id" >/dev/null
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
    while [ ! -e "$POSSE_E2E_SIGNAL_GATE/$task_id" ]; do sleep 0.02; done
    case "$task_id" in t1) pr_url='https://github.com/acme/shop/pull/17' ;; t2) pr_url='https://github.com/acme/shop/pull/18' ;; esac
    "$POSSE_E2E_POSSE_BIN" publish "Worker committed $task_id" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
    "$POSSE_E2E_POSSE_BIN" holler done "Worker committed $task_id" --pr "$pr_url" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
    while IFS= read -r instruction; do
      herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
      while [ -e "$POSSE_TEST_ROOT/pause-fix-commit" ]; do sleep 0.02; done
      printf 'fix change %s\n' "$task_id" > "e2e-fix-$task_id.txt"
      git add "e2e-fix-$task_id.txt"
      git commit -m "fix worker change $task_id" >/dev/null
      while [ ! -e "$POSSE_E2E_FIX_GATE/$task_id" ]; do sleep 0.02; done
      "$POSSE_E2E_POSSE_BIN" publish "Worker fixed $task_id" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
      "$POSSE_E2E_POSSE_BIN" holler done "Worker fixed $task_id" --pr "$pr_url" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
      herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
    done
    ;;
  *)
    while IFS= read -r line; do printf '%s\n' "$line" >> "$POSSE_TEST_ROOT/lead-prompts.log"; done
    ;;
esac
`
	if err := os.WriteFile(claude, []byte(agentScript), 0o700); err != nil {
		t.Fatal(err)
	}
	gh := filepath.Join(binDir, "gh")
	ghScript := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$POSSE_TEST_GH_LOG"
case "$1 $2" in
  "api graphql") cat "$POSSE_TEST_GH_STATE"; if [ -e "$POSSE_TEST_ROOT/gh-exit-nonzero" ]; then exit 1; fi ;;
  "pr list")
    case " $* " in *' --head posse/pr-lifecycle-change '*) branch=posse/pr-lifecycle-change; number=17 ;; *' --head posse/pr-follow-up '*) branch=posse/pr-follow-up; number=17 ;; *' --head posse/pr-create-recovery '*) branch=posse/pr-create-recovery; number=17 ;; *' --head posse/external-merge-change '*) branch=posse/external-merge-change; number=18 ;; *) exit 90 ;; esac
    if grep -q "/pull/$number" "$POSSE_TEST_GH_OPEN_PRS"; then
      if [ -f "$POSSE_TEST_ROOT/gh-list-stale-head" ] && [ -f "$POSSE_TEST_ROOT/gh-list-stale-head-remaining" ]; then
        remaining=$(cat "$POSSE_TEST_ROOT/gh-list-stale-head-remaining")
        if [ "$remaining" -gt 0 ]; then
          head=$(cat "$POSSE_TEST_ROOT/gh-list-stale-head")
          printf '%s\n' "$((remaining - 1))" > "$POSSE_TEST_ROOT/gh-list-stale-head-remaining"
        else
          head=$(git --git-dir="$POSSE_TEST_REMOTE" rev-parse "refs/heads/$branch")
        fi
      else
        head=$(git --git-dir="$POSSE_TEST_REMOTE" rev-parse "refs/heads/$branch")
      fi
      printf '[{"url":"https://github.com/acme/shop/pull/%s","headRefName":"%s","headRefOid":"%s"}]\n' "$number" "$branch" "$head"
    else
      printf '[]\n'
    fi ;;
  "pr create")
    case " $* " in *' --head posse/pr-lifecycle-change '*) branch=posse/pr-lifecycle-change; number=17 ;; *' --head posse/pr-follow-up '*) branch=posse/pr-follow-up; number=17 ;; *' --head posse/pr-create-recovery '*) branch=posse/pr-create-recovery; number=17 ;; *' --head posse/external-merge-change '*) branch=posse/external-merge-change; number=18 ;; *) exit 90 ;; esac
    head=$(git --git-dir="$POSSE_TEST_REMOTE" rev-parse "refs/heads/$branch")
    url="https://github.com/acme/shop/pull/$number"
    printf '[{"url":"%s","headRefName":"%s","headRefOid":"%s"}]\n' "$url" "$branch" "$head" > "$POSSE_TEST_GH_OPEN_PRS"
    printf '%s\n' "$url" ;;
  "pr view")
    case "$3" in */17) branch=$(git --git-dir="$POSSE_TEST_REMOTE" for-each-ref --format='%(refname:short)' 'refs/heads/posse/pr-*' | head -1) ;; */18) branch=posse/external-merge-change ;; *) exit 90 ;; esac
    head=$(git --git-dir="$POSSE_TEST_REMOTE" rev-parse "refs/heads/$branch")
    state=OPEN
    if grep -q '"state":"MERGED"' "$POSSE_TEST_GH_STATE"; then state=MERGED; fi
    printf '{"url":"%s","state":"%s","headRefOid":"%s","headRefName":"%s","baseRefName":"main","headRepository":{"nameWithOwner":"acme/shop"}}\n' "$3" "$state" "$head" "$branch" ;;
  "pr merge") : > "$POSSE_TEST_GH_MERGE"; printf 'Merged\n' ;;
  *) printf 'unexpected fake gh command: %s\n' "$*" >&2; exit 90 ;;
esac
`
	if err := os.WriteFile(gh, []byte(ghScript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "gh-open-prs.json"), []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "gh-state.json"), []byte(`{"data":{"repo1":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	home := filepath.Join(root, "posse")
	configText := "[lead]\nkind = \"claude\"\n\n[defaults]\nlanding_mode = \"pr\"\nauto_unsaddle = \"finished\"\npr_poll = \"1ms\"\n\n[profiles.deep]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"deep\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	remote := filepath.Join(root, "origin.git")
	initRepository(t, repo, remote, env)
	gitTest(t, env, repo, "remote", "set-url", "origin", "https://github.com/acme/shop.git")
	gitTest(t, env, repo, "config", "url.file://"+remote+".insteadOf", "https://github.com/acme/shop.git")
	if err := os.MkdirAll(filepath.Join(root, "signal-gates"), 0o700); err != nil {
		t.Fatal(err)
	}
	client := herdr.NewWithEnv("herdr", env)
	startServer(t, client)
	t.Cleanup(func() { stopLeadFinalizers(t, root, binary) })
	if err := client.CheckProtocol(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "integration", "install", "claude"); err != nil {
		t.Fatalf("install Claude integration in isolated Herdr: %v", err)
	}
	workspace, err := createWorkspace(client, repo)
	if err != nil {
		t.Fatal(err)
	}
	callerEnv := setEnv(env, "HERDR_ENV", "1")
	callerEnv = setEnv(callerEnv, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	callerEnv = setEnv(callerEnv, "HERDR_WORKSPACE_ID", workspace.Workspace.WorkspaceID)
	callerEnv = setEnv(callerEnv, "HERDR_TAB_ID", workspace.RootPane.TabID)
	if liveLead {
		// Run up inside the shell so its finalizer execs the fake Lead in
		// the actual pane. A detached CLI leaves no foreground agent to prompt.
		if _, err := client.Run(context.Background(), "pane", "run", workspace.RootPane.PaneID, "posse up --name shop --yes"); err != nil {
			t.Fatal(err)
		}
	} else {
		// This mode calls up outside the pane and has no live Lead to answer
		// its detached startup finalizer. Stop it now; it otherwise waits 30s
		// after a Herdr restart and outlives cleanup.
		runPosse(t, binary, repo, callerEnv, "up", "--name", "shop", "--yes")
		stopLeadFinalizers(t, root, binary)
	}
	opened, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	var project store.Project
	if !waitForCondition(15*time.Second, func() bool {
		project, err = opened.ProjectByName(context.Background(), "shop")
		return err == nil && project.LeadPaneID != ""
	}) {
		t.Fatalf("Lead did not start: %v", err)
	}
	leadEnv := setEnv(callerEnv, "HERDR_PANE_ID", project.LeadPaneID)
	leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", project.HerdrWorkspaceID)
	if liveLead {
		var processInfo json.RawMessage
		var processInfoErr error
		if !waitForCondition(15*time.Second, func() bool {
			processInfo, processInfoErr = client.Call(context.Background(), "pane.process_info", map[string]any{"pane_id": project.LeadPaneID})
			return processInfoErr == nil && paneForegroundMatches(processInfo, "claude")
		}) {
			t.Fatalf("Lead agent did not reach the pane foreground: %s (%v)", processInfo, processInfoErr)
		}
	}
	if _, err := client.Run(context.Background(), "pane", "report-agent", project.LeadPaneID, "--source", "posse.fake", "--agent", "claude", "--state", "working"); err != nil {
		t.Fatalf("mark isolated Lead busy: %v", err)
	}
	return &prLifecycleFixture{
		root: root, repo: repo, remote: remote, home: home, binary: binary,
		env: env, leadEnv: leadEnv, fixGate: fixGate, ghLog: filepath.Join(root, "gh.log"),
		ghState: filepath.Join(root, "gh-state.json"), ghOpenPRs: filepath.Join(root, "gh-open-prs.json"),
		ghMerge: filepath.Join(root, "gh-merge-called"), db: opened, project: project,
	}
}

func paneForegroundMatches(raw json.RawMessage, want string) bool {
	var response struct {
		ProcessInfo struct {
			ForegroundProcesses []struct {
				Name    string   `json:"name"`
				Argv    []string `json:"argv"`
				Cmdline string   `json:"cmdline"`
			} `json:"foreground_processes"`
		} `json:"process_info"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return false
	}
	for _, process := range response.ProcessInfo.ForegroundProcesses {
		if filepath.Base(process.Name) == want || len(process.Argv) > 0 && filepath.Base(process.Argv[0]) == want {
			return true
		}
		if fields := strings.Fields(process.Cmdline); len(fields) > 0 && filepath.Base(fields[0]) == want {
			return true
		}
	}
	return false
}

// A detached Lead finalizer can outlive a failed fixture startup. Stop only
// finalizers matching this fixture's binary and isolated state paths.
func stopLeadFinalizers(t *testing.T, root, binary string) {
	t.Helper()
	pids, err := leadFinalizerPIDs(root, binary)
	if err != nil {
		t.Errorf("find isolated Lead finalizers: %v", err)
		return
	}
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("stop isolated Lead finalizer %d: %v", pid, err)
		}
	}
	if waitForCondition(2*time.Second, func() bool {
		pids, err = leadFinalizerPIDs(root, binary)
		return err == nil && len(pids) == 0
	}) {
		return
	}
	// A finalizer stuck in an RPC must not survive fixture shutdown.
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("kill isolated Lead finalizer %d: %v", pid, err)
		}
	}
	if !waitForCondition(2*time.Second, func() bool {
		pids, err = leadFinalizerPIDs(root, binary)
		return err == nil && len(pids) == 0
	}) {
		t.Errorf("isolated Lead finalizers remain after shutdown: pids=%v err=%v", pids, err)
	}
}

func leadFinalizerPIDs(root, binary string) ([]int, error) {
	if runtime.GOOS != "linux" {
		return nil, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(cmdline), "\x00")
		if len(args) < 2 || args[0] != binary || args[1] != "_finalize-lead" {
			continue
		}
		environ, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if err != nil {
			continue
		}
		values := "\x00" + string(environ)
		if strings.Contains(values, "\x00POSSE_TEST_ROOT="+root+"\x00") && strings.Contains(values, "\x00XDG_CONFIG_HOME="+filepath.Join(root, "xdg")+"\x00") {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func (fixture *prLifecycleFixture) rideAndComplete(t *testing.T, brief, taskID string) store.Task {
	t.Helper()
	name := "pr-lifecycle-change"
	if taskID == "t2" {
		name = "external-merge-change"
	} else if strings.Contains(brief, "follow-up") {
		name = "pr-follow-up"
	}
	output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "ride", "--brief", brief, "--name", name)
	if !strings.Contains(output, taskID) {
		t.Fatalf("ride did not return %s: %s", taskID, output)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "signal-gates", taskID), []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.waitTaskState(t, taskID, store.StateDone)
	return fixture.mustTask(t, taskID)
}

func (fixture *prLifecycleFixture) rideAndCommitLegacy(t *testing.T, brief, taskID string) {
	t.Helper()
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "ride", "--brief", brief, "--name", "pr-create-recovery")
	if !waitForCondition(30*time.Second, func() bool {
		task := fixture.mustTask(t, taskID)
		if task.WorktreePath == "" {
			return false
		}
		output, err := exec.Command("git", "-C", task.WorktreePath, "rev-list", "--count", task.BaseRef+".."+task.Branch).CombinedOutput()
		return err == nil && strings.TrimSpace(string(output)) == "1"
	}) {
		t.Fatal("legacy Worker did not commit its change")
	}
	task := fixture.mustTask(t, taskID)
	if _, err := fixture.db.AddSignal(context.Background(), task.ID, "done", "Legacy Worker committed "+taskID, nil); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(context.Background(), task.ID, store.StateWorking, store.StateDone, "worker", "Legacy Worker committed "+taskID); err != nil {
		t.Fatal(err)
	}
}

func (fixture *prLifecycleFixture) mustTask(t *testing.T, taskID string) store.Task {
	t.Helper()
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, taskID)
	if err != nil {
		t.Fatalf("load %s: %v", taskID, err)
	}
	return task
}

func (fixture *prLifecycleFixture) waitTaskState(t *testing.T, taskID string, want store.State) {
	t.Helper()
	if !waitForCondition(30*time.Second, func() bool {
		task, err := fixture.db.Task(context.Background(), fixture.project.ID, taskID)
		return err == nil && task.State == want
	}) {
		task, _ := fixture.db.Task(context.Background(), fixture.project.ID, taskID)
		log, _ := os.ReadFile(filepath.Join(fixture.root, "worker-delivery.log"))
		t.Fatalf("Task %s did not reach %s: %#v, Worker delivery: %s", taskID, want, task, log)
	}
}

func (fixture *prLifecycleFixture) writeGraphQL(t *testing.T, alias, state, checks, review, mergeable, mergeCommit, head string) {
	t.Helper()
	contexts := []any{}
	if checks == "FAILURE" {
		contexts = append(contexts, map[string]any{"__typename": "CheckRun", "name": "unit", "status": "COMPLETED", "conclusion": "FAILURE", "detailsUrl": "https://checks.example/unit"})
	}
	pull := map[string]any{
		"url":   "https://github.com/acme/shop/pull/" + map[string]string{"pr1": "17", "pr2": "18"}[alias],
		"state": state, "headRefOid": head, "mergeable": mergeable, "reviewDecision": review,
		"mergeCommit":       map[string]any{"oid": mergeCommit},
		"statusCheckRollup": map[string]any{"state": checks, "contexts": map[string]any{"nodes": contexts}},
	}
	encoded, err := json.Marshal(map[string]any{"data": map[string]any{"repo1": map[string]any{alias: pull}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.ghState, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fixture *prLifecycleFixture) requireNotice(t *testing.T, taskID, kind string) {
	t.Helper()
	task := fixture.mustTask(t, taskID)
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.TaskID == task.ID && notice.Kind == kind {
			return
		}
	}
	t.Fatalf("Task %s has no %s Notice: %#v", taskID, kind, notices)
}

func (fixture *prLifecycleFixture) mergeOnLocalOrigin(t *testing.T, task store.Task, taskID string) string {
	t.Helper()
	checkout := filepath.Join(fixture.root, "external-merge-"+taskID)
	gitTest(t, fixture.env, fixture.root, "clone", "--branch", "main", fixture.remote, checkout)
	gitTest(t, fixture.env, checkout, "config", "user.name", "Posse E2E")
	gitTest(t, fixture.env, checkout, "config", "user.email", "posse-e2e@example.test")
	gitTest(t, fixture.env, checkout, "fetch", "origin", "refs/heads/"+task.Branch+":refs/remotes/origin/"+task.Branch)
	gitTest(t, fixture.env, checkout, "merge", "--squash", "refs/remotes/origin/"+task.Branch)
	gitTest(t, fixture.env, checkout, "commit", "-m", "external merge "+taskID)
	gitTest(t, fixture.env, checkout, "push", "origin", "HEAD:refs/heads/main")
	return strings.TrimSpace(gitTest(t, fixture.env, checkout, "rev-parse", "HEAD"))
}
