package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// A: a workspace member PR published but not yet gated (repo state "open")
// merges on the forge. The Task must Land.
func TestWorkspaceOpenMemberMergedLands(t *testing.T) {
	f := newWorkspaceLandFixture(t, "[defaults]\nlanding_mode = \"pr\"\nauto_unsaddle = \"never\"\n", []string{"worker", "e2e-tool"}, []string{"worker"})
	bin := filepath.Join(f.root, "bin")
	state := filepath.Join(f.root, "gh-view.json")
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$POSSE_TEST_GH_LOG\"\ncase \"$1 $2\" in\n" +
		"  \"pr view\") cat \"$POSSE_TEST_GH_VIEW\" ;;\n" +
		"  *) exit 90 ;;\nesac\n"
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GH_LOG", filepath.Join(f.root, "gh.log"))
	t.Setenv("POSSE_TEST_GH_VIEW", state)
	worker := filepath.Join(f.workspace, "worker")
	gitTest(t, worker, "remote", "set-url", "origin", "https://github.com/acme/worker.git")
	head := strings.TrimSpace(gitTest(t, filepath.Join(f.task.WorktreePath, "worker"), "rev-parse", "HEAD"))
	url := "https://github.com/acme/worker/pull/7"
	repo := f.repos()["worker"]
	repo.PRURL = url // what `posse publish --repo worker` records; state stays open
	if err := f.db.UpdateTaskRepo(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	view, _ := json.Marshal(map[string]any{"url": url, "state": "MERGED", "headRefOid": head, "mergeable": "MERGEABLE", "reviewDecision": "APPROVED", "mergeCommit": map[string]any{"oid": head}, "statusCheckRollup": []any{}})
	if err := os.WriteFile(state, view, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for tick := 0; tick < 3; tick++ {
		if err := f.service.pollWorkspacePullRequests(ctx, f.db, f.project, f.config(), true); err != nil {
			t.Fatal(err)
		}
	}
	notices, _ := f.db.Notices(ctx, f.project.ID, false)
	kinds := []string{}
	for _, n := range notices {
		kinds = append(kinds, n.Kind)
	}
	t.Logf("state=%s worker=%s notices=%v", f.state(), f.repos()["worker"].State, kinds)
	if f.state() != store.StateLanded {
		t.Fatalf("merged member did not Land the Task: state=%s worker repo=%#v notices=%v", f.state(), f.repos()["worker"], kinds)
	}
}

func insertObservation(t *testing.T, db *store.DB, projectID, taskID int64, url, head, state, merge string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO pr_observations(project_id, task_id, pr_url, head_sha, state, checks, review, mergeable, merge_commit, observed_at) VALUES (?, ?, ?, ?, ?, '{}', '', '', ?, 1)`, projectID, taskID, url, head, state, merge); err != nil {
		t.Fatal(err)
	}
}

// B: upgrade. The previous release recorded a MERGED observation but kept the
// Task working (pr_follow_up_pending). The first reconcile must Land it.
func TestUpgradeWorkingTaskWithRecordedMergeLands(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateWorking)
	defer f.db.Close()
	ctx := context.Background()
	url := "https://github.com/acme/shop/pull/17"
	if err := f.db.UpdateTaskLanding(ctx, f.task.ID, url, ""); err != nil {
		t.Fatal(err)
	}
	insertObservation(t, f.db, f.project.ID, f.task.ID, url, f.headSHA, "MERGED", f.headSHA)
	f.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", f.headSHA, f.headSHA)
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	for tick := 0; tick < 3; tick++ {
		if err := f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, true); err != nil {
			t.Fatal(err)
		}
		if err := f.service.autoTeardownLandedTasks(ctx, f.db, f.project, cfg); err != nil {
			t.Fatal(err)
		}
	}
	task, _ := f.db.Task(ctx, f.project.ID, "t1")
	notices, _ := f.db.Notices(ctx, f.project.ID, false)
	kinds := []string{}
	for _, n := range notices {
		kinds = append(kinds, n.Kind)
	}
	if task.State != store.StateTornDown && task.State != store.StateLanded {
		t.Fatalf("upgraded merged Task silently skipped: state=%s notices=%v", task.State, kinds)
	}
}

// C: a PR closed, then reopened on the forge and merged. The Task must Land.
func TestReopenedPRIsWatchedAgain(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateWorking)
	defer f.db.Close()
	ctx := context.Background()
	url := "https://github.com/acme/shop/pull/17"
	if err := f.db.UpdateTaskLanding(ctx, f.task.ID, url, ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	f.setGraphQLState(t, "CLOSED", "SUCCESS", "APPROVED", "MERGEABLE", "", f.headSHA)
	if err := f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	// The User reopens the PR and merges it.
	f.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", f.headSHA, f.headSHA)
	_ = os.Remove(f.ghLog)
	for tick := 0; tick < 3; tick++ {
		if err := f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	log, _ := os.ReadFile(f.ghLog)
	task, _ := f.db.Task(ctx, f.project.ID, "t1")
	if task.State != store.StateLanded {
		t.Fatalf("reopened+merged PR never Landed: state=%s gh calls after close=%q", task.State, log)
	}
}

// D: the Task's PR changes after the first one closed (new PR published).
// The new PR must be watched and no Decision must claim it closed.
func TestNewPRAfterClosedOneIsWatched(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateWorking)
	defer f.db.Close()
	ctx := context.Background()
	oldURL := "https://github.com/acme/shop/pull/16"
	insertObservation(t, f.db, f.project.ID, f.task.ID, oldURL, f.headSHA, "CLOSED", "")
	url := "https://github.com/acme/shop/pull/17"
	if err := f.db.UpdateTaskLanding(ctx, f.task.ID, url, ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	f.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", f.headSHA, f.headSHA)
	for tick := 0; tick < 2; tick++ {
		if err := f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	decisions, _ := f.db.Decisions(ctx, f.project.ID, true)
	task, _ := f.db.Task(ctx, f.project.ID, "t1")
	for _, d := range decisions {
		t.Logf("decision %s %s: %s", d.Kind, d.Origin, d.Question)
	}
	if task.State != store.StateLanded {
		t.Fatalf("new PR not watched: state=%s decisions=%d", task.State, len(decisions))
	}
}

// E: guard false refusals and bypasses of direct git push.
func TestGuardGitPushSubcommands(t *testing.T) {
	scope, _ := guardFixture(t)
	for _, command := range []string{"git stash push -m wip", "git commit -m push", "git log --grep push", "git checkout -b push"} {
		if refused, _ := guardCommand(command, scope); refused {
			t.Errorf("false refusal: %q", command)
		}
	}
	for _, command := range []string{"c=push; git $c origin HEAD", "git -c alias.p=push p origin HEAD", "git send-pack origin HEAD:refs/heads/main", "git \"$(echo push)\" origin HEAD"} {
		if refused, _ := guardCommand(command, scope); !refused {
			t.Errorf("bypass allowed: %q", command)
		}
	}
}

// F: a concurrent Teardown (another process holds the unsaddle intent) must
// not leave a false "teardown incomplete" reason on a Task that is torn down.
func TestConcurrentTeardownRaisesNoFalseReason(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateWorking)
	defer f.db.Close()
	ctx := context.Background()
	url := "https://github.com/acme/shop/pull/17"
	if err := f.db.UpdateTaskLanding(ctx, f.task.ID, url, ""); err != nil {
		t.Fatal(err)
	}
	f.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", f.headSHA, f.headSHA)
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	fake := f.service.Herdr.(*herdr.Fake)
	fake.SnapshotValue = herdr.Snapshot{Panes: fake.SnapshotValue.Panes[:1]}
	// Another live posse process (this test process) is mid-Teardown.
	if err := f.db.StartIntent(ctx, f.project.ID, f.task.ID, "unsaddle", "panes.close", `{"discard":false}`, os.Getppid()); err != nil {
		t.Fatal(err)
	}
	intent, err := f.db.IntentByTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.autoTeardownLandedTasks(ctx, f.db, f.project, cfg); err != nil {
		t.Fatal(err)
	}
	_ = f.db.FinishIntent(ctx, intent.ID, intent.ProcessID)
	if err := f.service.autoTeardownLandedTasks(ctx, f.db, f.project, cfg); err != nil {
		t.Fatal(err)
	}
	task, _ := f.db.Task(ctx, f.project.ID, "t1")
	notices, _ := f.db.Notices(ctx, f.project.ID, false)
	for _, n := range notices {
		if n.Kind == "unsaddle_incomplete" {
			t.Errorf("state=%s false reason: %s", task.State, n.Summary)
		} else {
			t.Logf("notice %s: %s", n.Kind, n.Summary)
		}
	}
}

// T: a PR closes while its Task is landing with an open land_ready Decision.
// The Decision must not stay answerable with "land".
func TestClosedPRObsoletesLandReadyDecision(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateDone)
	defer f.db.Close()
	if code, output, errOutput := f.run("land", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", f.headSHA)
	if err := f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	if err := f.service.raiseNoticeDecisions(ctx, f.db, f.project); err != nil {
		t.Fatal(err)
	}
	f.setGraphQLState(t, "CLOSED", "SUCCESS", "APPROVED", "MERGEABLE", "", f.headSHA)
	if err := f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	if err := f.service.raiseNoticeDecisions(ctx, f.db, f.project); err != nil {
		t.Fatal(err)
	}
	decisions, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range decisions {
		t.Logf("pending decision %s: %s %v", d.Kind, d.Question, d.Options)
		if d.Kind == "land_ready" {
			t.Errorf("land_ready Decision still pending after the PR closed")
		}
	}
}

// W: a workspace Task Lands from working because its PR member merged. Edits
// the Rider left in another member (here the already-landed local member) must
// be snapshotted or kept, not discarded by Mount release.
func TestWorkspaceTeardownKeepsNonPRMemberWork(t *testing.T) {
	f := newWorkspaceLandFixture(t, "[defaults]\nlanding_mode = \"pr\"\nauto_unsaddle = \"never\"\n", []string{"worker", "e2e-tool"}, []string{"worker", "e2e-tool"})
	bin := filepath.Join(f.root, "bin")
	state := filepath.Join(f.root, "gh-view.json")
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$POSSE_TEST_GH_LOG\"\ncase \"$1 $2\" in\n" +
		"  \"pr list\") printf '[]\\n' ;;\n" +
		"  \"pr create\") printf 'https://github.com/acme/worker/pull/7\\n' ;;\n" +
		"  \"pr view\") cat \"$POSSE_TEST_GH_VIEW\" ;;\n" +
		"  \"pr merge\") printf 'Merged\\n' ;;\n" +
		"  *) exit 90 ;;\nesac\n"
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GH_LOG", filepath.Join(f.root, "gh.log"))
	t.Setenv("POSSE_TEST_GH_VIEW", state)
	worker := filepath.Join(f.workspace, "worker")
	gitTest(t, worker, "remote", "set-url", "origin", "https://github.com/acme/worker.git")
	gitTest(t, worker, "config", "remote.origin.pushurl", filepath.Join(f.root, "remotes", "worker.git"))
	head := strings.TrimSpace(gitTest(t, filepath.Join(f.task.WorktreePath, "worker"), "rev-parse", "HEAD"))
	writeView := func(prState, mergeCommit string) {
		view := map[string]any{"url": "https://github.com/acme/worker/pull/7", "state": prState, "headRefOid": head, "mergeable": "MERGEABLE", "reviewDecision": "APPROVED", "statusCheckRollup": []any{}}
		if mergeCommit != "" {
			view["mergeCommit"] = map[string]any{"oid": mergeCommit}
		}
		encoded, _ := json.Marshal(view)
		if err := os.WriteFile(state, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeView("OPEN", "")
	if code := f.run("land", "t1"); code != 0 {
		t.Fatalf("land: %d\n%s", code, f.out.String())
	}
	if code := f.run("land", "t1", "--merge", "--user-approved", "ship it"); code != 0 {
		t.Fatalf("land --merge: %d\n%s", code, f.out.String())
	}
	ctx := context.Background()
	// The Lead sends a follow-up; the Rider starts editing the local member.
	if err := f.db.Transition(ctx, f.task.ID, store.StateLanding, store.StateWorking, "lead", "follow-up"); err != nil {
		t.Fatal(err)
	}
	unsaved := filepath.Join(f.task.WorktreePath, "e2e-tool", "follow-up-edit.txt")
	if err := os.WriteFile(unsaved, []byte("Rider work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeView("MERGED", head)
	if err := f.service.pollWorkspacePullRequests(ctx, f.db, f.project, f.config(), true); err != nil {
		t.Fatal(err)
	}
	if err := f.service.autoTeardownLandedTasks(ctx, f.db, f.project, f.config()); err != nil {
		t.Fatal(err)
	}
	if f.state() != store.StateTornDown {
		t.Fatalf("state=%s", f.state())
	}
	tool := filepath.Join(f.workspace, "e2e-tool")
	content := gitTest(t, tool, "show", "refs/heads/posse/span-members-leftover:follow-up-edit.txt")
	if content != "Rider work\n" {
		t.Fatalf("uncommitted Member Leftover was not saved: %q", content)
	}
	decisions, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range decisions {
		found = found || d.Kind == "leftover" && strings.Contains(d.Origin, "e2e-tool")
	}
	if !found {
		t.Fatalf("Member Leftover has no Decision: %#v", decisions)
	}
	_ = unsaved
}
