//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestTimedOutWatcherGitReadDoesNotLockWorktreeOrFailLookout(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	stopBackgroundLookout(t, f)

	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("[defaults]\nlanding_mode = \"pr\"\nauto_unsaddle = \"never\"\npr_poll = \"1ms\"\nstall_after = \"30m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(f.root, "watcher-rider.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: Watcher timeout fixture\ndone_when: test ends\n---\nWait for the test.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := f.runRideWithStoreBusyRetry(t, brief, "watcher-timeout-fixture"); !strings.Contains(output, "t1") {
		t.Fatalf("ride did not create t1: %s", output)
	}
	task := f.mustTask(t, "t1")
	if !waitForCondition(10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(f.root, "worker-ready-t1"))
		return err == nil
	}) {
		t.Fatal("fixture Rider did not reach its working turn")
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	statusStarted := filepath.Join(f.root, "watcher-git-status-started")
	statusLog := filepath.Join(f.root, "watcher-git-status.log")
	gitShim := "#!/bin/sh\n" +
		"set -eu\n" +
		"real_git=" + shellQuote(realGit) + "\n" +
		"target=" + shellQuote(task.WorktreePath) + "\n" +
		"started=" + shellQuote(statusStarted) + "\n" +
		"log=" + shellQuote(statusLog) + "\n" +
		"is_target=0\nexpect_path=0\nstatus=0\noptional_locks=0\n" +
		"[ \"${GIT_OPTIONAL_LOCKS:-}\" = 0 ] && optional_locks=1\n" +
		"for arg do\n" +
		"  case \"$arg\" in --no-optional-locks) optional_locks=1 ;; esac\n" +
		"  if [ \"$expect_path\" -eq 1 ]; then [ \"$arg\" = \"$target\" ] && is_target=1; expect_path=0; continue; fi\n" +
		"  if [ \"$arg\" = -C ]; then expect_path=1; continue; fi\n" +
		"  if [ \"$arg\" = status ]; then status=1; fi\n" +
		"done\n" +
		"if [ \"$is_target\" -eq 1 ] && [ \"$status\" -eq 1 ]; then\n" +
		"  printf 'optional_locks=%s args=%s\\n' \"$optional_locks\" \"$*\" >> \"$log\"\n" +
		"  : > \"$started\"\n" +
		"  if [ \"$optional_locks\" -eq 0 ]; then\n" +
		"    lock=$($real_git -C \"$target\" rev-parse --git-path index.lock)\n" +
		"    case \"$lock\" in /*) ;; *) lock=\"$target/$lock\" ;; esac\n" +
		"    : > \"$lock\"\n" +
		"  fi\n" +
		"  while :; do sleep 1; done\n" +
		"fi\n" +
		"exec \"$real_git\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(f.root, "bin", "git"), []byte(gitShim), 0o700); err != nil {
		t.Fatal(err)
	}

	noticeID, err := f.db.CreateNotice(context.Background(), store.Notice{
		ProjectID: f.project.ID, Kind: "worker_blocked", Summary: "trigger isolated watcher", DataJSON: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.MarkNoticesDelivered(context.Background(), f.project.ID, []int64{noticeID}, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(f.binary, "lookout", "--ack", strconv.FormatInt(noticeID, 10), "--timeout", "3500")
	command.Dir, command.Env = f.repo, f.leadEnv
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated Lookout failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(statusStarted); err != nil {
		t.Fatalf("watcher did not read the Rider worktree status: %v\n%s", err, output)
	}
	if data, err := os.ReadFile(statusLog); err != nil || !strings.Contains(string(data), "optional_locks=1") {
		t.Fatalf("watcher status invocation did not disable optional locks: %q %v", data, err)
	}
	lockPath, err := exec.Command(realGit, "-C", task.WorktreePath, "rev-parse", "--git-path", "index.lock").Output()
	if err != nil {
		t.Fatalf("resolve Rider index lock path: %v", err)
	}
	if _, err := os.Stat(strings.TrimSpace(string(lockPath))); !os.IsNotExist(err) {
		t.Fatalf("timed-out watcher left index.lock: stat error=%v", err)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "pr_watch_failing" {
			t.Fatalf("timed-out progress read raised pr_watch_failing: %#v", notice)
		}
	}
}
