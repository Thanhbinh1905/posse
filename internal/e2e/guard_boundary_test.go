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
	root := shortFixtureRootPath(t, newFixtureRoot(t, herdr.TestRootName()))
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
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if alive, err := fixtureProcessAlive(resolvedRoot); err != nil || !alive {
		t.Fatalf("fixture process scan missed the isolated Herdr server: alive=%v err=%v", alive, err)
	}
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
	otherRoot := shortFixtureRootPath(t, newFixtureRoot(t, herdr.TestRootName()))
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
	source := filepath.Join(root, "internal", "herdr")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "adapter_test.go"), []byte("herdrCall = 'fixture'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "probe"), []byte("print('herdr')\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	resolvedSocket, err := filepath.EvalSymlinks(socket)
	if err != nil {
		t.Fatal(err)
	}
	currentSocketIsDefault := resolvedSocket == filepath.Join(resolvedRoot, "xdg", "herdr", "herdr.sock")
	currentSocketEnv := ""
	currentSocketCase := "default socket"
	if !currentSocketIsDefault {
		currentSocketEnv = "HERDR_SOCKET_PATH=" + shellQuote(socket) + " "
		currentSocketCase = "current socket"
	}
	otherSocketEnv := "HERDR_SOCKET_PATH=" + shellQuote(herdr.SocketPath(otherEnv)) + " "
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
		{currentSocketCase + " via unsets", fmt.Sprintf("env -u HERDR_SOCKET_PATH -u HERDR_PANE_ID -u HERDR_ENV -u HERDR_BIN_PATH %sXDG_CONFIG_HOME=%s herdr workspace rename %s forbidden", currentSocketEnv, filepath.Join(root, "xdg"), id), true},
		{currentSocketCase + " via clean environment", fmt.Sprintf("env -i HOME=%s XDG_CONFIG_HOME=%s PATH=%s %sherdr workspace rename %s forbidden", filepath.Join(root, "home"), filepath.Join(root, "xdg"), os.Getenv("PATH"), currentSocketEnv, id), true},
		{"isolated experiment", fmt.Sprintf("env -u HERDR_SOCKET_PATH -u HERDR_PANE_ID -u HERDR_ENV -u HERDR_BIN_PATH %sXDG_CONFIG_HOME=%s POSSE_HOME=%s herdr workspace rename %s harmless", otherSocketEnv, filepath.Join(otherRoot, "xdg"), filepath.Join(otherRoot, "posse"), otherWorkspace.Workspace.WorkspaceID), false},
		{"isolated dynamic posse home", fmt.Sprintf("R=$(printf %%s %s); env -u HERDR_SOCKET_PATH -u HERDR_PANE_ID -u HERDR_ENV -u HERDR_BIN_PATH %sXDG_CONFIG_HOME=%s POSSE_HOME=$R/posse herdr workspace rename %s harmless", otherRoot, otherSocketEnv, filepath.Join(otherRoot, "xdg"), otherWorkspace.Workspace.WorkspaceID), false},
		{"isolated clean environment", fmt.Sprintf("env -i HOME=%s XDG_CONFIG_HOME=%s POSSE_HOME=%s PATH=%s %sherdr workspace rename %s harmless", filepath.Join(otherRoot, "home"), filepath.Join(otherRoot, "xdg"), filepath.Join(otherRoot, "posse"), os.Getenv("PATH"), otherSocketEnv, otherWorkspace.Workspace.WorkspaceID), false},
		{"unverifiable dynamic environment", fmt.Sprintf("R=$(printf %%s %s); HERDR_SOCKET_PATH=$R/herdr.sock herdr workspace rename %s forbidden", otherRoot, id), true},
		{"python path read", `python3 -c "from pathlib import Path; print(Path('internal/herdr/adapter_test.go').read_text())"`, false},
		{"python version query with Herdr data", `python3 -c "import sys; print('herdr runtime:', sys.version)"`, false},
		{"rg search with Herdr data", "rg -n herdrCall internal/herdr/adapter_test.go", false},
		{"python socket access", `python3 -c 'import socket, os; socket.socket(socket.AF_UNIX).connect(os.environ["HERDR_SOCKET_PATH"])'`, true},
		{"python Herdr subprocess", fmt.Sprintf(`python3 -c 'import subprocess; subprocess.run(["herdr", "workspace", "rename", "%s", "forbidden"])'`, id), true},
		{"python interactive stdin", fmt.Sprintf("python3 -i -c \"print('herdr')\" <<'PY'\nimport subprocess\nsubprocess.run(['herdr', 'workspace', 'rename', '%s', 'changed'], check=True)\nPY", id), true},
		{"python option operand before command", fmt.Sprintf(`python3 -X probe -c "import subprocess; subprocess.run(['herdr', 'workspace', 'rename', '%s', 'changed'], check=True)"`, id), true},
		{"python inspect environment with stdin", fmt.Sprintf("PYTHONINSPECT=1 python3 -c \"print('herdr')\" <<'PY'\nimport subprocess\nsubprocess.run(['herdr', 'workspace', 'rename', '%s', 'changed'], check=True)\nPY", id), true},
		{"python inspect environment", "PYTHONINSPECT=1 python3 -c \"print('herdr')\"", true},
		{"python redirected stdin", "python3 -c \"print('herdr')\" <<'PY'\nprint('post-command input')\nPY", false},
		{"python heredoc edit of Go source naming herdr", "python3 - <<'PY'\nfrom pathlib import Path\np = Path('internal/herdr/adapter_test.go')\np.write_text(p.read_text() + 'import \"github.com/thanhbinh1905/posse/internal/herdr\"\\nvar _ = herdr.Snapshot{}\\n')\nPY", false},
		{"python heredoc running herdr", fmt.Sprintf("python3 - <<'PY'\nimport subprocess\nsubprocess.run(['herdr', 'workspace', 'rename', '%s', 'forbidden'])\nPY", id), true},
		{"direct Worker push", "git push origin HEAD", true},
		{"command probe", "command -v herdr", false},
		{"which probe", "which herdr", false},
		{"type probe", "type herdr", false},
		{"version probe", "herdr --version", false},
		{"help probe", "herdr --help", false},
		{"plain path mention", "printf '%s\\n' /tmp/herdr-linux-x86_64", false},
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
			if tc.name == "unverifiable dynamic environment" && !strings.Contains(string(output), "cannot tell which Herdr server it reaches") {
				t.Errorf("dynamic environment refusal did not explain the unknown target: %s", output)
			}
			if tc.blocked && strings.HasPrefix(tc.name, "default socket") && (!strings.Contains(string(output), "default socket reaches the User's Herdr server") || !strings.Contains(string(output), "env -i HOME=/tmp/posse-e2e-lab/home XDG_CONFIG_HOME=/tmp/posse-e2e-lab/xdg POSSE_HOME=/tmp/posse-e2e-lab/posse")) {
				t.Errorf("refusal omitted the default-socket reason or accepted isolation command: %s", output)
			}
			if tc.blocked && strings.HasPrefix(tc.name, "current socket") && !strings.Contains(string(output), "it changes the Herdr session this Worker runs in") {
				t.Errorf("refusal did not identify the current isolated Herdr socket: %s", output)
			}
			runShell := true
			if tc.name == "rg search with Herdr data" {
				if _, err := exec.LookPath("rg"); err != nil {
					runShell = false
					t.Log("rg is not installed; guard acceptance was checked without executing the search")
				}
			}
			if !blocked && runShell {
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
			if tc.name == "python heredoc edit of Go source naming herdr" {
				edited, err := os.ReadFile(filepath.Join(source, "adapter_test.go"))
				if err != nil || !strings.Contains(string(edited), "var _ = herdr.Snapshot{}") {
					t.Errorf("allowed edit did not change the Go source: %v %q", err, edited)
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
			if strings.HasPrefix(tc.name, "isolated ") {
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
