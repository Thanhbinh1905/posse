//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func t228InstallForgeLag(t *testing.T, f *t205PRFixture) {
	t.Helper()
	for _, name := range []string{"gh", "glab"} {
		path := filepath.Join(f.root, "bin", name)
		original := readTestFile(t, path)
		original = strings.Replace(original, "import json", "import json\nimport time", 1)
		original = strings.Replace(original, "def head():\n", `def head():
    lag_path = root / 't228-lag.json'
    listing = args[:2] == ['pr', 'list'] or any('merge_requests?state=opened' in a for a in args)
    if listing and lag_path.exists():
        lag = json.loads(lag_path.read_text())
        start_path = root / 't228-lag-start'
        if not start_path.exists():
            start_path.write_text(str(time.monotonic()))
        elapsed = time.monotonic() - float(start_path.read_text())
        if elapsed < lag['duration']:
            return lag['head']
`, 1)
		if err := os.WriteFile(path, []byte(original), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func t228SetForgeLag(t *testing.T, f *t205PRFixture, head string, durationSeconds float64) {
	t.Helper()
	_ = os.Remove(filepath.Join(f.root, "t228-lag-start"))
	data, err := json.Marshal(map[string]any{"head": head, "duration": durationSeconds})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "t228-lag.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func t228Commit(t *testing.T, f *t205PRFixture, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.task.WorktreePath, name), []byte(name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, f.task.WorktreePath, "add", name)
	gitTest(t, f.env, f.task.WorktreePath, "commit", "-m", name)
	return strings.TrimSpace(gitTest(t, f.env, f.task.WorktreePath, "rev-parse", "HEAD"))
}

func t228SeedUnverifiedOpenPR(t *testing.T, f *t205PRFixture) string {
	t.Helper()
	// The fixture's 1ms PR poller runs git status in this same linked
	// worktree. Stop that unrelated poller before mutating its index directly.
	stopBackgroundLookout(t, f.prLifecycleFixture)
	oldHead := strings.TrimSpace(gitTest(t, f.env, f.task.WorktreePath, "rev-parse", "HEAD"))
	gitTest(t, f.env, f.task.WorktreePath, "push", "origin", "refs/heads/"+f.task.Branch+":refs/heads/"+f.task.Branch)
	state := `{"url":"https://github.com/acme/shop/pull/17","title":"PR lifecycle change","body":""}`
	if err := os.WriteFile(f.statePath(), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.callsPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	verified, err := f.db.WasVerifiedPRHead(context.Background(), f.task.ID, "https://github.com/acme/shop/pull/17", oldHead)
	if err != nil || verified {
		t.Fatalf("fixture head must start unverified: verified=%t err=%v", verified, err)
	}
	return oldHead
}

func t228AssertCrash86(t *testing.T, output string, err error) {
	t.Helper()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 86 {
		t.Fatalf("lookup crash injection: %v\n%s", err, output)
	}
}

func t228WorkspaceFixture(t *testing.T) *t205PRFixture {
	t.Helper()
	f := t205NewPRFixture(t, "github")
	// The fixture's 1ms PR poller reads the Rider worktree while these tests
	// create commits and exercise direct publish recovery.
	stopBackgroundLookout(t, f.prLifecycleFixture)
	t228InstallForgeLag(t, f)
	ctx := context.Background()
	if _, err := f.db.ExecContext(ctx, "UPDATE projects SET kind='workspace', root=? WHERE id=?", f.root, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SyncProjectRepos(ctx, f.project.ID, []store.ProjectRepo{{Name: "repo", Path: "repo", DefaultBranch: "main", Status: store.RepoActive}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, "UPDATE tasks SET worktree_path=? WHERE id=?", filepath.Dir(f.task.WorktreePath), f.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreateTaskRepos(ctx, f.task.ID, []store.TaskRepo{{Repo: "repo", WorktreePath: f.task.WorktreePath, BaseRef: "main", LandingMode: "pr", State: store.TaskRepoOpen}}); err != nil {
		t.Fatal(err)
	}
	return f
}

func t228WorkspacePublishArgs(summary string) []string {
	return append([]string{"--repo", "repo"}, t205PublishArgs(summary)...)
}

func TestPublishRemembersUnverifiedPrePushHeadAcrossRestart(t *testing.T) {
	f := t205NewPRFixture(t, "github")
	defer f.db.Close()
	t228InstallForgeLag(t, f)
	oldHead := t228SeedUnverifiedOpenPR(t, f)
	t228Commit(t, f, "repo-restart-follow-up.txt")
	t228SetForgeLag(t, f, oldHead, 10)
	before := readTestFile(t, f.callsPath())
	output, err := f.publish(t, setEnv(f.workerEnv, "POSSE_INTENT_CRASH_AT", "publish:after:pr.lookup"), t205PublishArgs("restart")...)
	t228AssertCrash86(t, output, err)
	t205AssertNoForgeMutation(t, strings.TrimPrefix(readTestFile(t, f.callsPath()), before))
	output, err = f.publish(t, f.workerEnv, t205PublishArgs("restart")...)
	if err != nil {
		t.Fatalf("restart rejected the known unverified pre-push head: %v\n%s", err, output)
	}
}

func TestPublishRetriesAfterTimeoutForUnverifiedPrePushHead(t *testing.T) {
	f := t205NewPRFixture(t, "github")
	defer f.db.Close()
	t228InstallForgeLag(t, f)
	oldHead := t228SeedUnverifiedOpenPR(t, f)
	t228Commit(t, f, "repo-timeout-follow-up.txt")
	t228SetForgeLag(t, f, oldHead, 45)
	before := readTestFile(t, f.callsPath())
	output, err := f.publish(t, f.workerEnv, t205PublishArgs("timeout")...)
	if err == nil || !strings.Contains(output, "pr_head_lag_timeout") || !strings.Contains(output, ",true,") {
		t.Fatalf("expected retryable lag timeout: %v\n%s", err, output)
	}
	t205AssertNoForgeMutation(t, strings.TrimPrefix(readTestFile(t, f.callsPath()), before))
	intents, err := f.db.Intents(context.Background(), f.project.ID)
	if err != nil || len(intents) != 0 {
		t.Fatalf("timeout left a pending publish intent: %v %v", intents, err)
	}
	output, err = f.publish(t, f.workerEnv, t205PublishArgs("timeout")...)
	if err != nil {
		t.Fatalf("immediate retry rejected the known unverified pre-push head: %v\n%s", err, output)
	}
}

func TestWorkspacePublishRemembersUnverifiedPrePushHeadAcrossRestart(t *testing.T) {
	f := t228WorkspaceFixture(t)
	defer f.db.Close()
	f.publishOK(t, t228WorkspacePublishArgs("first")...)
	oldHead := strings.TrimSpace(gitTest(t, f.env, f.task.WorktreePath, "rev-parse", "HEAD"))
	t228Commit(t, f, "workspace-restart-follow-up.txt")
	t228SetForgeLag(t, f, oldHead, 10)
	before := readTestFile(t, f.callsPath())
	output, err := f.publish(t, setEnv(f.workerEnv, "POSSE_INTENT_CRASH_AT", "publish:after:pr.lookup"), t228WorkspacePublishArgs("restart")...)
	t228AssertCrash86(t, output, err)
	t205AssertNoForgeMutation(t, strings.TrimPrefix(readTestFile(t, f.callsPath()), before))
	output, err = f.publish(t, f.workerEnv, t228WorkspacePublishArgs("restart")...)
	if err != nil {
		t.Fatalf("Workspace restart rejected the known pre-push head: %v\n%s", err, output)
	}
	if edits := strings.Count(strings.TrimPrefix(readTestFile(t, f.callsPath()), before), `"edit"`); edits != 1 {
		t.Fatalf("Workspace restart performed %d metadata edits, want 1", edits)
	}
}

func TestWorkspacePublishRetriesAfterTimeoutForUnverifiedPrePushHead(t *testing.T) {
	f := t228WorkspaceFixture(t)
	defer f.db.Close()
	f.publishOK(t, t228WorkspacePublishArgs("first")...)
	oldHead := strings.TrimSpace(gitTest(t, f.env, f.task.WorktreePath, "rev-parse", "HEAD"))
	t228Commit(t, f, "workspace-timeout-follow-up.txt")
	t228SetForgeLag(t, f, oldHead, 45)
	before := readTestFile(t, f.callsPath())
	output, err := f.publish(t, f.workerEnv, t228WorkspacePublishArgs("timeout")...)
	if err == nil || !strings.Contains(output, "pr_head_lag_timeout") || !strings.Contains(output, ",true,") {
		t.Fatalf("expected retryable Workspace lag timeout: %v\n%s", err, output)
	}
	t205AssertNoForgeMutation(t, strings.TrimPrefix(readTestFile(t, f.callsPath()), before))
	intents, err := f.db.Intents(context.Background(), f.project.ID)
	if err != nil || len(intents) != 0 {
		t.Fatalf("Workspace timeout left a pending publish intent: %v %v", intents, err)
	}
	output, err = f.publish(t, f.workerEnv, t228WorkspacePublishArgs("timeout")...)
	if err != nil {
		t.Fatalf("immediate Workspace retry rejected the known pre-push head: %v\n%s", err, output)
	}
	if edits := strings.Count(strings.TrimPrefix(readTestFile(t, f.callsPath()), before), `"edit"`); edits != 1 {
		t.Fatalf("Workspace timeout/retry performed %d metadata edits, want 1", edits)
	}
}

func TestPublishRejectsUnexpectedHeadDespitePersistedPrePushEvidence(t *testing.T) {
	f := t205NewPRFixture(t, "github")
	defer f.db.Close()
	t228InstallForgeLag(t, f)
	oldHead := t228SeedUnverifiedOpenPR(t, f)
	t228Commit(t, f, "unexpected-head-follow-up.txt")
	unexpected := strings.Repeat("f", 40)
	if unexpected == oldHead {
		unexpected = strings.Repeat("e", 40)
	}
	t228SetForgeLag(t, f, unexpected, 10)
	before := readTestFile(t, f.callsPath())
	started := time.Now()
	output, err := f.publish(t, f.workerEnv, t205PublishArgs("unexpected")...)
	if err == nil || !strings.Contains(output, "branch_moved") || !strings.Contains(output, ",false,") {
		t.Fatalf("unexpected head was not rejected non-retryably: %v\n%s", err, output)
	}
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("unexpected head was not rejected immediately: %s", elapsed)
	}
	t205AssertNoForgeMutation(t, strings.TrimPrefix(readTestFile(t, f.callsPath()), before))
}
