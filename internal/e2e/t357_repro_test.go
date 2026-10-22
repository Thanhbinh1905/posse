//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestT357ReproMergeUnderWriter(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	brief := filepath.Join(f.root, "merge-contention.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Merge contention\ndone_when: committed change exists\n---\nCreate a committed change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.startAndComplete(t, brief, "t1")
	stopBackgroundWatch(t, f)
	if output := runPosse(t, f.binary, f.repo, f.leadEnv, "merge", "t1"); !strings.Contains(output, "merging") {
		t.Fatalf("merge did not gate the Task before contention: %s", output)
	}
	task := f.mustTask(t, "t1")
	f.writeGraphQL(t, "pr1", "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", task.GatedSHA)
	writer, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(context.Background(), `UPDATE projects SET name=name WHERE id=?`, f.project.ID); err != nil {
		_ = writer.Rollback()
		t.Fatal(err)
	}
	command := exec.Command(f.binary, "merge", "t1", "--execute", "--user-approved", "User approved merge")
	command.Dir, command.Env = f.repo, f.leadEnv
	started := time.Now()
	done := make(chan struct{})
	go func() {
		time.Sleep(7000 * time.Millisecond)
		_ = writer.Rollback()
		close(done)
	}()
	output, err := command.CombinedOutput()
	<-done
	t.Logf("merge elapsed=%s err=%v output=%s", time.Since(started), err, output)
	if err != nil || !strings.Contains(string(output), "merging") {
		t.Fatalf("merge did not recover from writer contention: err=%v output=%s", err, output)
	}
}

func TestT357ReapLeakedPollOnlyWatch(t *testing.T) {
	f := newPRLifecycleFixtureWithManualWatchOwner(t)
	defer f.db.Close()
	env := setEnv(f.env, "POSSE_WATCH_TOKEN", "")
	command := exec.Command(f.binary, "watch", "--poll-only")
	command.Dir, command.Env = f.repo, env
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("foreground poll-only Watch did not start: %v", watchPIDs(f.root))
	}
	foregroundPID := watchPIDs(f.root)[0]
	outputCommand := exec.Command(f.binary, "list")
	outputCommand.Dir, outputCommand.Env = f.repo, env
	output, err := outputCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("list failed while reaping leaked Watch: %v\\n%s", err, output)
	}
	if !waitForCondition(10*time.Second, func() bool {
		state, err := f.db.WatchRuntime(context.Background(), f.project.ID)
		return err == nil && state.PID > 0 && state.PID != foregroundPID && len(watchPIDs(f.root)) == 1
	}) {
		state, _ := f.db.WatchRuntime(context.Background(), f.project.ID)
		t.Fatalf("unbounded poll-only Watch was not reaped before replacement: pids=%v foreground=%d state=%#v", watchPIDs(f.root), foregroundPID, state)
	}
}

func TestT357ReproUpUnderWriter(t *testing.T) {
	f := newPRLifecycleFixtureWithLead(t, true)
	defer f.db.Close()
	ctx := context.Background()
	if err := f.db.MarkProjectDown(ctx, f.project.ID); err != nil {
		t.Fatal(err)
	}
	writer, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(ctx, `UPDATE projects SET name=name WHERE id=?`, f.project.ID); err != nil {
		_ = writer.Rollback()
		t.Fatal(err)
	}
	command := exec.Command(f.binary, "up", "--name", "shop", "--yes", "--replace")
	command.Dir, command.Env = f.repo, f.callerEnv
	started := time.Now()
	done := make(chan struct{})
	go func() {
		time.Sleep(7000 * time.Millisecond)
		_ = writer.Rollback()
		close(done)
	}()
	output, err := command.CombinedOutput()
	<-done
	t.Logf("up elapsed=%s err=%v output=%s", time.Since(started), err, output)
	if err != nil {
		t.Fatalf("up did not recover from writer contention: err=%v output=%s", err, output)
	}
}
