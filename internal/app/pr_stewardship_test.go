package app

import (
	"context"
	"errors"
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

// #805: `posse pr inspect` gave up after about a second while the Project
// watch still held the poll claim, and named no cause.
func TestPRInspectWaitsForAProjectPollHeldByAnotherProcess(t *testing.T) {
	f, _ := stewardPollFixture(t)
	defer f.db.Close()
	ctx := context.Background()
	holder, err := f.db.ClaimPRPoll(ctx, f.project.ID, time.Now(), time.Minute, true)
	if err != nil || holder == "" {
		t.Fatalf("watch claim=%q err=%v", holder, err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		released <- f.db.ReleasePRPoll(ctx, f.project.ID, holder)
	}()
	code, output, stderr := f.run("pr", "inspect", "t1")
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("pr inspect: %d %s %s", code, output, stderr)
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
			var operation, subject, outcome string
			if err := fixture.db.QueryRowContext(context.Background(), `SELECT operation,subject,outcome FROM delivery_operations ORDER BY id DESC LIMIT 1`).Scan(&operation, &subject, &outcome); err != nil {
				t.Fatal(err)
			}
			if operation != "executor_policy" || subject != policy || outcome != "succeeded" {
				t.Fatalf("policy history = %q/%q/%q", operation, subject, outcome)
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

func TestPRReleaseKeepsDeliveryAndWatchStateAfterExecutorIsAbsent(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	defer fixture.db.Close()
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR: %d %s %s", code, output, stderr)
	}
	attachPRFixtureSandbox(t, fixture)
	removeFixtureTaskPane(fixture)

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

func TestForgeUnavailableNamesAProjectPollClaimThatIsNeverReleased(t *testing.T) {
	f, _ := stewardPollFixture(t)
	defer f.db.Close()
	ctx := context.Background()
	if holder, err := f.db.ClaimPRPoll(ctx, f.project.ID, time.Now(), time.Minute, true); err != nil || holder == "" {
		t.Fatalf("watch claim=%q err=%v", holder, err)
	}
	previous := stewardObserveBudget
	stewardObserveBudget = 300 * time.Millisecond
	defer func() { stewardObserveBudget = previous }()
	code, output, stderr := f.run("pr", "inspect", "t1")
	combined := output + stderr
	if code == 0 || !strings.Contains(combined, "forge_unavailable") || !strings.Contains(combined, "poll claim") {
		t.Fatalf("pr inspect: %d %s; want forge_unavailable naming the poll claim", code, combined)
	}
}
