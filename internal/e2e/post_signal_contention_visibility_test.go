//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestPostSignalMixedContentionAndPersistentFailureIsVisible(t *testing.T) {
	f, task, env, marker, gate := newPostSignalContentionFixture(t)
	if _, err := f.db.ExecContext(context.Background(), `CREATE TRIGGER review_nontransient BEFORE UPDATE OF pane_id ON tasks WHEN OLD.seq=8 BEGIN SELECT RAISE(ABORT, 'non-transient review constraint failure'); END`); err != nil {
		t.Fatal(err)
	}
	output, done := startPostSignalContention(t, f, task, env, marker, "failed")
	writer, err := store.OpenAt(filepath.Join(f.home, "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tx, err := writer.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := os.WriteFile(gate, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(900 * time.Millisecond)
		released <- tx.Commit()
	}()
	if err := <-done; err != nil {
		t.Fatalf("committed Signal rejected: %v", err)
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	var stale int64
	if err := f.db.QueryRowContext(context.Background(), `SELECT updated_at FROM tasks WHERE project_id=? AND seq=2`, f.project.ID).Scan(&stale); err != nil || stale != 2 {
		t.Fatalf("first unrelated Task did not exercise busy failure: %d %v", stale, err)
	}
	if strings.Contains(output.String(), "SQLITE_BUSY") || strings.Contains(output.String(), "database contention") || !strings.Contains(output.String(), "non-transient review constraint failure") {
		t.Fatalf("Rider should see persistent maintenance failure without transient contention details: %s", output.String())
	}
	stored, err := f.db.TaskByID(context.Background(), f.project.ID, task.ID)
	if err != nil || stored.State != store.StateFailed {
		t.Fatalf("committed Signal state was not retained: %#v %v", stored, err)
	}
}

func TestRosterSucceedsWhenObservationOnlyHitsTransientContention(t *testing.T) {
	f, _, _, _, _ := newPostSignalContentionFixture(t)
	writer, err := store.OpenAt(filepath.Join(f.home, "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tx, err := writer.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	released := make(chan error, 1)
	go func() {
		time.Sleep(1200 * time.Millisecond)
		released <- tx.Commit()
	}()
	command := exec.Command(f.binary, "roster", "--json")
	command.Dir, command.Env = f.repo, f.leadEnv
	output, commandErr := command.CombinedOutput()
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if commandErr != nil || strings.Contains(string(output), "store_busy") || !bytes.Contains(output, []byte(`"tasks"`)) || !bytes.Contains(output, []byte("Some Task observations were deferred")) {
		t.Fatalf("roster should show its durable Task view and explain deferred observations: error=%v output=%s", commandErr, output)
	}
}

func newPostSignalContentionFixture(t *testing.T) (*prLifecycleFixture, store.Task, []string, string, string) {
	t.Helper()
	f := newPRLifecycleFixtureWithLead(t, true)
	t.Cleanup(func() { _ = f.db.Close() })
	for _, pid := range lookoutPIDs(f.root) {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Fatal(err)
		}
	}
	if !waitForCondition(5*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 0 }) {
		t.Fatal("fixture Lookout did not stop")
	}
	ctx := context.Background()
	client := herdr.NewWithEnv("herdr", f.env)
	var first store.Task
	for seq := 1; seq <= 8; seq++ {
		label := fmt.Sprintf("posse:shop:t%d", seq)
		pane, err := createTab(client, f.project.HerdrWorkspaceID, f.repo, label)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Call(ctx, "pane.rename", map[string]any{"pane_id": pane.RootPane.PaneID, "label": label}); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Run(ctx, "pane", "report-agent", pane.RootPane.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "working"); err != nil {
			t.Fatal(err)
		}
		id, err := f.db.CreateTask(ctx, f.project.ID, store.Task{Seq: seq, Type: "scout", Title: label, LandingMode: "local", Branch: label, BaseRef: "main", WorktreePath: f.repo, HerdrWorkspaceID: f.project.HerdrWorkspaceID, PaneID: pane.RootPane.PaneID, PaneLabel: label})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.db.Transition(ctx, id, store.StateSpawning, store.StateWorking, "cli", "ready"); err != nil {
			t.Fatal(err)
		}
		if seq == 1 {
			mount, err := f.db.AcquireMount(ctx, f.project.ID, id, filepath.Join(f.home, "remuda"))
			if err != nil {
				t.Fatal(err)
			}
			gitTest(t, f.env, f.repo, "worktree", "add", "-b", "posse/review-a29", mount.Path, "main")
			first, err = f.db.TaskByID(ctx, f.project.ID, id)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET updated_at=seq WHERE project_id=?`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	realHerdr, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	marker, gate := filepath.Join(f.root, "post-commit"), filepath.Join(f.root, "allow-status")
	shim := filepath.Join(f.root, "status-gate-herdr")
	script := "#!/bin/sh\nset -eu\nif [ \"$*\" = 'status server --json' ]; then\n  : > " + shellQuote(marker) + "\n  while [ ! -e " + shellQuote(gate) + " ]; do sleep 0.01; done\nfi\nexec " + shellQuote(realHerdr) + " \"$@\"\n"
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	env := setEnv(f.env, "HERDR_ENV", "1")
	env = setEnv(env, "HERDR_PANE_ID", first.PaneID)
	env = setEnv(env, "HERDR_WORKSPACE_ID", first.HerdrWorkspaceID)
	env = setEnv(env, "HERDR_BIN_PATH", shim)
	env = setEnv(env, "POSSE_WORKER_HOME", f.home)
	return f, first, env, marker, gate
}

func startPostSignalContention(t *testing.T, f *prLifecycleFixture, task store.Task, env []string, marker, verb string) (*bytes.Buffer, chan error) {
	t.Helper()
	command := exec.Command(f.binary, "holler", verb, "post-Signal contention reproduction")
	command.Env, command.Dir = env, task.WorktreePath
	output := &bytes.Buffer{}
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() { _ = command.Process.Kill() })
	if !waitForCondition(10*time.Second, func() bool { _, err := os.Stat(marker); return err == nil }) {
		t.Fatal("post-commit status checkpoint was not reached")
	}
	signals, err := f.db.TaskSignals(context.Background(), task.ID, 10)
	if err != nil || len(signals) != 1 || signals[0].Verb != verb {
		t.Fatalf("Signal not committed at checkpoint: %#v %v", signals, err)
	}
	return output, done
}
