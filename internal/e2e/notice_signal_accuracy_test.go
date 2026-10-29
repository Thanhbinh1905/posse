//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/app"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type workspacePRReadinessFixture struct {
	base        *prLifecycleFixture
	leadEnv     []string
	workspace   string
	memberRoot  string
	worktree    string
	viewPath    string
	blockPath   string
	startedPath string
	releasePath string
	ghLog       string
	project     store.Project
	taskID      int64
	prURL       string
	branch      string
	head        string
}

func newWorkspacePRReadinessFixture(t *testing.T) *workspacePRReadinessFixture {
	t.Helper()
	base := newPRLifecycleFixture(t)
	t.Cleanup(func() { _ = base.db.Close() })
	ctx := context.Background()
	workspace := filepath.Join(base.root, "stack")
	memberRoot := filepath.Join(workspace, "member")
	remote := filepath.Join(base.root, "member-origin.git")
	initRepository(t, memberRoot, remote, base.env)
	gitTest(t, base.env, memberRoot, "remote", "set-url", "origin", "https://github.com/acme/member.git")
	gitTest(t, base.env, memberRoot, "config", "url.file://"+remote+".insteadOf", "https://github.com/acme/member.git")
	members := []store.ProjectRepo{{Name: "member", Path: "member", DefaultBranch: "main", Status: store.RepoActive, OriginHost: "github.com"}}
	project, err := base.db.CreateWorkspaceProject(ctx, "stack", workspace, members)
	if err != nil {
		t.Fatal(err)
	}
	client := herdr.NewWithEnv("herdr", base.env)
	lead, err := createWorkspace(client, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := base.db.SetProjectLead(ctx, project.ID, lead.Workspace.WorkspaceID, lead.RootPane.PaneID, "posse:stack:lead"); err != nil {
		t.Fatal(err)
	}
	leadEnv := setEnv(base.leadEnv, "HERDR_PANE_ID", lead.RootPane.PaneID)
	leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", lead.Workspace.WorkspaceID)
	branch := "posse/t1"
	sandbox := filepath.Join(base.root, "stack-sandbox")
	worktree := filepath.Join(sandbox, "member")
	if err := os.MkdirAll(sandbox, 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, base.env, memberRoot, "worktree", "add", "-q", "-b", branch, worktree, "main")
	if err := os.WriteFile(filepath.Join(worktree, "change.txt"), []byte("workspace change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, base.env, worktree, "add", "change.txt")
	gitTest(t, base.env, worktree, "commit", "-qm", "workspace change")
	gitTest(t, base.env, worktree, "push", "-u", "origin", branch)
	head := strings.TrimSpace(gitTest(t, base.env, worktree, "rev-parse", "HEAD"))
	taskID, err := base.db.CreateTask(ctx, project.ID, store.Task{
		Seq: 1, Type: "change", Title: "Workspace readiness", MergeMode: "pr", Branch: branch,
		BaseRef: "main", WorktreePath: sandbox,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		from, to store.State
		source   string
	}{{store.StateSpawning, store.StateWorking, "cli"}, {store.StateWorking, store.StateDone, "worker"}} {
		if err := base.db.Transition(ctx, taskID, transition.from, transition.to, transition.source, "E2E fixture"); err != nil {
			t.Fatal(err)
		}
	}
	prURL := "https://github.com/acme/member/pull/17"
	if _, err := base.db.ExecContext(ctx, `INSERT INTO task_repos(task_id,repo,worktree_path,base_ref,landing_mode,state,gated_sha,pr_url,updated_at) VALUES(?,?,?,?,?,'open',?,?,?)`, taskID, "member", worktree, "main", "pr", head, prURL, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	fixture := &workspacePRReadinessFixture{
		base: base, leadEnv: leadEnv, workspace: workspace, memberRoot: memberRoot, worktree: worktree,
		viewPath: filepath.Join(base.root, "member-pr-view.json"), blockPath: filepath.Join(base.root, "block-pr-view"),
		startedPath: filepath.Join(base.root, "pr-view-started"), releasePath: filepath.Join(base.root, "release-pr-view"), ghLog: filepath.Join(base.root, "workspace-gh.log"),
		project: project, taskID: taskID, prURL: prURL, branch: branch, head: head,
	}
	gh := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> " + shellQuote(fixture.ghLog) + "\ncase \"$1 $2\" in\n  \"auth status\") exit 0 ;;\n  \"pr view\")\n    if [ -e " + shellQuote(fixture.blockPath) + " ]; then : > " + shellQuote(fixture.startedPath) + "; while [ ! -e " + shellQuote(fixture.releasePath) + " ]; do sleep 0.02; done; fi\n    cat " + shellQuote(fixture.viewPath) + " ;;\n  *) echo \"unexpected fake gh command: $*\" >&2; exit 90 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(base.root, "bin", "gh"), []byte(gh), 0o700); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *workspacePRReadinessFixture) writeView(t *testing.T, head, review string) {
	t.Helper()
	pull := map[string]any{
		"url": f.prURL, "state": "OPEN", "headRefOid": head, "headRefName": f.branch, "baseRefName": "main",
		"mergeable": "MERGEABLE", "reviewDecision": review, "headRepository": map[string]any{"nameWithOwner": "acme/member"},
		"statusCheckRollup": []any{},
	}
	encoded, err := json.Marshal(pull)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.viewPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *workspacePRReadinessFixture) poll(t *testing.T) []byte {
	t.Helper()
	command := exec.Command(f.base.binary, "watch", "--poll-only", "--timeout", "2500")
	command.Dir, command.Env = f.workspace, f.leadEnv
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("posse watch --poll-only: %v\n%s", err, output)
	}
	return output
}

func (f *workspacePRReadinessFixture) mergeReadyCount(t *testing.T) int {
	t.Helper()
	notices, err := f.base.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, notice := range notices {
		if notice.Kind == "merge_ready" && notice.TaskID == f.taskID {
			count++
		}
	}
	return count
}

func TestWorkspaceMergeReadyNoticeIsDeduplicatedPerHeadAndCompletion(t *testing.T) {
	fixture := newWorkspacePRReadinessFixture(t)
	fixture.writeView(t, fixture.head, "APPROVED")
	output := fixture.poll(t)
	if got := fixture.mergeReadyCount(t); got != 1 {
		observation, observationErr := fixture.base.db.LatestMemberPRObservation(context.Background(), fixture.taskID, "member")
		log, _ := os.ReadFile(fixture.ghLog)
		var watchFailure string
		_ = fixture.base.db.QueryRowContext(context.Background(), `SELECT last_error FROM pr_task_watch_failures WHERE task_id=? AND repo='member'`, fixture.taskID).Scan(&watchFailure)
		notices, _ := fixture.base.db.Notices(context.Background(), fixture.project.ID, false)
		t.Fatalf("first ready observation created %d merge_ready Notices, want 1; watch=%s gh=%s observation=%+v err=%v failure=%q notices=%+v", got, output, log, observation, observationErr, watchFailure, notices)
	}

	fixture.writeView(t, fixture.head, "CHANGES_REQUESTED")
	fixture.poll(t)
	fixture.writeView(t, fixture.head, "APPROVED")
	fixture.poll(t)
	if got := fixture.mergeReadyCount(t); got != 1 {
		t.Fatalf("same member head and completion created %d merge_ready Notices after observations changed and returned to ready, want 1", got)
	}

}

type promptAcceptedThenRejectedAdapter struct {
	*herdr.Fake
	accepted []string
}

func (a *promptAcceptedThenRejectedAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	result, err := a.Fake.Call(ctx, method, params)
	if err != nil {
		return result, err
	}
	if method == "agent.prompt" {
		if text, ok := params["text"].(string); ok {
			a.accepted = append(a.accepted, text)
		}
		return nil, &herdr.Error{Code: "agent_not_ready", Message: "Pi reported not ready after applying the prompt"}
	}
	return result, nil
}

func TestUnsendCannotWithdrawPromptAppliedBeforeHerdrError(t *testing.T) {
	fixture := newPRLifecycleFixtureWithManualWatchOwner(t)
	defer fixture.db.Close()
	ctx := context.Background()
	const leadPane, workerPane, workspaceID = "w2:lead", "w2:p1", "w2"
	if err := fixture.db.SetProjectLead(ctx, fixture.project.ID, workspaceID, leadPane, "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	taskID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{
		Seq: 1, Type: "change", Title: "Pi delivery status", MergeMode: "local", PaneID: workerPane,
		PaneLabel: "posse:shop:t1", HerdrWorkspaceID: workspaceID, WorktreePath: fixture.repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{
		Panes: []herdr.Pane{
			{PaneID: leadPane, WorkspaceID: workspaceID, Label: "posse:shop:lead"},
			{PaneID: workerPane, WorkspaceID: workspaceID, Label: "posse:shop:t1", Agent: "pi", AgentStatus: "idle"},
		},
		FocusedPaneID: leadPane,
	}
	fake.Results["agent.explain"] = []byte(`{"explain":{"state":"ready","screen_detection_skipped":false}}`)
	adapter := &promptAcceptedThenRejectedAdapter{Fake: fake}
	service := app.New(fixture.home, adapter)
	t.Chdir(fixture.repo)
	var originalHerdrEnv []string
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(key, "HERDR_") {
			originalHerdrEnv = append(originalHerdrEnv, entry)
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Setenv("HERDR_PANE_ID", leadPane); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("HERDR_WORKSPACE_ID", workspaceID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, entry := range os.Environ() {
			key, _, ok := strings.Cut(entry, "=")
			if ok && strings.HasPrefix(key, "HERDR_") {
				_ = os.Unsetenv(key)
			}
		}
		for _, entry := range originalHerdrEnv {
			key, value, ok := strings.Cut(entry, "=")
			if ok {
				_ = os.Setenv(key, value)
			}
		}
	}()

	var sendOutput bytes.Buffer
	cli := service.CLI()
	cli.Out, cli.ErrOut = &sendOutput, &sendOutput
	sendCode := cli.Run([]string{"send", "t1", "TLS trust instruction"})
	var messageID int64
	if err := fixture.db.QueryRowContext(ctx, `SELECT id FROM messages WHERE task_id=?`, taskID).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	message, err := fixture.db.MessageByID(ctx, messageID)
	if err != nil || (message.Status != "submitting" && message.Status != "queued") || len(adapter.accepted) != 1 || !strings.Contains(adapter.accepted[0], "TLS trust instruction") {
		t.Fatalf("Worker-applied prompt status=%+v accepted=%q err=%v", message, adapter.accepted, err)
	}

	var unsendOutput bytes.Buffer
	cli = service.CLI()
	cli.Out, cli.ErrOut = &unsendOutput, &unsendOutput
	if code := cli.Run([]string{"unsend", strconv.FormatInt(messageID, 10)}); code == 0 || !strings.Contains(unsendOutput.String(), "message_not_queued") || strings.Contains(unsendOutput.String(), "state: withdrawn") {
		t.Fatalf("unsend reported the applied prompt as withdrawn: exit=%d output=%s; original send exit=%d output=%s", code, unsendOutput.String(), sendCode, sendOutput.String())
	}
	if sendCode == 0 || !strings.Contains(sendOutput.String(), "message_delivery_uncertain") {
		t.Fatalf("post-submit Herdr error was not treated as uncertain: exit=%d output=%s", sendCode, sendOutput.String())
	}
	message, err = fixture.db.MessageByID(ctx, messageID)
	if err != nil || message.Status != "submitting" {
		t.Fatalf("unsend changed the applied prompt status to %+v: %v", message, err)
	}
}

func TestUnsendRaceNeverReportsWithdrawnAfterDelivery(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	client := herdr.NewWithEnv("herdr", fixture.env)
	if err := os.MkdirAll(filepath.Join(fixture.root, "pi", "extensions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "integration", "install", "pi"); err != nil {
		t.Fatalf("install Pi integration: %v", err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(fixture.binary), "pi"), []byte(fakeBusyHarness), 0o700); err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.deep.kind", "pi")
	brief := filepath.Join(fixture.root, "unsend-race.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Unsend delivery race\ndone_when: racing withdrawal does not lie\n---\nWait for a follow-up.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", brief, "--name", "unsend-race")
	task := fixture.mustTask(t, "t1")
	if !waitForCondition(15*time.Second, func() bool {
		snapshot, err := client.Snapshot(context.Background())
		if err != nil {
			return false
		}
		for _, pane := range snapshot.Panes {
			if pane.PaneID == task.PaneID {
				return pane.Agent == "pi" && pane.AgentStatus == "working" && !pane.Focused
			}
		}
		return false
	}) {
		t.Fatal("Pi Worker did not become ready before the withdrawal race")
	}
	messageID, err := fixture.db.QueueMessage(context.Background(), task.ID, "Race withdrawal against delivery", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "pane", "report-agent", task.PaneID, "--source", "posse.e2e", "--agent", "pi", "--state", "idle"); err != nil {
		t.Fatalf("start delivery before withdrawal: %v", err)
	}
	if !waitForCondition(10*time.Second, func() bool {
		message, err := fixture.db.MessageByID(context.Background(), messageID)
		return err == nil && (message.Status == "submitting" || message.Status == "delivered")
	}) {
		message, _ := fixture.db.MessageByID(context.Background(), messageID)
		t.Fatalf("delivery did not reach its irreversible boundary: %#v", message)
	}
	unsend := exec.Command(fixture.binary, "unsend", strconv.FormatInt(messageID, 10))
	unsend.Dir, unsend.Env = fixture.repo, fixture.leadEnv
	output, unsendErr := unsend.CombinedOutput()
	if unsendErr == nil || !strings.Contains(string(output), "message_not_queued") || strings.Contains(string(output), "state: withdrawn") {
		t.Fatalf("unsend did not refuse a message whose delivery had started: err=%v output=%s", unsendErr, output)
	}
	if !waitForCondition(10*time.Second, func() bool {
		message, err := fixture.db.MessageByID(context.Background(), messageID)
		return err == nil && message.Status == "delivered"
	}) {
		message, _ := fixture.db.MessageByID(context.Background(), messageID)
		t.Fatalf("in-flight message did not reach delivered status: %#v", message)
	}
	message, err := fixture.db.MessageByID(context.Background(), messageID)
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.ReadFile(filepath.Join(fixture.root, "fake-agent-input.log"))
	if err != nil {
		t.Fatal(err)
	}
	if message.Status != "delivered" || !strings.Contains(string(input), "Race withdrawal against delivery") {
		t.Fatalf("Worker applied a message that Posse did not mark delivered: status=%s output=%s input=%s", message.Status, output, input)
	}
}

func TestQueuedDeliveryAfterDoneDoesNotLookLikeExternalResume(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "config", "set", "defaults.merge_mode", "local"); !strings.Contains(output, "local") {
		t.Fatalf("configure local Merge Mode: %s", output)
	}
	agent := `#!/bin/sh
set -eu
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
case "$PWD/" in
  "$POSSE_E2E_WORKTREES/"*)
    IFS= read -r prompt || exit 0
    launch_path=${prompt#Read }
    launch_path=${launch_path% and follow it.}
    task_id=$(basename "$(dirname "$launch_path")")
    printf 'initial change\n' > first-change.txt
    git add first-change.txt
    git commit -m 'initial change' >/dev/null
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
    until "$POSSE_E2E_POSSE_BIN" signal done 'Initial Worker completion' >> "$POSSE_TEST_ROOT/queued-resume.log" 2>&1; do sleep 0.02; done
    : > "$POSSE_TEST_ROOT/first-done-$task_id"
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
    turn_token=
    while IFS= read -r line; do
      printf '%s\n' "$line" >> "$POSSE_TEST_ROOT/queued-resume.log"
      case "$line" in *'| turn '*) turn_token=$(printf '%s\n' "$line" | sed -n 's/.*| turn \([^]]*\)].*/\1/p') ;; esac
      case "$line" in *'Make one follow-up change after completion.'*) break ;; esac
    done
    if "$POSSE_E2E_POSSE_BIN" signal done 'Delayed completion from the prior Worker turn' >> "$POSSE_TEST_ROOT/queued-resume.log" 2>&1; then
      : > "$POSSE_TEST_ROOT/stale-done-accepted-$task_id"
    fi
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
    : > "$POSSE_TEST_ROOT/follow-up-working-$task_id"
    while [ ! -e "$POSSE_E2E_SIGNAL_GATE/continue-current-done-$task_id" ]; do sleep 0.02; done
    printf 'queued follow-up\n' > follow-up.txt
    git add follow-up.txt
    git commit -m 'queued follow-up' >/dev/null
    if [ -n "$turn_token" ]; then done_turn_args="--turn $turn_token"; else done_turn_args=; fi
    until "$POSSE_E2E_POSSE_BIN" signal done 'Queued follow-up completion' $done_turn_args >> "$POSSE_TEST_ROOT/queued-resume.log" 2>&1; do sleep 0.02; done
    : > "$POSSE_TEST_ROOT/follow-up-done-$task_id"
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
    while IFS= read -r line; do :; done
    ;;
  *) while IFS= read -r line; do :; done ;;
esac
`
	if err := os.WriteFile(filepath.Join(fixture.root, "bin", "claude"), []byte(agent), 0o700); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(fixture.root, "queued-resume.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Queued follow-up after done\ndone_when: queued follow-up is committed\n---\nCreate a committed change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", brief, "--name", "queued-resume")
	fixture.waitTaskState(t, "t1", store.StateDone)
	if !waitForCondition(15*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(fixture.root, "first-done-t1"))
		return err == nil
	}) {
		t.Fatal("Worker did not complete its first turn")
	}
	watch := exec.Command(fixture.binary, "watch", "--poll-only", "--timeout", "30000")
	watch.Dir, watch.Env = fixture.repo, fixture.leadEnv
	if err := watch.Start(); err != nil {
		t.Fatalf("start Project Watch: %v", err)
	}
	defer func() {
		_ = watch.Process.Kill()
		_ = watch.Wait()
	}()
	if !waitForCondition(10*time.Second, func() bool {
		var turnEnded int
		err := fixture.db.QueryRowContext(context.Background(), `SELECT turn_ended FROM completed_rider_busy WHERE task_id=?`, fixture.mustTask(t, "t1").ID).Scan(&turnEnded)
		return err == nil && turnEnded == 1
	}) {
		t.Fatal("Project Watch did not observe the completed Worker turn end")
	}
	if task := fixture.mustTask(t, "t1"); task.State != store.StateDone {
		t.Fatalf("idle Worker status reopened a completed Task: %s", task.State)
	}
	initialNotices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range initialNotices {
		if notice.TaskID == fixture.mustTask(t, "t1").ID && notice.Kind == "worker_resumed" {
			t.Fatalf("idle Worker after done was reported as an external resume: %#v", notice)
		}
	}
	output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Make one follow-up change after completion.", "--queue")
	if !strings.Contains(output, "state: queued") && !strings.Contains(output, "state: delivered") {
		t.Fatalf("queue follow-up after done: %s", output)
	}
	var messageID int64
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT id FROM messages WHERE task_id=? ORDER BY id DESC LIMIT 1`, fixture.mustTask(t, "t1").ID).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(20*time.Second, func() bool {
		current, err := fixture.db.MessageByID(context.Background(), messageID)
		return err == nil && current.Status == "delivered"
	}) {
		current, _ := fixture.db.MessageByID(context.Background(), messageID)
		log, _ := os.ReadFile(filepath.Join(fixture.root, "queued-resume.log"))
		t.Fatalf("queued follow-up was not delivered: message=%+v log=%s", current, log)
	}
	workingMarker := filepath.Join(fixture.root, "follow-up-working-t1")
	if !waitForCondition(15*time.Second, func() bool {
		_, err := os.Stat(workingMarker)
		return err == nil
	}) {
		log, _ := os.ReadFile(filepath.Join(fixture.root, "queued-resume.log"))
		t.Fatalf("Worker did not reach the queued instruction turn: %s", log)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "stale-done-accepted-t1")); err == nil {
		t.Fatal("a done Signal from the prior turn was accepted after queued delivery")
	}
	time.Sleep(3 * time.Second) // Allow the running Watch to observe the active instruction turn.
	if task := fixture.mustTask(t, "t1"); task.State != store.StateWorking {
		t.Fatalf("stale completion changed the queued instruction Task state: %s", task.State)
	}
	activeNotices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range activeNotices {
		if notice.TaskID == fixture.mustTask(t, "t1").ID && notice.Kind == "worker_resumed" {
			t.Fatalf("Posse-delivered instruction was misclassified as an outside Worker resume: %#v", notice)
		}
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "signal-gates", "continue-current-done-t1"), []byte("continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(20*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(fixture.root, "follow-up-done-t1"))
		return err == nil
	}) {
		log, _ := os.ReadFile(filepath.Join(fixture.root, "queued-resume.log"))
		t.Fatalf("Worker did not finish the queued follow-up: %s", log)
	}
	if task := fixture.mustTask(t, "t1"); task.State != store.StateDone {
		t.Fatalf("queued follow-up completion state = %s, want done", task.State)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.TaskID == fixture.mustTask(t, "t1").ID && notice.Kind == "worker_resumed" {
			t.Fatalf("Posse-delivered instruction was reported as an external Worker resume: %#v", notice)
		}
	}
}

func TestSendContinuesAfterBlockedForgeTimeout(t *testing.T) {
	fixture := newWorkspacePRReadinessFixture(t)
	fixture.writeView(t, fixture.head, "APPROVED")
	if err := os.WriteFile(fixture.blockPath, []byte("block\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(fixture.base.binary, "send", "t1", "Deliver without queueing")
	command.Dir, command.Env = fixture.workspace, fixture.leadEnv
	outputCh := make(chan struct {
		output []byte
		err    error
	}, 1)
	go func() {
		output, err := command.CombinedOutput()
		outputCh <- struct {
			output []byte
			err    error
		}{output: output, err: err}
	}()
	started := time.Now()
	select {
	case result := <-outputCh:
		elapsed := time.Since(started)
		_ = os.WriteFile(fixture.releasePath, []byte("release\n"), 0o600)
		if result.err != nil || !strings.Contains(string(result.output), "state: queued") {
			t.Fatalf("ordinary send did not queue without waiting for Forge: err=%v output=%s", result.err, result.output)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("ordinary send took %s after the blocked Forge exceeded its refresh budget", elapsed)
		}
	case <-time.After(5 * time.Second):
		_ = os.WriteFile(fixture.releasePath, []byte("release\n"), 0o600)
		result := <-outputCh
		t.Fatalf("ordinary send waited for the blocked Forge response: err=%v output=%s", result.err, result.output)
	}
	if _, err := os.Stat(fixture.startedPath); err != nil {
		t.Fatalf("ordinary send did not attempt the bounded Forge refresh: stat err=%v", err)
	}
	var messages int
	if err := fixture.base.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE task_id=?`, fixture.taskID).Scan(&messages); err != nil || messages != 1 {
		t.Fatalf("send did not queue its message: count=%d err=%v", messages, err)
	}
}

func TestQueuedSendIsBoundedAndShowsQueueAge(t *testing.T) {
	fixture := newWorkspacePRReadinessFixture(t)
	fixture.writeView(t, fixture.head, "APPROVED")
	if err := os.WriteFile(fixture.blockPath, []byte("block\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(fixture.base.binary, "send", "t1", "Queue without waiting for Forge", "--queue")
	command.Dir, command.Env = fixture.workspace, fixture.leadEnv
	outputCh := make(chan struct {
		output []byte
		err    error
	}, 1)
	go func() {
		output, err := command.CombinedOutput()
		outputCh <- struct {
			output []byte
			err    error
		}{output: output, err: err}
	}()
	var result struct {
		output []byte
		err    error
	}
	select {
	case result = <-outputCh:
	case <-time.After(time.Second):
		if err := os.WriteFile(fixture.releasePath, []byte("release\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		result = <-outputCh
		t.Fatalf("queued send waited for the blocked Forge response: err=%v output=%s", result.err, result.output)
	}
	if err := os.WriteFile(fixture.releasePath, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result.err != nil || !strings.Contains(string(result.output), "state: queued") {
		t.Fatalf("queued send did not return its queued result: err=%v output=%s", result.err, result.output)
	}
	messages, err := fixture.base.db.UndeliveredTaskMessages(context.Background(), fixture.taskID)
	if err != nil || len(messages) != 1 || messages[0].Status != "queued" {
		t.Fatalf("queued message = %+v, %v", messages, err)
	}
	show := exec.Command(fixture.base.binary, "show", "t1")
	show.Dir, show.Env = fixture.workspace, fixture.leadEnv
	showOutput, err := show.CombinedOutput()
	if err != nil || !strings.Contains(string(showOutput), "queued_for") {
		t.Fatalf("posse show omitted queue age: err=%v output=%s", err, showOutput)
	}
}

func TestWorkspacePRReadinessDoesNotOutliveDoneState(t *testing.T) {
	fixture := newWorkspacePRReadinessFixture(t)
	fixture.writeView(t, fixture.head, "APPROVED")
	if err := os.WriteFile(fixture.blockPath, []byte("block\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(fixture.base.binary, "watch", "--poll-only", "--timeout", "30000")
	command.Dir, command.Env = fixture.workspace, fixture.leadEnv
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	if !waitForCondition(10*time.Second, func() bool {
		_, err := os.Stat(fixture.startedPath)
		return err == nil
	}) {
		t.Fatal("workspace PR poll did not reach the blocked forge response")
	}
	if err := fixture.base.db.Transition(context.Background(), fixture.taskID, store.StateDone, store.StateWorking, "lead", "Lead sent a new instruction"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.releasePath, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(10*time.Second, func() bool {
		_, err := fixture.base.db.LatestMemberPRObservation(context.Background(), fixture.taskID, "member")
		return err == nil
	}) {
		t.Fatal("workspace PR poll did not store its result after release")
	}
	if got := fixture.mergeReadyCount(t); got != 0 {
		t.Fatalf("stale ready observation raised %d merge_ready Notices after Task reopened, want 0", got)
	}
}

func TestDoneSignalRefusesHeadWithFailedRequiredChecks(t *testing.T) {
	fixture := newPRLifecycleFixtureWithManualWatchOwner(t)
	defer fixture.db.Close()

	ctx := context.Background()
	branch := "posse/t1"
	worktree := filepath.Join(fixture.root, "posse", "pool", "stale-done")
	gitTest(t, fixture.env, fixture.repo, "worktree", "add", "-b", branch, worktree, "main")
	if err := os.WriteFile(filepath.Join(worktree, "change.txt"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.env, worktree, "add", "change.txt")
	gitTest(t, fixture.env, worktree, "commit", "-m", "change for stale completion")
	head := strings.TrimSpace(gitTest(t, fixture.env, worktree, "rev-parse", "HEAD"))
	gitTest(t, fixture.env, worktree, "push", "origin", branch)

	taskID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{
		Seq: 1, Type: "change", Title: "Stale completion", MergeMode: "pr", Branch: branch,
		BaseRef: "main", WorktreePath: worktree,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		from, to store.State
		source   string
	}{{store.StateSpawning, store.StateWorking, "cli"}} {
		if err := fixture.db.Transition(ctx, taskID, transition.from, transition.to, transition.source, "E2E fixture"); err != nil {
			t.Fatal(err)
		}
	}
	prURL := "https://github.com/acme/shop/pull/17"
	if _, err := fixture.db.NextTaskLaunchWithIdentity(ctx, taskID, "deep", "", false); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.UpdateTaskMerge(ctx, taskID, prURL, ""); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.SetTaskGatedSHA(ctx, taskID, head); err != nil {
		t.Fatal(err)
	}
	openPRs, err := json.Marshal([]map[string]string{{"url": prURL, "headRefName": branch, "headRefOid": head}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.ghOpenPRs, openPRs, 0o600); err != nil {
		t.Fatal(err)
	}
	observation := store.PRObservation{
		ProjectID: fixture.project.ID, TaskID: taskID, PRURL: prURL, HeadSHA: head, State: "OPEN",
		Checks: `{"state":"FAILURE","failures":[{"name":"unit","url":"https://checks.example/unit"}]}`,
		Review: "APPROVED", Mergeable: "MERGEABLE", ObservedAt: time.Now().UnixMilli(),
	}
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.RecordPRObservation(ctx, observation, store.PRObservationEffect{}, task); err != nil {
		t.Fatal(err)
	}

	workerEnv := setEnv(fixture.leadEnv, "HERDR_PANE_ID", "")
	worker := exec.Command(fixture.binary, "signal", "done", "Stale completion", "--pr", prURL, "--turn", "launch:1")
	worker.Dir, worker.Env = worktree, workerEnv
	output, err := worker.CombinedOutput()
	if err == nil {
		t.Fatalf("done Signal accepted a head with failed required checks: %s", output)
	}
	if !strings.Contains(strings.ToLower(string(output)), "check") {
		t.Fatalf("done Signal failed for an unrelated reason: %s", output)
	}
	signals, err := fixture.db.TaskSignals(ctx, taskID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, signal := range signals {
		if signal.Verb == "done" {
			t.Fatalf("refused done Signal was recorded: %#v; output=%s", signal, output)
		}
	}
	current, err := fixture.db.TaskByID(ctx, fixture.project.ID, taskID)
	if err != nil || current.State != store.StateWorking {
		t.Fatalf("failed-check completion changed Task state: %#v, %v; output=%s", current, err, output)
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE pr_observations SET checks=? WHERE task_id=? AND head_sha=?`, `{"state":"SUCCESS","failures":[]}`, taskID, head); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.NextTaskLaunchWithIdentity(ctx, taskID, "deep", "", false); err != nil {
		t.Fatal(err)
	}
	staleInstance := exec.Command(fixture.binary, "signal", "done", "Old instance completion", "--pr", prURL)
	staleInstance.Dir, staleInstance.Env = worktree, workerEnv
	if staleOutput, err := staleInstance.CombinedOutput(); err == nil || !strings.Contains(string(staleOutput), "stale_worker_turn") {
		t.Fatalf("old Worker instance was not rejected: err=%v output=%s", err, staleOutput)
	}
	messageID, err := fixture.db.QueueMessage(ctx, taskID, "Instruction for the relaunched Worker", false)
	if err != nil {
		t.Fatal(err)
	}
	claimToken := "t331-delivery"
	if claimed, err := fixture.db.ClaimMessage(ctx, messageID, claimToken, time.Now().UnixMilli(), time.Now().Add(-2*time.Minute).UnixMilli()); err != nil || !claimed {
		t.Fatalf("claim instruction for the new turn: claimed=%v err=%v", claimed, err)
	}
	if err := fixture.db.MarkMessageSubmitting(ctx, messageID, claimToken); err != nil {
		t.Fatal(err)
	}
	staleTurn := exec.Command(fixture.binary, "signal", "done", "Prior turn completion", "--pr", prURL)
	staleTurn.Dir, staleTurn.Env = worktree, workerEnv
	if staleOutput, err := staleTurn.CombinedOutput(); err == nil || !strings.Contains(string(staleOutput), "stale_worker_turn") {
		t.Fatalf("done Signal predating the delivered instruction was not rejected: err=%v output=%s", err, staleOutput)
	}
	currentTurn := exec.Command(fixture.binary, "signal", "done", "Current turn completion", "--pr", prURL, "--turn", "message:"+strconv.FormatInt(messageID, 10))
	currentTurn.Dir, currentTurn.Env = worktree, workerEnv
	if currentOutput, err := currentTurn.CombinedOutput(); err != nil || !strings.Contains(string(currentOutput), "state: done") {
		t.Fatalf("current turn was not accepted after the checks passed: err=%v output=%s", err, currentOutput)
	}

	removed := exec.Command(fixture.binary, "holler", "done", "Old vocabulary check")
	removed.Dir, removed.Env = fixture.repo, fixture.leadEnv
	replacement, err := removed.CombinedOutput()
	if err == nil || !strings.Contains(string(replacement), "posse signal") {
		t.Fatalf("removed holler command did not name its replacement: err=%v output=%s", err, replacement)
	}
}

func TestDoneSignalRefusesTurnFromBeforeDeliveredInstruction(t *testing.T) {
	fixture := newPRLifecycleFixtureWithManualWatchOwner(t)
	defer fixture.db.Close()
	ctx := context.Background()
	worktree := filepath.Join(fixture.root, "posse", "pool", "stale-instruction-turn")
	gitTest(t, fixture.env, fixture.repo, "worktree", "add", "-b", "posse/t1", worktree, "main")
	if err := os.WriteFile(filepath.Join(worktree, "report.md"), []byte("Current Research report.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	taskID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{
		Seq: 1, Type: "research", Title: "Instruction turn completion", MergeMode: "local", Branch: "posse/t1",
		BaseRef: "main", WorktreePath: worktree,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "E2E fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.NextTaskLaunchWithIdentity(ctx, taskID, "deep", "", false); err != nil {
		t.Fatal(err)
	}
	messageID, err := fixture.db.QueueMessage(ctx, taskID, "Update the Report before completion", false)
	if err != nil {
		t.Fatal(err)
	}
	const deliveryToken = "t331-instruction-delivery"
	if claimed, err := fixture.db.ClaimMessage(ctx, messageID, deliveryToken, time.Now().UnixMilli(), time.Now().Add(-2*time.Minute).UnixMilli()); err != nil || !claimed {
		t.Fatalf("claim instruction: claimed=%v err=%v", claimed, err)
	}
	if err := fixture.db.MarkMessageSubmitting(ctx, messageID, deliveryToken); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.MarkMessageDelivered(ctx, messageID, deliveryToken, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	workerEnv := setEnv(fixture.leadEnv, "HERDR_PANE_ID", "")
	worker := exec.Command(fixture.binary, "signal", "done", "Delayed completion from before the instruction", "--report", "report.md")
	worker.Dir, worker.Env = worktree, workerEnv
	output, err := worker.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "stale_worker_turn") {
		t.Fatalf("pre-instruction completion was not refused: err=%v output=%s", err, output)
	}
	currentTurn, turnErr := fixture.db.TaskTurnToken(ctx, taskID)
	if turnErr != nil || currentTurn == "" || !strings.Contains(string(output), "Retry the same Signal with `--turn "+currentTurn+"`") {
		t.Fatalf("refusal does not print the current turn token %q: err=%v output=%s", currentTurn, turnErr, output)
	}
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, taskID)
	if err != nil || task.State != store.StateWorking {
		t.Fatalf("stale completion changed Task state: task=%+v err=%v", task, err)
	}
}

func TestDoneSignalRefusesStaleWorkerInstanceAfterRelaunch(t *testing.T) {
	fixture := newPRLifecycleFixtureWithManualWatchOwner(t)
	defer fixture.db.Close()
	ctx := context.Background()
	worktree := filepath.Join(fixture.root, "posse", "pool", "stale-instance")
	gitTest(t, fixture.env, fixture.repo, "worktree", "add", "-b", "posse/t1", worktree, "main")
	if err := os.WriteFile(filepath.Join(worktree, "report.md"), []byte("Current Research report.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	taskID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{
		Seq: 1, Type: "research", Title: "Relaunched Worker completion", MergeMode: "local", Branch: "posse/t1",
		BaseRef: "main", WorktreePath: worktree,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "E2E fixture"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := fixture.db.NextTaskLaunchWithIdentity(ctx, taskID, "deep", "", false); err != nil {
			t.Fatal(err)
		}
	}
	workerEnv := setEnv(fixture.leadEnv, "HERDR_PANE_ID", "")
	worker := exec.Command(fixture.binary, "signal", "done", "Delayed completion from the prior Worker instance", "--report", "report.md")
	worker.Dir, worker.Env = worktree, workerEnv
	output, err := worker.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "stale_worker_turn") {
		t.Fatalf("prior Worker instance completion was not refused: err=%v output=%s", err, output)
	}
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, taskID)
	if err != nil || task.State != store.StateWorking {
		t.Fatalf("stale completion changed Task state: task=%+v err=%v", task, err)
	}
	signals, err := fixture.db.TaskSignals(ctx, taskID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, signal := range signals {
		if signal.Verb == "done" {
			t.Fatalf("stale completion was recorded: signal=%+v", signal)
		}
	}
}

func TestAckedClaudeWakeResolvesReceiptInSameTurn(t *testing.T) {
	fixture := newAcknowledgedNoticeFixture(t)
	defer fixture.db.Close()

	code, output := fixture.cli(t, "watch", "--json", "--handoff", "--destination", fixture.destination, "--timeout", "2000")
	if code != 0 {
		t.Fatalf("deliver Notice wake: %s", output)
	}
	var batch noticeHandoffPayload
	if err := json.Unmarshal(output, &batch); err != nil {
		t.Fatalf("decode Notice wake %q: %v", output, err)
	}
	if len(batch.Notices) != 1 || batch.Notices[0].ID != fixture.noticeID || batch.Delivery.State != "printed" {
		t.Fatalf("delivered Notice wake = %+v", batch)
	}

	code, output = fixture.cli(t, "ack", strconv.FormatInt(fixture.noticeID, 10))
	if code != 0 {
		t.Fatalf("ack Notice in the wake's turn: %s", output)
	}
	delivery, err := fixture.db.NoticeDelivery(context.Background(), batch.Delivery.DeliveryID)
	if err != nil || delivery.State != "accepted" || delivery.AcceptedAt == 0 {
		t.Fatalf("acked Notice batch receipt = %+v, %v; want accepted", delivery, err)
	}
	notices, err := fixture.db.NoticesByIDs(context.Background(), fixture.project.ID, []int64{fixture.noticeID})
	if err != nil || len(notices) != 1 || notices[0].AckedAt == 0 || notices[0].DeliveredAt == 0 {
		t.Fatalf("acked Notice was not accepted as delivered: %+v, %v", notices, err)
	}

	nextID, err := fixture.db.CreateNotice(context.Background(), store.Notice{ProjectID: fixture.project.ID, Kind: "needs_decision", Summary: "Next Notice after the acknowledged batch"})
	if err != nil {
		t.Fatal(err)
	}
	code, output = fixture.cli(t, "watch", "--json", "--handoff", "--destination", fixture.destination, "--timeout", "2000")
	if code != 0 {
		t.Fatalf("deliver the next Notice after acknowledgement: %s", output)
	}
	if err := json.Unmarshal(output, &batch); err != nil {
		t.Fatalf("decode next Notice wake %q: %v", output, err)
	}
	if len(batch.Notices) != 1 || batch.Notices[0].ID != nextID {
		t.Fatalf("next wake repeated or lost a Notice: %+v", batch)
	}
}

func TestDoneSignalAcceptsRerunOfFailedRequiredChecks(t *testing.T) {
	fixture := newPRLifecycleFixtureWithManualWatchOwner(t)
	defer fixture.db.Close()

	ctx := context.Background()
	branch := "posse/t1"
	worktree := filepath.Join(fixture.root, "posse", "pool", "rerun-done")
	gitTest(t, fixture.env, fixture.repo, "worktree", "add", "-b", branch, worktree, "main")
	if err := os.WriteFile(filepath.Join(worktree, "change.txt"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.env, worktree, "add", "change.txt")
	gitTest(t, fixture.env, worktree, "commit", "-m", "change for rerun completion")
	head := strings.TrimSpace(gitTest(t, fixture.env, worktree, "rev-parse", "HEAD"))
	gitTest(t, fixture.env, worktree, "push", "origin", branch)

	taskID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{
		Seq: 1, Type: "change", Title: "Rerun completion", MergeMode: "pr", Branch: branch,
		BaseRef: "main", WorktreePath: worktree,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "E2E fixture"); err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/acme/shop/pull/17"
	if _, err := fixture.db.NextTaskLaunchWithIdentity(ctx, taskID, "deep", "", false); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.UpdateTaskMerge(ctx, taskID, prURL, ""); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.SetTaskGatedSHA(ctx, taskID, head); err != nil {
		t.Fatal(err)
	}
	openPRs, err := json.Marshal([]map[string]string{{"url": prURL, "headRefName": branch, "headRefOid": head}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.ghOpenPRs, openPRs, 0o600); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	failed := store.PRObservation{
		ProjectID: fixture.project.ID, TaskID: taskID, PRURL: prURL, HeadSHA: head, State: "OPEN",
		Checks: `{"state":"FAILURE","failures":[{"name":"unit","url":"https://checks.example/unit"}]}`,
		Review: "APPROVED", Mergeable: "MERGEABLE", ObservedAt: time.Now().UnixMilli(),
	}
	if _, err := fixture.db.RecordPRObservation(ctx, failed, store.PRObservationEffect{}, task); err != nil {
		t.Fatal(err)
	}

	workerEnv := setEnv(fixture.leadEnv, "HERDR_PANE_ID", "")
	signalDone := func() ([]byte, error) {
		worker := exec.Command(fixture.binary, "signal", "done", "Rerun completion", "--pr", prURL, "--turn", "launch:1")
		worker.Dir, worker.Env = worktree, workerEnv
		return worker.CombinedOutput()
	}

	// The forge still reports the failure: done stays refused.
	fixture.writeGraphQL(t, "pr1", "OPEN", "FAILURE", "APPROVED", "MERGEABLE", "", head)
	if output, err := signalDone(); err == nil || !strings.Contains(string(output), "checks_failed") {
		t.Fatalf("done Signal accepted a head whose checks still fail: err=%v output=%s", err, output)
	}

	// The Worker reran the failed job: the forge reports pending checks while the
	// stored observation is still the failure.
	fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "APPROVED", "MERGEABLE", "", head)
	if output, err := signalDone(); err != nil || !strings.Contains(string(output), "state: done") {
		t.Fatalf("done Signal was refused while the failed checks are rerunning: err=%v output=%s", err, output)
	}
	observation, err := fixture.db.LatestPRObservation(ctx, taskID)
	if err != nil || decodeChecksState(observation.Checks) != "PENDING" {
		t.Fatalf("observation after the rerun: %#v, %v", observation, err)
	}
}

func decodeChecksState(encoded string) string {
	var checks struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal([]byte(encoded), &checks)
	return checks.State
}
