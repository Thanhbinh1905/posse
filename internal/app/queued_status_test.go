package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestStatusMakesNoNetworkCallPerQueuedTask(t *testing.T) {
	fixture := newStartFixture(t)
	gitTest(t, fixture.repo, "remote", "add", "origin", "https://github.com/acme/shop.git")
	configText := "[defaults]\nmax_workers = 1\nmerge_mode = \"local\"\n\n[profiles.claude]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"claude\"\n"
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	shims := installNetworkShims(t)
	scratch := t.TempDir()

	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(context.Background(), fixture.project.ID, store.Task{Seq: 1, Type: "change", Title: "Current Worker", ShortName: "current-worker", State: store.StateSpawning}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = output, output
	for i := 0; i < 30; i++ {
		brief := filepath.Join(scratch, fmt.Sprintf("ticket-%d.md", i))
		if err := os.WriteFile(brief, []byte(fmt.Sprintf("---\ntype: change\ntitle: Ticketed queue %d\ndone_when: ticket %d lands\nticket: %d\n---\nQueued work.\n", i, 100+i, 100+i)), 0o600); err != nil {
			t.Fatal(err)
		}
		output.Reset()
		if code := cli.Run([]string{"start", "--brief", brief, "--queue"}); code != 0 {
			t.Fatalf("queue Task %d exit=%d output=%s", i, code, output.String())
		}
	}
	if err := os.WriteFile(shims.slow, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) []string {
		t.Helper()
		shims.take(t)
		output.Reset()
		code := cli.Run(args)
		if code != 0 {
			t.Fatalf("posse %v exit=%d output=%s", args, code, output.String())
		}
		return shims.take(t)
	}

	for _, args := range [][]string{{}, {"list"}} {
		if network := run(args...); len(network) != 0 {
			t.Errorf("posse %v with no free Worker slot made %d network calls:\n%s", args, len(network), strings.Join(network, "\n"))
		}
	}

	if err := os.WriteFile(shims.blockers, []byte(openBlocker), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(strings.Replace(configText, "max_workers = 1", "max_workers = 2", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{}, {}, {"list"}, {}, {"list"}} {
		if network := run(args...); len(network) > 1 {
			t.Errorf("posse %v with one free Worker slot made %d network calls, want at most 1:\n%s", args, len(network), strings.Join(network, "\n"))
		}
	}
	if declined := strings.Count(output.String(), "waiting for Change Task waits for open native blocker https://github.com/acme/shop/issues/17"); declined < 4 {
		t.Errorf("posse list showed %d recorded blocker wait reasons, want at least 4:\n%s", declined, output.String())
	}
}

type networkShims struct {
	calls, slow, blockers string
}

// installNetworkShims puts git and gh on PATH that log each origin ls-remote
// and GitHub API call, answer ls-remote with no refs, fetch with success and
// gh with the contents of blockers, and add 100 ms per call while slow exists.
func installNetworkShims(t *testing.T) networkShims {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	shims := networkShims{calls: filepath.Join(dir, "network.log"), slow: filepath.Join(dir, "slow"), blockers: filepath.Join(dir, "blockers.json")}
	if err := os.WriteFile(shims.blockers, []byte("[[]]"), 0o600); err != nil {
		t.Fatal(err)
	}
	scripts := map[string]string{
		"git": fmt.Sprintf("#!/bin/sh\ncase \" $* \" in\n*\" ls-remote \"*) echo \"ls-remote $*\" >> %q; [ -e %q ] && sleep 0.1; exit 0;;\n*\" fetch \"*) exit 0;;\nesac\nexec %q \"$@\"\n", shims.calls, shims.slow, realGit),
		"gh":  fmt.Sprintf("#!/bin/sh\necho \"gh $*\" >> %q\n[ -e %q ] && sleep 0.1\ncat %q\n", shims.calls, shims.slow, shims.blockers),
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return shims
}

func (s networkShims) take(t *testing.T) []string {
	t.Helper()
	logged, err := os.ReadFile(s.calls)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.calls, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if network := strings.TrimSpace(string(logged)); network != "" {
		return strings.Split(network, "\n")
	}
	return nil
}

const openBlocker = `[[{"number":17,"state":"open","repository_url":"https://api.github.com/repos/acme/shop","html_url":"https://github.com/acme/shop/issues/17"}]]`

func TestOnlyWatchRechecksADeclinedQueuedTask(t *testing.T) {
	ctx := context.Background()
	fixture := newStartFixture(t)
	gitTest(t, fixture.repo, "remote", "add", "origin", "https://github.com/acme/shop.git")
	configText := "[defaults]\nmax_workers = 1\nmerge_mode = \"local\"\n\n[profiles.claude]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"claude\"\n"
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	shims := installNetworkShims(t)
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 1, Type: "change", Title: "Current Worker", ShortName: "current-worker", State: store.StateSpawning}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(t.TempDir(), "ticket.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Ticketed queue\ndone_when: blocker closes\nticket: 12\n---\nQueued work.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"start", "--brief", brief, "--queue"}); code != 0 {
		t.Fatalf("queue exit=%d output=%s", code, output.String())
	}
	if err := os.WriteFile(shims.blockers, []byte(openBlocker), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(strings.Replace(configText, "max_workers = 1", "max_workers = 2", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	queued, err := db.Task(ctx, fixture.project.ID, "t2")
	if err != nil {
		t.Fatal(err)
	}
	refill := func(watch bool) store.State {
		t.Helper()
		shims.take(t)
		if _, err := fixture.service.refillQueuedTasks(ctx, db, fixture.project, cfg, watch); err != nil {
			t.Fatal(err)
		}
		task, err := db.TaskByID(ctx, fixture.project.ID, queued.ID)
		if err != nil {
			t.Fatal(err)
		}
		return task.State
	}

	if state := refill(false); state != store.StateQueued || len(shims.take(t)) != 1 {
		t.Fatalf("first refill: state=%s, want one blocker check that declines the queued Task", state)
	}
	if reason, err := db.QueuedTaskDeclineReason(ctx, queued.ID); err != nil || !strings.Contains(reason, "open native blocker https://github.com/acme/shop/issues/17") {
		t.Fatalf("recorded decline=%q err=%v", reason, err)
	}
	if state := refill(false); state != store.StateQueued || len(shims.take(t)) != 0 {
		t.Fatalf("status refill re-checked a declined Task: state=%s", state)
	}
	if state := refill(true); state != store.StateQueued || len(shims.take(t)) != 0 {
		t.Fatalf("Watch refill re-checked a decline younger than %s: state=%s", queuedDeclineRecheck, state)
	}
	stale := time.Now().Add(-queuedDeclineRecheck)
	if _, err := db.ExecContext(ctx, `UPDATE queued_task_declines SET checked_at=? WHERE task_id=?`, stale.UnixMilli(), queued.ID); err != nil {
		t.Fatal(err)
	}
	if state := refill(true); state != store.StateQueued || len(shims.take(t)) != 1 {
		t.Fatalf("Watch refill did not re-check a decline older than %s: state=%s", queuedDeclineRecheck, state)
	}
	declines, err := db.QueuedTaskDeclines(ctx, fixture.project.ID)
	if err != nil || !declines[queued.ID].CheckedAt.After(stale) {
		t.Fatalf("Watch re-check did not refresh the decline: %+v err=%v", declines, err)
	}
}
