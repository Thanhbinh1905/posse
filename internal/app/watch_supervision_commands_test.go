package app

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
)

func TestWorkerCommandsSkipWatchSupervision(t *testing.T) {
	fixture := newWorkerFixture(t)
	t.Setenv(workerHomeEnv, fixture.home)
	previous := watchExecutableVerifier
	t.Cleanup(func() { watchExecutableVerifier = previous })
	calls := 0
	watchExecutableVerifier = func(string) bool {
		calls++
		return false
	}
	service := New(fixture.home, nil)
	ctx := &axi.Context{Context: context.Background(), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}
	for _, name := range []string{"validate", "publish", "signal"} {
		command := &axi.Command{Name: name, Handler: func(*axi.Context, []string) error { return nil }}
		service.structureStoreErrors(command)
		if err := command.Handler(ctx, nil); err != nil {
			t.Fatalf("Worker %s command failed: %v", name, err)
		}
	}
	if calls != 0 {
		t.Fatalf("Worker commands triggered Project Watch supervision %d times", calls)
	}
}

func TestNoticeOnlyCommandsSkipWatchSupervision(t *testing.T) {
	previous := watchExecutableVerifier
	t.Cleanup(func() { watchExecutableVerifier = previous })
	calls := 0
	watchExecutableVerifier = func(string) bool {
		calls++
		return false
	}
	service := New(t.TempDir(), nil)
	ctx := &axi.Context{Context: context.Background(), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}
	for _, test := range []struct {
		command string
		args    []string
	}{{command: "ack"}, {command: "lookout"}, {command: "watch", args: []string{"--receipt=receipt"}}, {command: "watch", args: []string{"--handoff", "--destination", "pi:session"}}, {command: "watch", args: []string{"--handoff", "--destination", "opencode:session"}}} {
		name := test.command + " " + strings.Join(test.args, " ")
		t.Run(name, func(t *testing.T) {
			before := calls
			service.superviseWatchRuntimes(ctx, test.command, test.args)
			if calls != before {
				t.Fatalf("delivery-only %s command probed Project Watch supervision", name)
			}
		})
	}
}

func TestForegroundPollOnlyWatchSkipsWatchSupervision(t *testing.T) {
	previous := watchExecutableVerifier
	t.Cleanup(func() { watchExecutableVerifier = previous })
	calls := 0
	watchExecutableVerifier = func(string) bool {
		calls++
		return false
	}
	service := New(t.TempDir(), nil)
	ctx := &axi.Context{Context: context.Background(), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}
	service.superviseWatchRuntimes(ctx, "watch", []string{"--poll-only", "--timeout", "2500"})
	if calls != 0 {
		t.Fatal("foreground --poll-only Watch was supervised after its wait loop")
	}
}

func TestWatchRepairSkipsDuplicateWatchSupervision(t *testing.T) {
	previous := watchExecutableVerifier
	t.Cleanup(func() { watchExecutableVerifier = previous })
	calls := 0
	watchExecutableVerifier = func(string) bool {
		calls++
		return false
	}
	service := New(t.TempDir(), nil)
	ctx := &axi.Context{Context: context.Background(), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}
	service.superviseWatchRuntimes(ctx, "watch", []string{"--repair"})
	if calls != 0 {
		t.Fatal("Watch repair redundantly supervised the runtime it just repaired")
	}
}

func TestWatchCommandRunsWatchSupervision(t *testing.T) {
	previous := watchExecutableVerifier
	t.Cleanup(func() { watchExecutableVerifier = previous })
	calls := 0
	watchExecutableVerifier = func(string) bool {
		calls++
		return false
	}
	service := New(t.TempDir(), nil)
	ctx := &axi.Context{Context: context.Background(), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}
	service.superviseWatchRuntimes(ctx, "watch", nil)
	if calls == 0 {
		t.Fatal("Watch command did not run Project Watch supervision")
	}
}
