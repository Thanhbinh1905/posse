package app

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/thanhbinh1905/posse/internal/greenhouse"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestGreenhouseAfterDownUsesStubbedSelfExec(t *testing.T) {
	home, err := os.MkdirTemp(os.Getenv("TMPDIR"), "g")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	greenhouseHome := greenhouse.HomeFor(home)
	if err := os.MkdirAll(greenhouseHome, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(greenhouseHome, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.WriteFile(filepath.Join(greenhouseHome, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(greenhouseHome, "state.json"), []byte(`{"runs":{"active":{"status":"running"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	previous := selfExecCommand
	called := false
	selfExecCommand = func(args ...string) (*exec.Cmd, error) {
		called = true
		if len(args) != 2 || args[0] != "_greenhouse" || args[1] != "stop" {
			t.Errorf("self-exec args = %q, want _greenhouse stop", args)
		}
		return exec.Command("true"), nil
	}
	t.Cleanup(func() { selfExecCommand = previous })

	message, err := greenhouseAfterDown(context.Background(), db, home)
	if err != nil || message != "stops after its reviews settle" {
		t.Fatalf("greenhouseAfterDown = %q, %v", message, err)
	}
	if !called {
		t.Fatal("Greenhouse stop waiter bypassed the stubbed self-exec seam")
	}
}
