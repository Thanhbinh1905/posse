package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func gitlabStewardFixture(t *testing.T) (stewardTarget, string) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$GLAB_FIXTURE/glab.log"
case "$*" in
  *"projects/acme%2Fshop/jobs/55") cat "$GLAB_FIXTURE/job.json" ;;
  *"projects/acme%2Fshop/pipelines/9/jobs?include_retried=true&per_page=100") cat "$GLAB_FIXTURE/jobs.json" ;;
  *"projects/acme%2Fshop/merge_requests/7/notes?per_page=100") cat "$GLAB_FIXTURE/notes.json" ;;
  *) printf 'unexpected glab invocation: %s\n' "$*" >&2; exit 90 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "glab"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GLAB_FIXTURE", root)
	target := stewardTarget{
		forge: repositoryForge{Kind: "gitlab", Host: "git.example.com", Path: "acme/shop", Root: root},
		prURL: "https://git.example.com/acme/shop/-/merge_requests/7",
	}
	return target, root
}

func writeGlabResponse(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGitLabStewardRerunIsBoundToTheHeadAndObservedByNewerJob(t *testing.T) {
	ctx := context.Background()
	target, root := gitlabStewardFixture(t)
	head := strings.Repeat("a", 40)

	writeGlabResponse(t, root, "job.json", `{"id":55,"name":"unit","status":"failed","pipeline":{"id":9,"sha":"`+head+`"}}`)
	baseline, err := forgeRerunBaseline(ctx, target, "55", head)
	if err != nil || baseline != "9" {
		t.Fatalf("baseline = %q, %v", baseline, err)
	}
	var refusal *axi.Error
	if _, err := forgeRerunBaseline(ctx, target, "55", strings.Repeat("b", 40)); !errors.As(err, &refusal) || refusal.Code != "pr_rerun_refused" {
		t.Fatalf("a job from another head was accepted: %v", err)
	}
	writeGlabResponse(t, root, "job.json", `{"id":55,"name":"unit","status":"running","pipeline":{"id":9,"sha":"`+head+`"}}`)
	if _, err := forgeRerunBaseline(ctx, target, "55", head); !errors.As(err, &refusal) || refusal.Code != "pr_rerun_refused" {
		t.Fatalf("a running job was accepted for rerun: %v", err)
	}

	writeGlabResponse(t, root, "jobs.json", `[{"id":55,"name":"unit","status":"failed"},{"id":56,"name":"lint","status":"success"}]`)
	if applied, err := forgeRerunApplied(ctx, target, "55", baseline); err != nil || applied {
		t.Fatalf("rerun without a newer job applied=%v err=%v", applied, err)
	}
	writeGlabResponse(t, root, "jobs.json", `[{"id":55,"name":"unit","status":"failed"},{"id":61,"name":"unit","status":"pending"}]`)
	if applied, err := forgeRerunApplied(ctx, target, "55", baseline); err != nil || !applied {
		t.Fatalf("newer retried job was not observed: applied=%v err=%v", applied, err)
	}
	writeGlabResponse(t, root, "jobs.json", `not json`)
	if _, err := forgeRerunApplied(ctx, target, "55", baseline); err == nil {
		t.Fatal("an unreadable pipeline was treated as an observed outcome")
	}
}

func TestStewardReplyMarkerIsFoundAmongGitLabNotes(t *testing.T) {
	ctx := context.Background()
	target, root := gitlabStewardFixture(t)
	writeGlabResponse(t, root, "notes.json", `[{"body":"Thanks"},{"body":"Rerun requested.\n\n`+stewardOperationMarker(3)+`"}]`)
	if found, err := forgeCommentHasMarker(ctx, target, stewardOperationMarker(3)); err != nil || !found {
		t.Fatalf("posted reply was not found: found=%v err=%v", found, err)
	}
	// Operation 3's marker must not satisfy operation 31 or 4.
	for _, id := range []int64{4, 31} {
		if found, err := forgeCommentHasMarker(ctx, target, stewardOperationMarker(id)); err != nil || found {
			t.Fatalf("operation %d matched another reply: found=%v err=%v", id, found, err)
		}
	}
}

func TestStewardRerunOfAnInProgressRunIsRetryableAndNamesTheState(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	script := "#!/bin/sh\nprintf '%s' '{\"attempt\":1,\"status\":\"in_progress\",\"headSha\":\"" + head + "\"}'\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	target := stewardTarget{
		forge: repositoryForge{Kind: "github", Root: root},
		prURL: "https://github.com/acme/shop/pull/7",
	}
	var refusal *axi.Error
	_, err := forgeRerunBaseline(context.Background(), target, "41", head)
	if !errors.As(err, &refusal) || !refusal.Retryable || !strings.Contains(refusal.Message, "in_progress") {
		t.Fatalf("an in-progress run must give a retryable error naming its state: %v", err)
	}
}

func TestCILogCommandForGitLabNamesTheRepository(t *testing.T) {
	forge := repositoryForge{Kind: "gitlab", Host: "git.example.com", Path: "acme/shop"}
	got := ciLogCommand(forge, "https://git.example.com/acme/shop/-/merge_requests/7", failedCheck{JobID: "55"})
	if want := "glab ci trace 55 --repo https://git.example.com/acme/shop"; got != want {
		t.Fatalf("log command = %q, want %q", got, want)
	}
}

func stewardPollFixture(t *testing.T) (*prMergeFixture, string) {
	t.Helper()
	f := newPRMergeFixture(t, "pr", store.StateWorking)
	ctx := context.Background()
	f.setGHHead(t, f.headSHA)
	if code, output, stderr := f.run("publish", "Worker completion summary"); code != 0 {
		t.Fatalf("publish: %d %s %s", code, output, stderr)
	}
	prURL := "https://github.com/acme/shop/pull/17"
	if err := f.db.UpdateTaskMerge(ctx, f.task.ID, prURL, ""); err != nil {
		t.Fatal(err)
	}
	f.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", f.headSHA)
	return f, prURL
}

func addStewardPollPRs(t *testing.T, f *prMergeFixture) {
	t.Helper()
	ctx := context.Background()
	urls := []string{"https://github.com/acme/shop/pull/18", "https://github.com/acme/shop/pull/19"}
	for i, prURL := range urls {
		seq := i + 2
		if _, err := f.db.CreateTask(ctx, f.project.ID, store.Task{Seq: seq, Type: "change", Title: "Watched change", State: store.StateSpawning, MergeMode: "pr", Branch: fmt.Sprintf("posse/t%d", seq), BaseRef: "main", PRURL: prURL, GatedSHA: f.headSHA, AutonomyMerge: "ask"}); err != nil {
			t.Fatal(err)
		}
	}
	prs := map[string]any{}
	for i, number := range []int{17, 18, 19} {
		prs[fmt.Sprintf("pr%d", i+1)] = map[string]any{
			"url": fmt.Sprintf("https://github.com/acme/shop/pull/%d", number), "state": "OPEN", "headRefOid": f.headSHA,
			"baseRefName": "main", "mergeable": "MERGEABLE", "reviewDecision": "APPROVED",
			"mergeCommit": map[string]any{"oid": ""}, "statusCheckRollup": map[string]any{"state": "SUCCESS", "contexts": map[string]any{"nodes": []any{}}},
		}
	}
	encoded, err := json.Marshal(map[string]any{"data": map[string]any{"repo1": prs}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.ghState, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func startSlowStewardProjectPoll(t *testing.T, f *prMergeFixture) <-chan error {
	t.Helper()
	t.Setenv("POSSE_TEST_GH_DELAY", "1")
	t.Setenv("POSSE_TEST_GH_DELAY_ONCE", filepath.Join(f.root, "slow-poll-once"))
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- f.service.pollProjectPullRequests(context.Background(), f.db, f.project, cfg, true)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		log, err := os.ReadFile(f.ghLog)
		if err == nil && strings.Contains(string(log), "api graphql") {
			if !strings.Contains(string(log), "pr2: pullRequest(number: 18)") || !strings.Contains(string(log), "pr3: pullRequest(number: 19)") {
				t.Fatalf("Project poll did not query all three PRs: %s", log)
			}
			return done
		}
		select {
		case err := <-done:
			t.Fatalf("Project poll finished before querying PRs: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Project poll did not start its GraphQL request")
	return nil
}

func TestPRInspectWaitsForSlowProjectPollOfSeveralPRs(t *testing.T) {
	f, _ := stewardPollFixture(t)
	defer f.db.Close()
	addStewardPollPRs(t, f)
	previousBudget := stewardObserveBudget
	stewardObserveBudget = 300 * time.Millisecond
	defer func() { stewardObserveBudget = previousBudget }()
	pollDone := startSlowStewardProjectPoll(t, f)
	started := time.Now().UnixMilli()
	code, output, stderr := f.run("pr", "inspect", "t1")
	if err := <-pollDone; err != nil {
		t.Fatalf("Project poll: %v", err)
	}
	if code != 0 {
		t.Fatalf("pr inspect: %d %s %s", code, output, stderr)
	}
	observation, err := f.db.LatestPRObservation(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := f.db.PRObservationConfirmedAt(context.Background(), observation.ID)
	if err != nil || confirmed < started {
		t.Fatalf("inspect observation confirmed at %d, command started at %d, err=%v", confirmed, started, err)
	}
}

func TestPRReleaseWaitsForSlowProjectPollOfSeveralPRs(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateDone)
	defer f.db.Close()
	f.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", f.headSHA)
	if code, output, stderr := f.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR: %d %s %s", code, output, stderr)
	}
	attachPRFixtureSandbox(t, f)
	removeFixtureTaskPane(f)
	addStewardPollPRs(t, f)
	previousBudget := stewardObserveBudget
	stewardObserveBudget = 300 * time.Millisecond
	defer func() { stewardObserveBudget = previousBudget }()
	pollDone := startSlowStewardProjectPoll(t, f)
	started := time.Now().UnixMilli()
	code, output, stderr := f.run("pr", "release", "t1", "--policy", "release")
	if err := <-pollDone; err != nil {
		t.Fatalf("Project poll: %v", err)
	}
	if code != 0 {
		t.Fatalf("pr release: %d %s %s", code, output, stderr)
	}
	observation, err := f.db.LatestPRObservation(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := f.db.PRObservationConfirmedAt(context.Background(), observation.ID)
	if err != nil || confirmed < started {
		t.Fatalf("release observation confirmed at %d, command started at %d, err=%v", confirmed, started, err)
	}
}

func TestPRExecutorPoliciesAreRecordedSeparatelyFromMergeAndTeardown(t *testing.T) {
	for _, policy := range []string{"retain", "suspend"} {
		t.Run(policy, func(t *testing.T) {
			fixture := newPRMergeFixture(t, "pr", store.StateDone)
			defer fixture.db.Close()
			fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
			if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
				t.Fatalf("open PR: %d %s %s", code, output, stderr)
			}
			attachPRFixtureSandbox(t, fixture)
			deliveries, err := fixture.db.TaskDeliveries(context.Background(), fixture.task.ID)
			if err != nil || len(deliveries) != 1 {
				t.Fatalf("Task deliveries = %#v err=%v", deliveries, err)
			}
			defaultPolicy, err := fixture.db.DeliveryExecutorPolicy(context.Background(), deliveries[0].ID)
			if err != nil || defaultPolicy != "retain" {
				t.Fatalf("default executor policy = %q err=%v", defaultPolicy, err)
			}

			if code, output, stderr := fixture.run("pr", "release", "t1", "--policy", policy); code != 0 {
				t.Fatalf("set %s policy: %d %s %s", policy, code, output, stderr)
			}
			var operation, subject, outcome, baseline string
			if err := fixture.db.QueryRowContext(context.Background(), `SELECT operation,subject,outcome,baseline FROM delivery_operations ORDER BY id DESC LIMIT 1`).Scan(&operation, &subject, &outcome, &baseline); err != nil {
				t.Fatal(err)
			}
			if operation != "executor_policy" || subject != policy || outcome != "succeeded" || baseline != fmt.Sprint(fixture.task.Launches) {
				t.Fatalf("policy history = %q/%q/%q at launch %q", operation, subject, outcome, baseline)
			}
			sandbox, err := fixture.db.SandboxByTask(context.Background(), fixture.task.ID)
			if err != nil || sandbox.State != "held" {
				t.Fatalf("%s changed Sandbox custody: %#v err=%v", policy, sandbox, err)
			}
			task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
			if err != nil || task.State != store.StateMerging {
				t.Fatalf("%s changed Land state: %#v err=%v", policy, task, err)
			}
			fake := fixture.service.Herdr.(*herdr.Fake)
			cleared := fake.CallCount("pane.clear_agent_authority") > 0
			if (policy == "suspend") != cleared {
				t.Fatalf("%s changed executor state: clear_agent_authority=%t", policy, cleared)
			}
		})
	}
}

func TestPRReleasePreflightFailureDoesNotRecordSucceededExecutorPolicy(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	defer fixture.db.Close()
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR: %d %s %s", code, output, stderr)
	}
	fake := fixture.service.Herdr.(*herdr.Fake)
	for i := range fake.SnapshotValue.Panes {
		if fake.SnapshotValue.Panes[i].PaneID == fixture.task.PaneID {
			fake.SnapshotValue.Panes[i].AgentStatus = "working"
		}
	}

	code, output, stderr := fixture.run("pr", "release", "t1", "--policy", "release")
	combined := output + stderr
	if code == 0 || !strings.Contains(combined, "Worker status is working") {
		t.Fatalf("active Worker release exit=%d output=%s stderr=%s", code, output, stderr)
	}
	var operations int
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM delivery_operations WHERE operation='executor_policy' AND outcome='succeeded'`).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if operations != 0 {
		t.Fatalf("preflight refusal recorded %d succeeded executor policies", operations)
	}
}

func TestRelaunchAfterPublishedPRReleaseReacquiresSandbox(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	attachPRFixtureSandbox(t, fixture)
	repository, err := gitCommonDir(ctx, fixture.worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ClaimBranchAuthor(ctx, repository, fixture.task.Branch, fixture.task.ID, 1, fixture.headSHA, fixture.headSHA, false); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.worktree, "push", "origin", fixture.task.Branch)
	prURL := "https://github.com/acme/shop/pull/17"
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET pr_url=?,profile='worker',launches=1 WHERE id=?`, prURL, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	fixture.task.PRURL = prURL
	fixture.task.Profile = "worker"
	fixture.task.Launches = 1
	if _, err := fixture.db.EnsureDelivery(ctx, fixture.task.ID, "", prURL, "publish"); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(fixture.home, "config.toml")
	configBody, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, append(configBody, []byte("\n[profiles.worker]\nkind = \"claude\"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	removeFixtureTaskPane(fixture)
	fake := fixture.service.Herdr.(*herdr.Fake)
	fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true}}`)
	if code, output, stderr := fixture.run("pr", "release", "t1", "--policy", "release"); code != 0 {
		t.Fatalf("release published PR executor: %d %s %s", code, output, stderr)
	}
	otherID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 2, Type: "change", Title: "reuse released Sandbox", State: store.StateSpawning, MergeMode: "pr", Branch: "posse/t2", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	other := store.Task{ID: otherID, ProjectID: fixture.project.ID, Seq: 2, Type: "change", Branch: "posse/t2", BaseRef: "main"}
	reused, err := acquireSandbox(ctx, fixture.db, fixture.project, other, fixture.home, "warm", nil)
	if err != nil {
		t.Fatalf("reuse released Sandbox: %v", err)
	}
	if reused.Path != fixture.worktree {
		t.Fatalf("Task t2 acquired %s, want released Sandbox %s", reused.Path, fixture.worktree)
	}
	if branch := strings.TrimSpace(gitTest(t, reused.Path, "branch", "--show-current")); branch != "posse/t2" {
		t.Fatalf("reused Sandbox branch = %s, want posse/t2", branch)
	}
	if err := os.WriteFile(filepath.Join(reused.Path, "other-task.txt"), []byte("other Task head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, reused.Path, "add", "other-task.txt")
	gitTest(t, reused.Path, "commit", "-m", "other Task head")
	gitTest(t, fixture.repo, "commit", "--allow-empty", "-m", "advance target before conflict")
	gitTest(t, fixture.repo, "push", "origin", "main")
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "CONFLICTING", "", fixture.headSHA)
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatalf("poll released PR conflict: %v", err)
	}
	var conflictInstructionID int64
	if err := fixture.db.QueryRowContext(ctx, `SELECT message_id FROM validation_fix_rounds WHERE ship_task_id=? AND kind='freshness' ORDER BY seq DESC LIMIT 1`, fixture.task.ID).Scan(&conflictInstructionID); err != nil {
		t.Fatalf("read pr_conflict instruction: %v", err)
	}
	fake.BeforeCall = func(method string) {
		if method == "agent.start" {
			fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{PaneID: "fake:child:p1", WorkspaceID: "fake:child", Label: fixture.task.PaneLabel, Agent: "claude", AgentStatus: "working"})
		}
	}
	if code, output, stderr := fixture.run("relaunch", "t1"); code != 0 {
		t.Fatalf("relaunch released Task after pr_conflict: %d %s %s", code, output, stderr)
	}
	message, err := fixture.db.MessageByID(ctx, conflictInstructionID)
	if err != nil || message.Status != "queued" && message.Status != "delivered" {
		t.Fatalf("pr_conflict instruction was lost during relaunch: %#v err=%v", message, err)
	}
	sandbox, err := fixture.db.SandboxByTask(ctx, fixture.task.ID)
	if err != nil || sandbox.State != "held" || sandbox.TaskID != fixture.task.ID {
		t.Fatalf("relaunch Sandbox = %#v, err=%v", sandbox, err)
	}
	if branch := strings.TrimSpace(gitTest(t, sandbox.Path, "branch", "--show-current")); branch != fixture.task.Branch {
		t.Fatalf("relaunched Sandbox branch = %s, want %s", branch, fixture.task.Branch)
	}
	if head := strings.TrimSpace(gitTest(t, sandbox.Path, "rev-parse", "HEAD")); head != fixture.headSHA {
		t.Fatalf("relaunched Sandbox head = %s, want published head %s", head, fixture.headSHA)
	}
}

func TestRelaunchAfterExecutorReleaseOrSuspendReacquiresBranchAuthor(t *testing.T) {
	for _, variant := range []string{"release", "release-pruned", "suspend"} {
		t.Run(variant, func(t *testing.T) {
			policy := strings.TrimSuffix(variant, "-pruned")
			fixture := newPRMergeFixture(t, "pr", store.StateDone)
			defer fixture.db.Close()
			if variant == "release-pruned" {
				pooled := filepath.Join(fixture.home, "pool", "shop", "sandbox-1")
				if err := os.MkdirAll(filepath.Dir(pooled), 0o700); err != nil {
					t.Fatal(err)
				}
				gitTest(t, fixture.repo, "worktree", "move", fixture.worktree, pooled)
				if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET worktree_path=? WHERE id=?`, pooled, fixture.task.ID); err != nil {
					t.Fatal(err)
				}
				fixture.worktree, fixture.task.WorktreePath = pooled, pooled
			}
			fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
			if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
				t.Fatalf("open PR: %d %s %s", code, output, stderr)
			}
			attachPRFixtureSandbox(t, fixture)
			repository, err := gitCommonDir(context.Background(), fixture.worktree)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.db.ClaimBranchAuthor(context.Background(), repository, fixture.task.Branch, fixture.task.ID, 1, fixture.headSHA, fixture.headSHA, false); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(fixture.home, "config.toml")
			configBody, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, append(configBody, []byte("\n[profiles.worker]\nkind = \"claude\"\n")...), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET profile='worker',launches=1 WHERE id=?`, fixture.task.ID); err != nil {
				t.Fatal(err)
			}
			if policy == "release" {
				removeFixtureTaskPane(fixture)
			}
			fake := fixture.service.Herdr.(*herdr.Fake)
			fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true}}`)
			fake.BeforeCall = func(method string) {
				if method != "agent.start" {
					return
				}
				for i := range fake.SnapshotValue.Panes {
					if fake.SnapshotValue.Panes[i].Label == fixture.task.PaneLabel {
						fake.SnapshotValue.Panes[i].Agent = "claude"
						fake.SnapshotValue.Panes[i].AgentStatus = "working"
						return
					}
				}
				fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{PaneID: "fake:child:p1", WorkspaceID: "fake:child", Label: fixture.task.PaneLabel, Agent: "claude", AgentStatus: "working"})
			}
			if code, output, stderr := fixture.run("pr", "release", "t1", "--policy", policy); code != 0 {
				t.Fatalf("set %s policy: %d %s %s", policy, code, output, stderr)
			}
			if variant == "release-pruned" {
				if err := os.WriteFile(configPath, append(configBody, []byte("\n[profiles.worker]\nkind = \"claude\"\n\n[pool]\nkeep_idle = 0\n")...), 0o600); err != nil {
					t.Fatal(err)
				}
				if code, output, stderr := fixture.run("pool", "prune", "--yes"); code != 0 {
					t.Fatalf("prune released Sandbox: %d %s %s", code, output, stderr)
				}
				if _, err := os.Stat(fixture.worktree); !os.IsNotExist(err) {
					t.Fatalf("former Sandbox still exists after prune: %v", err)
				}
			}
			if code, output, stderr := fixture.run("relaunch", "t1"); code != 0 {
				t.Fatalf("relaunch after %s: %d %s %s", policy, code, output, stderr)
			}
			relaunched, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
			if err != nil || relaunched.Launches != 2 || relaunched.State != store.StateMerging {
				t.Fatalf("relaunched Task = %#v, err=%v", relaunched, err)
			}
			sandbox, err := fixture.db.SandboxByTask(context.Background(), fixture.task.ID)
			if err != nil || sandbox.State != "held" || sandbox.TaskID != fixture.task.ID {
				t.Fatalf("relaunch Sandbox = %#v, err=%v", sandbox, err)
			}
			claim, err := fixture.db.BranchAuthor(context.Background(), repository, fixture.task.Branch)
			if err != nil || claim.Generation != 2 || claim.Attempt != 2 || claim.HeadSHA != fixture.headSHA {
				t.Fatalf("relaunch author claim = %#v, err=%v", claim, err)
			}
		})
	}
}

func TestRelaunchAfterReleaseRefusesWhenTheHeadClaimOrStoppedWriterCannotBeVerified(t *testing.T) {
	for _, refusal := range []string{"head", "branch-advanced", "claim-lock", "writer-active", "foreign-author"} {
		t.Run(refusal, func(t *testing.T) {
			fixture := newPRMergeFixture(t, "pr", store.StateDone)
			defer fixture.db.Close()
			fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
			if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
				t.Fatalf("open PR: %d %s %s", code, output, stderr)
			}
			attachPRFixtureSandbox(t, fixture)
			repository, err := gitCommonDir(context.Background(), fixture.worktree)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.db.ClaimBranchAuthor(context.Background(), repository, fixture.task.Branch, fixture.task.ID, 1, fixture.headSHA, fixture.headSHA, false); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(fixture.home, "config.toml")
			configBody, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, append(configBody, []byte("\n[profiles.worker]\nkind = \"claude\"\n")...), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET profile='worker',launches=1 WHERE id=?`, fixture.task.ID); err != nil {
				t.Fatal(err)
			}
			removeFixtureTaskPane(fixture)
			if code, output, stderr := fixture.run("pr", "release", "t1", "--policy", "release"); code != 0 {
				t.Fatalf("release executor: %d %s %s", code, output, stderr)
			}
			switch refusal {
			case "head":
				if _, err := fixture.db.ExecContext(context.Background(), `UPDATE branch_authorship SET head_sha=? WHERE repository=? AND branch=?`, strings.Repeat("f", 40), repository, fixture.task.Branch); err != nil {
					t.Fatal(err)
				}
			case "branch-advanced":
				tree, err := gitOutput(context.Background(), fixture.repo, "rev-parse", fixture.headSHA+"^{tree}")
				if err != nil {
					t.Fatal(err)
				}
				advancedHead, err := gitOutput(context.Background(), fixture.repo, "commit-tree", tree, "-p", fixture.headSHA, "-m", "unexpected post-release commit")
				if err != nil {
					t.Fatal(err)
				}
				gitTest(t, fixture.repo, "update-ref", "refs/heads/"+fixture.task.Branch, advancedHead)
			case "claim-lock":
				lock, err := acquireTaskBranchAuthorLock(context.Background(), fixture.worktree, fixture.task.Branch)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			case "foreign-author":
				other, err := fixture.db.CreateTask(context.Background(), fixture.project.ID, store.Task{Seq: 2, Type: "change", Title: "competing author", State: store.StateSpawning, Branch: fixture.task.Branch})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.db.ClaimBranchAuthor(context.Background(), repository, fixture.task.Branch, other, 1, fixture.headSHA, fixture.headSHA, true); err != nil {
					t.Fatal(err)
				}
			case "writer-active":
				fake := fixture.service.Herdr.(*herdr.Fake)
				fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{PaneID: fixture.task.PaneID, WorkspaceID: fixture.task.HerdrWorkspaceID, Label: fixture.task.PaneLabel, Agent: "claude", AgentStatus: "working"})
			}
			fake := fixture.service.Herdr.(*herdr.Fake)
			fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true}}`)
			fake.BeforeCall = func(method string) {
				if method == "agent.start" {
					fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{PaneID: "fake:child:p1", WorkspaceID: "fake:child", Label: fixture.task.PaneLabel, Agent: "claude", AgentStatus: "working"})
				}
			}
			code, output, stderr := fixture.run("relaunch", "t1")
			if code == 0 {
				t.Fatalf("relaunch ignored %s refusal: %s %s", refusal, output, stderr)
			}
			if _, err := fixture.db.SandboxByTask(context.Background(), fixture.task.ID); !store.IsNotFound(err) {
				t.Fatalf("refused relaunch acquired Sandbox: err=%v", err)
			}
			unchanged, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
			if err != nil || unchanged.Launches != 1 {
				t.Fatalf("refused relaunch changed Task attempt: %#v err=%v", unchanged, err)
			}
			if got := fixture.service.Herdr.(*herdr.Fake).CallCount("agent.start"); got != 0 {
				t.Fatalf("refused relaunch started %d agents", got)
			}
		})
	}
}

func TestReviewStartRefusesReleasedTaskSandbox(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR: %d %s %s", code, output, stderr)
	}
	attachPRFixtureSandbox(t, fixture)
	if _, err := fixture.db.ClaimBranchAuthor(context.Background(), mustGitCommonDir(t, fixture.worktree), fixture.task.Branch, fixture.task.ID, 1, fixture.headSHA, fixture.headSHA, false); err != nil {
		t.Fatal(err)
	}
	removeFixtureTaskPane(fixture)
	if code, output, stderr := fixture.run("pr", "release", "t1", "--policy", "release"); code != 0 {
		t.Fatalf("release executor: %d %s %s", code, output, stderr)
	}
	otherID, err := fixture.db.CreateTask(context.Background(), fixture.project.ID, store.Task{Seq: 2, Type: "change", Title: "reuse released Sandbox", State: store.StateSpawning, MergeMode: "pr", Branch: "posse/t2", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	other := store.Task{ID: otherID, ProjectID: fixture.project.ID, Seq: 2, Type: "change", Branch: "posse/t2", BaseRef: "main"}
	reused, err := acquireSandbox(context.Background(), fixture.db, fixture.project, other, fixture.home, "warm", nil)
	if err != nil {
		t.Fatalf("reuse released Sandbox: %v", err)
	}
	if reused.Path != fixture.worktree {
		t.Fatalf("Task t2 acquired %s, want released Sandbox %s", reused.Path, fixture.worktree)
	}
	if err := os.WriteFile(filepath.Join(reused.Path, "other-task.txt"), []byte("other Task head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, reused.Path, "add", "other-task.txt")
	gitTest(t, reused.Path, "commit", "-m", "other Task head")
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte("review_provider = \"greenhouse\"\n[defaults]\nmerge_mode = \"pr\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, output, stderr := fixture.run("review", "start", "t1")
	combined := output + stderr
	if code == 0 || !strings.Contains(combined, "review_sandbox_missing") || !strings.Contains(combined, "Sandbox") {
		t.Fatalf("review of released Task exit=%d output=%s stderr=%s", code, output, stderr)
	}
}

func mustGitCommonDir(t *testing.T, path string) string {
	t.Helper()
	repository, err := gitCommonDir(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func TestPRReleaseKeepsDeliveryAndWatchStateAfterExecutorIsAbsent(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	defer fixture.db.Close()
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR: %d %s %s", code, output, stderr)
	}
	attachPRFixtureSandbox(t, fixture)
	removeFixtureTaskPane(fixture)
	if count, err := fixture.db.ActiveWorkerCount(context.Background(), fixture.project.ID); err != nil || count != 1 {
		t.Fatalf("done Change with held Sandbox must count before release: count=%d err=%v", count, err)
	}

	if code, output, stderr := fixture.run("pr", "release", "t1", "--policy", "release"); code != 0 {
		t.Fatalf("release executor: %d %s %s", code, output, stderr)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateMerging {
		t.Fatalf("release changed Land state: %#v err=%v", task, err)
	}
	if policy, err := fixture.db.DeliveryExecutorPolicy(context.Background(), fixture.task.ID); err != nil || policy != "release" {
		t.Fatalf("effective executor policy = %q err=%v", policy, err)
	}
	if sandbox, err := fixture.db.SandboxByTask(context.Background(), fixture.task.ID); !store.IsNotFound(err) {
		t.Fatalf("released Task still owns Sandbox %#v err=%v", sandbox, err)
	}
	if count, err := fixture.db.ActiveWorkerCount(context.Background(), fixture.project.ID); err != nil || count != 0 {
		t.Fatalf("released done Change still counts as a Worker: count=%d err=%v", count, err)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatalf("PR watch after release: %v", err)
	}
	observation, err := fixture.db.LatestPRObservation(context.Background(), fixture.task.ID)
	if err != nil || observation.State != "OPEN" || observation.HeadSHA != fixture.headSHA {
		t.Fatalf("release lost PR watch custody: %#v err=%v", observation, err)
	}
}

func TestPRReleaseResumesAfterSandboxResetBeforeRelease(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	defer fixture.db.Close()
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR: %d %s %s", code, output, stderr)
	}
	attachPRFixtureSandbox(t, fixture)
	removeFixtureTaskPane(fixture)
	ctx := context.Background()
	gitTest(t, fixture.repo, "commit", "--allow-empty", "-m", "advance target")
	gitTest(t, fixture.repo, "push", "origin", "main")
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "CONFLICTING", "", fixture.headSHA)
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatalf("queue initial pr_conflict instruction: %v", err)
	}
	var oldMessageID int64
	if err := fixture.db.QueryRowContext(ctx, `SELECT message_id FROM validation_fix_rounds WHERE ship_task_id=? AND kind='freshness' ORDER BY seq DESC LIMIT 1`, fixture.task.ID).Scan(&oldMessageID); err != nil {
		t.Fatalf("read initial freshness instruction: %v", err)
	}
	if err := fixture.db.StartIntent(ctx, fixture.project.ID, fixture.task.ID, "pr-release", "in_progress:sandbox.release", `{}`, deadIntentProcessID); err != nil {
		t.Fatal(err)
	}
	if err := resetWorktree(ctx, fixture.worktree, projectRepoTarget(fixture.project), "warm", false); err != nil {
		t.Fatal(err)
	}

	if code, output, stderr := fixture.run("pr", "release", "t1", "--policy", "release"); code != 0 {
		t.Fatalf("resume release after Sandbox reset: %d %s %s", code, output, stderr)
	}
	message, err := fixture.db.MessageByID(ctx, oldMessageID)
	if err != nil || message.Status != "withdrawn" {
		t.Fatalf("superseded Posse freshness instruction = %#v err=%v, want withdrawn", message, err)
	}
	var newMessageID int64
	if err := fixture.db.QueryRowContext(ctx, `SELECT message_id FROM validation_fix_rounds WHERE ship_task_id=? AND kind='freshness' ORDER BY seq DESC LIMIT 1`, fixture.task.ID).Scan(&newMessageID); err != nil {
		t.Fatalf("read re-raised freshness instruction: %v", err)
	}
	if newMessageID == oldMessageID {
		t.Fatalf("pr_conflict was not re-raised after release: old message %d is still current", oldMessageID)
	}
	newMessage, err := fixture.db.MessageByID(ctx, newMessageID)
	if err != nil || newMessage.Status != "queued" {
		t.Fatalf("re-raised Posse freshness instruction = %#v err=%v", newMessage, err)
	}
	if sandbox, err := fixture.db.SandboxByTask(ctx, fixture.task.ID); !store.IsNotFound(err) {
		t.Fatalf("resumed release left Task Sandbox owned: %#v err=%v", sandbox, err)
	}
}

func TestPRReleaseKeepsRefreshIntentUntilConflictRequeueSucceeds(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	defer fixture.db.Close()
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR: %d %s %s", code, output, stderr)
	}
	attachPRFixtureSandbox(t, fixture)
	removeFixtureTaskPane(fixture)
	ctx := context.Background()
	gitTest(t, fixture.repo, "commit", "--allow-empty", "-m", "advance target")
	gitTest(t, fixture.repo, "push", "origin", "main")
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "CONFLICTING", "", fixture.headSHA)
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatalf("queue pr_conflict instruction: %v", err)
	}
	var oldMessageID int64
	if err := fixture.db.QueryRowContext(ctx, `SELECT message_id FROM validation_fix_rounds WHERE ship_task_id=? AND kind='freshness' ORDER BY seq DESC LIMIT 1`, fixture.task.ID).Scan(&oldMessageID); err != nil {
		t.Fatalf("read freshness instruction: %v", err)
	}
	messageBeforeRelease, err := fixture.db.MessageByID(ctx, oldMessageID)
	if err != nil || messageBeforeRelease.Status != "queued" {
		t.Fatalf("freshness instruction before release = %#v err=%v", messageBeforeRelease, err)
	}
	if err := fixture.db.StartIntent(ctx, fixture.project.ID, fixture.task.ID, "pr-release", "in_progress:sandbox.release", `{}`, deadIntentProcessID); err != nil {
		t.Fatal(err)
	}
	if err := resetWorktree(ctx, fixture.worktree, projectRepoTarget(fixture.project), "warm", false); err != nil {
		t.Fatal(err)
	}
	withdrawn, blockers, err := fixture.db.WithdrawQueuedFreshnessInstructions(ctx, fixture.task.ID)
	if err != nil || len(withdrawn) != 1 || len(blockers) != 0 {
		t.Fatalf("withdraw freshness before simulated refresh failure: withdrawn=%#v blockers=%#v err=%v", withdrawn, blockers, err)
	}
	target, err := fixture.service.resolveStewardTarget(ctx, fixture.db, fixture.project, "t1", "")
	if err != nil {
		t.Fatal(err)
	}
	var policy store.DeliveryOperation
	beginPolicy := func() error {
		var err error
		policy, err = beginExecutorReleasePolicy(ctx, fixture.db, target, "release", fixture.headSHA)
		return err
	}
	completePolicy := func() error {
		return fixture.db.ResolveDeliveryOperation(ctx, policy.ID, "succeeded", "Executor stopped and Sandbox released")
	}
	if err := fixture.service.releaseExecutor(ctx, fixture.db, target, target.cfg, beginPolicy, completePolicy, func() error { return errors.New("simulated PR refresh failure") }); err == nil || !strings.Contains(err.Error(), "simulated PR refresh failure") {
		t.Fatalf("releaseExecutor error = %v, want simulated PR refresh failure", err)
	}
	var policyOutcome string
	if err := fixture.db.QueryRowContext(ctx, `SELECT outcome FROM delivery_operations WHERE id=?`, policy.ID).Scan(&policyOutcome); err != nil || policyOutcome != "succeeded" {
		t.Fatalf("executor policy after post-release refresh failure = %q, err=%v", policyOutcome, err)
	}
	intent, err := fixture.db.IntentByTask(ctx, fixture.task.ID)
	if err != nil || intent.Command != "pr-release" {
		t.Fatalf("release intent was not retained for retry: %#v err=%v", intent, err)
	}
	oldMessage, err := fixture.db.MessageByID(ctx, oldMessageID)
	if err != nil || oldMessage.Status != "withdrawn" {
		t.Fatalf("superseded freshness instruction = %#v err=%v", oldMessage, err)
	}
	if _, err := fixture.db.SandboxByTask(ctx, fixture.task.ID); !store.IsNotFound(err) {
		t.Fatalf("failed refresh did not leave the Sandbox released: err=%v", err)
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE intents SET process_id=? WHERE task_id=? AND command='pr-release'`, deadIntentProcessID, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := fixture.run("pr", "release", "t1", "--policy", "release"); code != 0 {
		t.Fatalf("retry release after PR refresh recovers: %d %s %s", code, output, stderr)
	}
	var newMessageID int64
	if err := fixture.db.QueryRowContext(ctx, `SELECT message_id FROM validation_fix_rounds WHERE ship_task_id=? AND kind='freshness' ORDER BY seq DESC LIMIT 1`, fixture.task.ID).Scan(&newMessageID); err != nil {
		t.Fatalf("read re-raised freshness instruction: %v", err)
	}
	if newMessageID == oldMessageID {
		t.Fatalf("retry reused withdrawn instruction %d", oldMessageID)
	}
	newMessage, err := fixture.db.MessageByID(ctx, newMessageID)
	if err != nil || newMessage.Status != "queued" {
		t.Fatalf("re-raised freshness instruction = %#v err=%v", newMessage, err)
	}
}

func TestPRReleaseResumeKeepsLeadInstructionsQueued(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	defer fixture.db.Close()
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR: %d %s %s", code, output, stderr)
	}
	attachPRFixtureSandbox(t, fixture)
	removeFixtureTaskPane(fixture)
	ctx := context.Background()
	if err := fixture.db.StartIntent(ctx, fixture.project.ID, fixture.task.ID, "pr-release", "in_progress:sandbox.release", `{}`, deadIntentProcessID); err != nil {
		t.Fatal(err)
	}
	freshness, err := fixture.db.QueueValidationFixInstruction(ctx, fixture.task.ID, "freshness", fixture.headSHA, "freshness:conflict", "Merge origin/main into the Task branch")
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := fixture.db.QueueMessage(ctx, fixture.task.ID, "Lead instruction: preserve this message", true)
	if err != nil {
		t.Fatal(err)
	}

	code, output, stderr := fixture.run("pr", "release", "t1", "--policy", "release")
	combined := output + stderr
	if code == 0 || !strings.Contains(combined, "posse unsend "+fmt.Sprint(messageID)) || !strings.Contains(combined, "posse pr release t1 --policy release") {
		t.Fatalf("blocked release did not name exact recovery commands: exit=%d output=%s", code, combined)
	}
	message, err := fixture.db.MessageByID(ctx, messageID)
	if err != nil || message.Status != "queued" {
		t.Fatalf("Lead instruction changed during release recovery: %#v err=%v", message, err)
	}
	freshnessMessage, err := fixture.db.MessageByID(ctx, freshness.MessageID)
	if err != nil || freshnessMessage.Status != "queued" {
		t.Fatalf("Posse instruction was withdrawn while Lead instruction blocked recovery: %#v err=%v", freshnessMessage, err)
	}
}

func TestPRReleaseResumePreservesOtherAutomaticValidationInstructions(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	defer fixture.db.Close()
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR: %d %s %s", code, output, stderr)
	}
	attachPRFixtureSandbox(t, fixture)
	removeFixtureTaskPane(fixture)
	ctx := context.Background()
	if err := fixture.db.StartIntent(ctx, fixture.project.ID, fixture.task.ID, "pr-release", "in_progress:sandbox.release", `{}`, deadIntentProcessID); err != nil {
		t.Fatal(err)
	}
	gateHead, ciHead, reviewHead := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	gate, err := fixture.db.QueueValidationFixInstruction(ctx, fixture.task.ID, "gate", gateHead, "gate:interrupted", "Fix the Gate failure")
	if err != nil {
		t.Fatal(err)
	}
	coalescedFreshness, err := fixture.db.QueueValidationFixInstruction(ctx, fixture.task.ID, "freshness", gateHead, "freshness:coalesced", "Refresh the target")
	if err != nil || coalescedFreshness.MessageID != gate.MessageID {
		t.Fatalf("coalesced freshness result = %#v err=%v", coalescedFreshness, err)
	}
	ci, err := fixture.db.QueueValidationFixInstruction(ctx, fixture.task.ID, "ci", ciHead, "ci:interrupted", "Fix the CI failure")
	if err != nil {
		t.Fatal(err)
	}
	review, err := fixture.db.QueueValidationFixInstruction(ctx, fixture.task.ID, "review", reviewHead, "review:interrupted", "Fix the Review findings")
	if err != nil {
		t.Fatal(err)
	}
	freshness, err := fixture.db.QueueValidationFixInstruction(ctx, fixture.task.ID, "freshness", fixture.headSHA, "freshness:interrupted", "Merge origin/main into the Task branch")
	if err != nil {
		t.Fatal(err)
	}

	if code, output, stderr := fixture.run("pr", "release", "t1", "--policy", "release"); code != 0 {
		t.Fatalf("automatic validation instructions blocked interrupted release: %d %s %s", code, output, stderr)
	}
	for label, id := range map[string]int64{"gate and coalesced freshness": gate.MessageID, "CI": ci.MessageID, "Review": review.MessageID} {
		message, err := fixture.db.MessageByID(ctx, id)
		if err != nil || message.Status != "queued" {
			t.Errorf("%s instruction = %#v err=%v, want queued", label, message, err)
		}
	}
	oldFreshness, err := fixture.db.MessageByID(ctx, freshness.MessageID)
	if err != nil || oldFreshness.Status != "withdrawn" {
		t.Fatalf("pure freshness instruction = %#v err=%v, want withdrawn", oldFreshness, err)
	}
}

func TestPRReleaseRefusesWhenDeliveryPreservationIsUncertain(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	defer fixture.db.Close()
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR: %d %s %s", code, output, stderr)
	}
	if err := os.WriteFile(fixture.ghState, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, output, stderr := fixture.run("pr", "release", "t1", "--policy", "release")
	combined := output + stderr
	if code == 0 || !strings.Contains(combined, "pr_release_refused") || !strings.Contains(combined, "preservation") {
		t.Fatalf("uncertain release exit=%d output=%s stderr=%s", code, output, stderr)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateMerging {
		t.Fatalf("refused release changed Task state: %#v err=%v", task, err)
	}
	snapshot, err := fixture.service.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, found := findTaskPane(snapshot.Panes, task); !found {
		t.Fatal("refused release changed the executor pane")
	}
	var operations int
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM delivery_operations WHERE operation='executor_policy'`).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if operations != 0 {
		t.Fatalf("refused release recorded %d applied policy operations", operations)
	}
	if cleared := fixture.service.Herdr.(*herdr.Fake).CallCount("pane.clear_agent_authority"); cleared != 0 {
		t.Fatalf("refused release changed executor authority %d times", cleared)
	}
}

func TestPRReleaseRefusesWhenRemoteHeadDiffersFromPRHead(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	defer fixture.db.Close()
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR: %d %s %s", code, output, stderr)
	}
	gitTest(t, fixture.worktree, "commit", "--allow-empty", "-m", "advance Task branch")
	gitTest(t, fixture.worktree, "push", "origin", "posse/t1")

	code, output, stderr := fixture.run("pr", "release", "t1", "--policy", "release")
	combined := output + stderr
	if code == 0 || !strings.Contains(combined, "pr_release_refused") || !strings.Contains(combined, "PR head") {
		t.Fatalf("mismatched-head release exit=%d output=%s stderr=%s", code, output, stderr)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateMerging {
		t.Fatalf("refused release changed Task state: %#v err=%v", task, err)
	}
	if cleared := fixture.service.Herdr.(*herdr.Fake).CallCount("pane.clear_agent_authority"); cleared != 0 {
		t.Fatalf("refused release changed executor authority %d times", cleared)
	}
}

func TestPRReleaseRefusesUnpublishedDetachedSandboxHead(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	defer fixture.db.Close()
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR: %d %s %s", code, output, stderr)
	}
	attachPRFixtureSandbox(t, fixture)
	removeFixtureTaskPane(fixture)
	gitTest(t, fixture.worktree, "checkout", "--detach")
	if err := os.WriteFile(filepath.Join(fixture.worktree, "unpublished.txt"), []byte("keep this commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.worktree, "add", "unpublished.txt")
	gitTest(t, fixture.worktree, "commit", "-m", "unpublished Sandbox output")
	unpublishedHead, err := gitOutput(context.Background(), fixture.worktree, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	code, output, stderr := fixture.run("pr", "release", "t1", "--policy", "release")
	combined := output + stderr
	if code == 0 || !strings.Contains(combined, "pr_release_refused") {
		t.Fatalf("release accepted unpublished detached Sandbox HEAD: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	currentHead, err := gitOutput(context.Background(), fixture.worktree, "rev-parse", "HEAD")
	if err != nil || currentHead != unpublishedHead {
		t.Fatalf("refusal changed Sandbox HEAD from %s to %s: err=%v", unpublishedHead, currentHead, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.worktree, "unpublished.txt")); err != nil {
		t.Fatalf("refusal lost unpublished Sandbox output: %v", err)
	}
	sandbox, err := fixture.db.SandboxByTask(context.Background(), fixture.task.ID)
	if err != nil || sandbox.State != "held" {
		t.Fatalf("refusal released Sandbox custody: %#v err=%v", sandbox, err)
	}
}

func TestForgeUnavailableNamesAProjectPollClaimThatIsNeverReleased(t *testing.T) {
	f, _ := stewardPollFixture(t)
	defer f.db.Close()
	ctx := context.Background()
	if holder, err := f.db.ClaimPRPoll(ctx, f.project.ID, time.Now(), time.Minute, true); err != nil || holder == "" {
		t.Fatalf("watch claim=%q err=%v", holder, err)
	}
	previous, previousWait := stewardObserveBudget, stewardObserveWaitBudget
	stewardObserveBudget, stewardObserveWaitBudget = 300*time.Millisecond, 300*time.Millisecond
	defer func() { stewardObserveBudget, stewardObserveWaitBudget = previous, previousWait }()
	code, output, stderr := f.run("pr", "inspect", "t1")
	combined := output + stderr
	if code == 0 || !strings.Contains(combined, "forge_unavailable") || !strings.Contains(combined, "poll claim") {
		t.Fatalf("pr inspect: %d %s; want forge_unavailable naming the poll claim", code, combined)
	}
}

func TestForgeUnavailableDoesNotBlameProjectPollClaimForForgeTimeout(t *testing.T) {
	f, _ := stewardPollFixture(t)
	defer f.db.Close()
	t.Setenv("POSSE_TEST_GH_DELAY", "1")
	previous := externalCommandTimeout
	externalCommandTimeout = 100 * time.Millisecond
	defer func() { externalCommandTimeout = previous }()
	code, output, stderr := f.run("pr", "inspect", "t1")
	combined := output + stderr
	if code == 0 || !strings.Contains(combined, "forge_unavailable") || strings.Contains(combined, "poll claim") {
		t.Fatalf("pr inspect: %d %s; want forge_unavailable without blaming the Project poll claim", code, combined)
	}
}
