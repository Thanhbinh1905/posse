//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// Exercise the hook decision and, when allowed, the actual shell command on
// a disposable Herdr server. No command in this test can reach the host server.
func TestWorkerGuardAgainstIsolatedHerdr(t *testing.T) {
	root := newFixtureRoot(t, herdr.TestRootName())
	env := herdr.IsolatedTestEnvironment(root)
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"home", "posse"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.Open(filepath.Join(root, "posse"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	bin := filepath.Join(root, "posse-bin")
	build := exec.Command("go", "build", "-o", bin, "./cmd/posse")
	build.Dir = moduleRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	client := herdr.NewWithEnv("herdr", env)
	startServer(t, client)
	foreign, err := createWorkspace(client, root)
	if err != nil {
		t.Fatal(err)
	}
	socket := herdr.SocketPath(env)
	foreignBinary := filepath.Join(root, "pm")
	binaryData, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreignBinary, binaryData, 0700); err != nil {
		t.Fatal(err)
	}
	workerEnv := setEnv(env, "POSSE_WORKER_HOME", filepath.Join(root, "posse"))
	workerEnv = setEnv(workerEnv, "HERDR_SOCKET_PATH", socket)
	workerEnv = setEnv(workerEnv, "HERDR_PANE_ID", "w-test:p-test")
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	// This path is not linked to the fixture socket. It is deliberately absent.
	isolated := filepath.Join(other, "herdr.sock")
	id := foreign.Workspace.WorkspaceID
	otherRoot := newFixtureRoot(t, herdr.TestRootName())
	otherEnv := herdr.IsolatedTestEnvironment(otherRoot)
	if _, err := herdr.WriteIsolatedConfig(otherRoot); err != nil {
		t.Fatal(err)
	}
	otherClient := herdr.NewWithEnv("herdr", otherEnv)
	startServer(t, otherClient)
	otherWorkspace, err := createWorkspace(otherClient, otherRoot)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, command string
		blocked       bool
	}{
		{"false branch", fmt.Sprintf("if false; then export HERDR_SOCKET_PATH=%s; fi; herdr workspace rename %s forbidden", isolated, id), true},
		{"help operand", fmt.Sprintf("herdr workspace rename %s -- --help", id), true},
		{"hardlink alias", fmt.Sprintf("ln -f $HERDR_SOCKET_PATH ./alias.sock && HERDR_SOCKET_PATH=./alias.sock herdr workspace rename %s forbidden", id), true},
		{"generated script", fmt.Sprintf("printf 'herdr workspace rename %s forbidden\\n' > ./generated.sh && bash ./generated.sh", id), true},
		{"generated make", fmt.Sprintf("printf 'all:\\n\\therdr workspace rename %s forbidden\\n' > ./generated.mk && make -f ./generated.mk", id), true},
		{"default make recipe", fmt.Sprintf("printf 'all:\\n\\therdr workspace rename %s forbidden\\n' > ./Makefile && make", id), true},
		{"generated executable", fmt.Sprintf("printf '#!/bin/sh\\nherdr workspace rename %s forbidden\\n' > ./generated && chmod +x ./generated && ./generated", id), true},
		{"renamed posse build", foreignBinary + " config set defaults.max_workers 9", true},
		{"isolated experiment", fmt.Sprintf("env -u HERDR_SOCKET_PATH -u HERDR_PANE_ID -u HERDR_ENV -u HERDR_BIN_PATH XDG_CONFIG_HOME=%s POSSE_HOME=%s herdr workspace rename %s harmless", filepath.Join(otherRoot, "xdg"), filepath.Join(otherRoot, "posse"), otherWorkspace.Workspace.WorkspaceID), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": tc.command}})
			guard := exec.Command(bin, "_guard")
			guard.Dir = root
			guard.Env = workerEnv
			guard.Stdin = strings.NewReader(string(payload))
			output, err := guard.CombinedOutput()
			blocked := err != nil && guard.ProcessState.ExitCode() == 2
			if blocked != tc.blocked {
				t.Errorf("guard blocked=%v, want %v: %v %s", blocked, tc.blocked, err, output)
			}
			if !blocked {
				shell := exec.Command("bash", "-c", tc.command)
				shell.Dir = root
				shell.Env = workerEnv
				result, err := shell.CombinedOutput()
				if tc.blocked {
					t.Logf("unguarded shell: %v %s", err, result)
				} else if err != nil {
					t.Errorf("isolated shell failed: %v %s", err, result)
				}
			}
			snapshot, err := client.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, ws := range snapshot.Workspaces {
				if ws.WorkspaceID == id && ws.Label != "posse-e2e" {
					t.Errorf("foreign workspace label %q changed by %s", ws.Label, tc.command)
				}
			}
			if !tc.blocked {
				otherSnapshot, err := otherClient.Snapshot(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, ws := range otherSnapshot.Workspaces {
					if ws.WorkspaceID == otherWorkspace.Workspace.WorkspaceID && ws.Label == "harmless" {
						found = true
					}
				}
				if !found {
					t.Error("isolated workspace was not renamed")
				}
			}
		})
	}
}
