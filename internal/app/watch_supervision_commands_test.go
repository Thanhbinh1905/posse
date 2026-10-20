package app

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
)

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
	}{{command: "ack"}, {command: "lookout"}, {command: "watch", args: []string{"--receipt=receipt"}}} {
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

func TestPiExtensionHandoffRunsWatchSupervision(t *testing.T) {
	previous := watchExecutableVerifier
	t.Cleanup(func() { watchExecutableVerifier = previous })
	calls := 0
	watchExecutableVerifier = func(string) bool {
		calls++
		return false
	}
	service := New(t.TempDir(), nil)
	ctx := &axi.Context{Context: context.Background(), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}
	service.superviseWatchRuntimes(ctx, "watch", []string{"--handoff", "--destination", "pi:repair"})
	if calls == 0 {
		t.Fatal("Pi extension handoff did not run Project Watch supervision")
	}
}

func TestAdapterHandoffsRunWatchSupervision(t *testing.T) {
	previous := watchExecutableVerifier
	t.Cleanup(func() { watchExecutableVerifier = previous })
	calls := 0
	watchExecutableVerifier = func(string) bool {
		calls++
		return false
	}
	service := New(t.TempDir(), nil)
	ctx := &axi.Context{Context: context.Background(), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}
	for _, destination := range []string{"pi:repair", "opencode:repair"} {
		before := calls
		service.superviseWatchRuntimes(ctx, "watch", []string{"--handoff", "--destination", destination})
		if calls == before {
			t.Errorf("%s handoff did not run Project Watch supervision", destination)
		}
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
