//go:build e2e && linux

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestResearchNoticeAcknowledgementTearsDownUnbornRepository(t *testing.T) {
	f := newWorkerTabsFixtureWithOptions(t, workerTabsOptions{emptyFolder: true})
	if err := os.WriteFile(filepath.Join(f.home, "projects", "shop", "config.toml"), []byte("[defaults]\nauto_teardown = \"finished\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	researchBrief := filepath.Join(f.root, "research.md")
	if err := os.WriteFile(researchBrief, []byte("---\ntype: research\ntitle: Survey the empty Project\ndone_when: report.md is complete\n---\nWrite a concise Report.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, taskID := range []string{"t1", "t2", "t3"} {
		name := "research-" + taskID
		if output := runPosse(t, f.binary, f.repo, f.leadEnv, "start", "--brief", researchBrief, "--name", name); !strings.Contains(output, taskID) {
			t.Fatalf("start %s: %s", taskID, output)
		}
		f.waitState(t, taskID, store.StateWorking)
		task := f.task(t, taskID)
		if err := os.WriteFile(filepath.Join(task.WorktreePath, "report.md"), []byte(taskID+" reviewed the empty Project.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runPosse(t, f.binary, task.WorktreePath, workerSignalEnv(f, task), "signal", "done", "Report ready", "--report", "report.md")
		f.waitState(t, taskID, store.StateReported)
		notices, err := db.Notices(context.Background(), f.projectID, false)
		if err != nil {
			t.Fatal(err)
		}
		var noticeID string
		for _, notice := range notices {
			if notice.TaskID == task.ID && notice.Kind == "task_done" {
				noticeID = fmt.Sprint(notice.ID)
				break
			}
		}
		if noticeID == "" {
			t.Fatalf("%s has no task_done Notice: %#v", taskID, notices)
		}
		if taskID == "t1" {
			gitTest(t, f.env, f.repo, "symbolic-ref", "HEAD", "refs/heads/not-main")
			runPosseExpectingFailure(t, f, f.repo, f.leadEnv, "teardown_incomplete", "ack", noticeID)
			f.waitState(t, taskID, store.StateReported)
			sandbox, err := db.SandboxByTask(context.Background(), task.ID)
			if err != nil || sandbox.State != "held" {
				t.Fatalf("failed teardown did not preserve the held Sandbox: %#v %v", sandbox, err)
			}
			gitTest(t, f.env, f.repo, "symbolic-ref", "HEAD", "refs/heads/main")
			if output := runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", taskID); !strings.Contains(output, "torn-down") {
				t.Fatalf("retry teardown %s: %s", taskID, output)
			}
		} else {
			output := runPosse(t, f.binary, f.repo, f.leadEnv, "ack", noticeID)
			if !strings.Contains(output, "acknowledged: 1") {
				t.Fatalf("ack %s: %s", noticeID, output)
			}
		}
		f.waitState(t, taskID, store.StateTornDown)
		if _, err := gitCommand(f.env, f.repo, "rev-parse", "--verify", "refs/heads/main^{commit}"); err == nil {
			t.Fatalf("%s teardown created a commit on unborn main", taskID)
		}
	}
}

// Reproduce #699: Posse starts in a new folder without manual git setup, runs
// Tasks before the first commit and lands the first commit by fast-forward.
func TestNewFolderLandsFirstCommitWithoutGitSetup(t *testing.T) {
	f := newWorkerTabsFixtureWithOptions(t, workerTabsOptions{emptyFolder: true})
	if branch := strings.TrimSpace(gitTest(t, f.env, f.repo, "symbolic-ref", "--short", "HEAD")); branch != "main" {
		t.Fatalf("posse up initialized branch %q, want main", branch)
	}
	if out, err := gitCommand(f.env, f.repo, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		t.Fatalf("posse up created a commit: %s", out)
	}
	if show := runPosse(t, f.binary, f.repo, f.leadEnv, "project", "show"); !strings.Contains(show, "merge_mode: local") {
		t.Fatalf("a Project without origin does not use local Merge Mode: %s", show)
	}

	researchBrief := filepath.Join(f.root, "research.md")
	if err := os.WriteFile(researchBrief, []byte("---\ntype: research\ntitle: Survey the empty Project\ndone_when: report.md is complete\n---\nWrite a concise Report.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, f.binary, f.repo, f.leadEnv, "start", "--brief", researchBrief, "--name", "survey"); !strings.Contains(output, "t1") {
		t.Fatalf("research start: %s", output)
	}
	f.waitState(t, "t1", store.StateWorking)
	research := f.task(t, "t1")
	if err := os.WriteFile(filepath.Join(research.WorktreePath, "report.md"), []byte("The Project is empty.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, research.WorktreePath, workerSignalEnv(f, research), "signal", "done", "Report ready", "--report", "report.md")
	if output := runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t1"); !strings.Contains(output, "torn-down") {
		t.Fatalf("research teardown before the first commit: %s", output)
	}

	// The Change Task reuses the Research Task's Sandbox.
	change := f.start(t, "t2", "First file", "first-file")
	if change.WorktreePath != research.WorktreePath {
		t.Fatalf("t2 Sandbox %s, want the reused %s", change.WorktreePath, research.WorktreePath)
	}
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "hello.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, change.WorktreePath, "add", "hello.txt")
	gitTest(t, f.env, change.WorktreePath, "commit", "-m", "Add hello")
	runPosse(t, f.binary, change.WorktreePath, workerSignalEnv(f, change), "signal", "done", "Added hello.txt")
	if output := runPosse(t, f.binary, f.repo, f.leadEnv, "merge", "t2", "--execute", "--user-approved", "User approved the first local merge"); !strings.Contains(output, "merged") {
		t.Fatalf("local merge: %s", output)
	}
	if got := strings.TrimSpace(gitTest(t, f.env, f.repo, "log", "--format=%s", "main")); got != "Add hello" {
		t.Fatalf("main history = %q, want the Task commit as its first commit", got)
	}
	if contents, err := os.ReadFile(filepath.Join(f.repo, "hello.txt")); err != nil || string(contents) != "hello\n" {
		t.Fatalf("root checkout does not show the merged file: %q %v", contents, err)
	}
	if output := runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t2"); !strings.Contains(output, "torn-down") {
		t.Fatalf("merged change teardown: %s", output)
	}

	// A PR-mode start without a remote is a repository precondition: it
	// refuses before admission, so no Task fails and no Sandbox breaks.
	prBrief := filepath.Join(f.root, "pr.md")
	if err := os.WriteFile(prBrief, []byte("---\ntype: change\ntitle: Needs a PR\ndone_when: a PR exists\nmerge_mode: pr\n---\nOpen a PR.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosseExpectingFailure(t, f, f.repo, f.leadEnv, "remote_missing", "start", "--brief", prBrief, "--name", "needs-pr")
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tasks, err := db.Tasks(context.Background(), f.projectID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.State == store.StateFailed || task.Seq > 2 {
			t.Fatalf("the refused start left Task t%d in %s", task.Seq, task.State)
		}
	}
	sandboxes, err := db.Sandboxes(context.Background(), f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, sandbox := range sandboxes {
		if sandbox.State == "broken" {
			t.Fatalf("sandbox-%d is broken", sandbox.Number)
		}
	}
}
