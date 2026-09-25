package app

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// guardEnv mirrors a Worker pane: Herdr sets HERDR_SOCKET_PATH to the default
// socket, so dropping the variable still reaches the same server.
var guardEnv = []string{"HOME=/home/u", "HERDR_SOCKET_PATH=/home/u/.config/herdr/herdr.sock", "HERDR_PANE_ID=w9:p1", "HERDR_ENV=1", "HERDR_BIN_PATH=/home/u/.local/bin/herdr", "PATH=/usr/bin"}

func TestGuardRefusesHerdrChangesToTheWorkerSession(t *testing.T) {
	for _, command := range []string{
		"herdr workspace close wGN --group",
		"herdr workspace close w1",
		"herdr tab close w1:t1",
		"herdr pane close w1:p1",
		"herdr pane send-text w1:p1 'hello'",
		"herdr pane run w1:p1 'printf SHELL_READY'",
		"herdr agent prompt posse-lead 'hi'",
		"herdr worktree open --workspace w1 --path /tmp/m",
		"herdr server stop",
		"herdr",
		"cd /tmp && herdr workspace create --cwd /tmp",
		"/home/u/.local/bin/herdr workspace close w1",
		"unset HERDR_SOCKET_PATH; herdr workspace close w1",
		"env -u HERDR_SOCKET_PATH herdr workspace close w1",
		"XDG_CONFIG_HOME=/tmp/x herdr workspace close w1",
		"bash -c 'herdr workspace close w1'",
		`sh -lc "herdr tab close w1:t1"`,
		"eval herdr workspace close w1",
		"echo $(herdr workspace close w1)",
		"herdr $SUBCOMMAND w1",
		"HERDR_SOCKET_PATH=$(mktemp) herdr workspace close w1",
		"herdr --session other workspace close w1",
		"command herdr pane close w1:p1",
		"nohup herdr pane close w1:p1 &",
	} {
		if refused, reason := guardHerdrCommand(command, guardEnv); !refused {
			t.Errorf("guard allowed %q", command)
		} else if reason.why == "" || reason.command == "" {
			t.Errorf("guard gave no reason for %q: %#v", command, reason)
		}
	}
}

func TestGuardAllowsReadsAndIsolatedHerdrServers(t *testing.T) {
	for _, command := range []string{
		"herdr workspace list",
		"herdr pane read w1:p1 --source recent",
		"herdr api snapshot",
		"herdr api schema --json",
		"herdr status",
		"herdr --help",
		"herdr workspace close --help",
		"herdr --version",
		"grep -rn herdr internal",
		"go test ./internal/e2e/...",
		"echo herdr-agent-state",
		"env -u HERDR_SOCKET_PATH XDG_CONFIG_HOME=/tmp/posse-e2e-x/xdg herdr workspace close w1",
		"R=/tmp/posse-e2e-x; env -u HERDR_SOCKET_PATH XDG_CONFIG_HOME=$R/xdg herdr server stop",
		`export HERDR_SOCKET_PATH=/tmp/posse-e2e-x/herdr.sock; herdr workspace close w1`,
		"HERDR_SOCKET_PATH=/tmp/posse-e2e-x/herdr.sock herdr pane close w1:p1",
		"env -i HOME=/tmp/posse-e2e-x/home herdr server",
		"if true; then export HERDR_SOCKET_PATH=/tmp/posse-e2e-x/herdr.sock; else export HERDR_SOCKET_PATH=/tmp/posse-e2e-x/herdr.sock; fi; herdr workspace close w1",
	} {
		if refused, reason := guardHerdrCommand(command, guardEnv); refused {
			t.Errorf("guard refused %q: %#v", command, reason)
		}
	}
}

func TestGuardConservativeAcrossBranchesAndRuntimeFiles(t *testing.T) {
	for _, command := range []string{
		"if false; then export HERDR_SOCKET_PATH=/tmp/iso.sock; fi; herdr workspace close w1 --group",
		"if true; then export HERDR_SOCKET_PATH=/tmp/iso.sock; else HERDR_SOCKET_PATH=/tmp/other.sock; fi; herdr workspace close w1",
		"if false; then HERDR_SOCKET_PATH=/tmp/iso.sock; fi; bash -c 'herdr workspace close w1'",
		"HERDR_SOCKET_PATH=/tmp/iso.sock; if true; then export HERDR_SOCKET_PATH=/home/u/.config/herdr/herdr.sock; fi; herdr workspace close w1",
		"f() { export HERDR_SOCKET_PATH=/tmp/iso.sock; }; herdr workspace close w1",
		"while false; do export HERDR_SOCKET_PATH=/tmp/iso.sock; done; herdr workspace close w1",
		"case no in yes) export HERDR_SOCKET_PATH=/tmp/iso.sock;; esac; herdr workspace close w1",
		"herdr workspace rename w1 -- --help",
		"printf 'herdr workspace close w1\\n' > ./new.sh && bash ./new.sh",
		"printf 'herdr workspace close w1\\n' > ./new.sh && chmod +x ./new.sh && ./new.sh",
		"printf 'all:\\n\\therdr workspace close w1\\n' > ./new.mk && make -f ./new.mk",
	} {
		if refused, _ := guardHerdrCommand(command, guardEnv); !refused {
			t.Errorf("allowed %q", command)
		}
	}
}

func TestGuardRecognizesSocketHardlinks(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "session.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	alias := filepath.Join(dir, "alias.sock")
	if err := os.Link(socket, alias); err != nil {
		t.Fatal(err)
	}
	env := append(append([]string{}, guardEnv...), "HERDR_SOCKET_PATH="+socket)
	for _, command := range []string{
		"HERDR_SOCKET_PATH=" + alias + " herdr workspace rename w1 changed",
		"ln -f $HERDR_SOCKET_PATH ./new.sock && HERDR_SOCKET_PATH=./new.sock herdr workspace rename w1 changed",
	} {
		if refused, _ := guardHerdrCommand(command, env); !refused {
			t.Errorf("allowed %q", command)
		}
	}
}

func TestRunGuardBlocksOnlyWorkers(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, entry := range guardEnv {
		name, value, _ := strings.Cut(entry, "=")
		t.Setenv(name, value)
	}
	t.Chdir(root)
	service := testService(home, herdr.NewFake())
	input := `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"herdr workspace close wGN --group"}}`

	t.Setenv(workerHomeEnv, "")
	var stderr bytes.Buffer
	if code := service.RunGuard(strings.NewReader(input), &stderr); code != 0 {
		t.Fatalf("guard blocked a caller that is not a Worker: code=%d stderr=%s", code, stderr.String())
	}

	t.Setenv(workerHomeEnv, home)
	stderr.Reset()
	if code := service.RunGuard(strings.NewReader(input), &stderr); code != guardExitBlocked || !strings.Contains(stderr.String(), "isolated Herdr server") {
		t.Fatalf("guard let a Worker close workspaces: code=%d stderr=%s", code, stderr.String())
	}
	stderr.Reset()
	argv := `{"tool_name":"Bash","tool_input":{"command":["herdr","pane","close","w1:p1"]}}`
	if code := service.RunGuard(strings.NewReader(argv), &stderr); code != guardExitBlocked {
		t.Fatalf("guard missed an argv command: code=%d stderr=%s", code, stderr.String())
	}
	stderr.Reset()
	read := `{"tool_name":"Bash","tool_input":{"command":"herdr workspace list"}}`
	if code := service.RunGuard(strings.NewReader(read), &stderr); code != 0 {
		t.Fatalf("guard blocked a read: code=%d stderr=%s", code, stderr.String())
	}
	if code := service.RunGuard(strings.NewReader(`{"tool_name":"Edit","tool_input":{"file_path":"x"}}`), &stderr); code != 0 {
		t.Fatalf("guard blocked a tool without a command: code=%d", code)
	}
}

// guardFixture writes the files indirection cases refer to and returns a
// scope for a Worker of workerHome whose installed posse is posseBinary.
func guardFixture(t *testing.T) (guardScope, string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"close.sh":        "#!/bin/sh\nherdr workspace close w1 --group\n",
		"plain.sh":        "#!/bin/sh\necho hello\n",
		"nested.sh":       "sh ./close.sh\n",
		"raw.py":          "import json, os, socket\ns = socket.socket(socket.AF_UNIX)\ns.connect(os.environ['HERDR_SOCKET_PATH'])\ns.sendall(json.dumps({'id': 'x', 'method': 'workspace.rename'}).encode())\n",
		"literal.py":      "import socket\nsocket.socket(socket.AF_UNIX).connect('/home/u/.config/herdr/herdr.sock')\n",
		"harmless.py":     "print('hello')\n",
		"close-script.py": "#!/usr/bin/env python3\nimport subprocess\nsubprocess.run(['herdr', 'pane', 'close', 'w1:p1'])\n",
		"posse-copy":      "\x7fELF",
		"posse":           "\x7fELF",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return guardScope{env: append(append([]string(nil), guardEnv...), "PATH="+dir+":/usr/bin"), cwd: dir, workerHome: "/home/u/.posse", posse: filepath.Join(dir, "posse")}, dir
}

func TestGuardFollowsIndirection(t *testing.T) {
	scope, dir := guardFixture(t)
	for _, command := range []string{
		"sh close.sh",
		"bash ./close.sh",
		"./close.sh",
		dir + "/close.sh",
		"source close.sh",
		". ./close.sh",
		"sh nested.sh",
		"cd / && sh " + dir + "/close.sh",
		"cd " + dir + " && sh close.sh",
		"sh <<'EOF'\nherdr workspace close w1\nEOF",
		"bash -s <<< 'herdr pane close w1:p1'",
		"echo 'herdr workspace close w1' | sh",
		"$(which herdr) workspace close w1",
		"`command -v herdr` workspace close w1",
		"$HERDR_BIN_PATH workspace close w1",
		"xargs herdr workspace close <<< w1",
		`find . -maxdepth 0 -exec herdr workspace close w1 \;`,
		"timeout 5 herdr pane close w1:p1",
		"sudo -u u herdr pane close w1:p1",
		"setsid herdr pane close w1:p1",
		"env -S 'herdr pane close w1:p1'",
		"env -C / sh " + dir + "/close.sh",
		"python3 raw.py",
		"python3 literal.py",
		"./close-script.py",
		`python3 -c 'import subprocess; subprocess.run(["herdr", "workspace", "rename", "w1", "x"])'`,
		"python3 - <<'EOF'\nimport os\nprint(os.environ['HERDR_SOCKET_PATH'])\nEOF",
		`node -e 'require("child_process").execSync("herdr pane close w1:p1")'`,
		`perl -e 'system("herdr pane close w1:p1")'`,
		`ruby -e 'system("herdr pane close w1:p1")'`,
		"curl --unix-socket /home/u/.config/herdr/herdr.sock http://x/",
		"socat - UNIX-CONNECT:$HERDR_SOCKET_PATH",
		"nc -U ~/.config/herdr/herdr.sock",
		"HERDR_SOCKET_PATH=/tmp/iso.sock python3 literal.py",
		"./posse-copy config set defaults.auto_unsaddle never",
		dir + "/posse-copy ride --brief b.md",
		"env -u POSSE_WORKER_HOME -u HERDR_PANE_ID sh -c './posse-copy config set defaults.auto_unsaddle never'",
		"go run ./cmd/posse config set defaults.auto_unsaddle never",
		"POSSE_HOME=/home/u/.posse ./posse-copy roster",
	} {
		if refused, reason := guardCommand(command, scope); !refused {
			t.Errorf("guard allowed %q", command)
		} else if reason.why == "" {
			t.Errorf("guard gave no reason for %q", command)
		}
	}
}

func TestGuardAllowsIndirectionThatStaysIsolated(t *testing.T) {
	scope, dir := guardFixture(t)
	for _, command := range []string{
		"sh plain.sh",
		"./plain.sh",
		"python3 harmless.py",
		`python3 -c 'print(1)'`,
		"HERDR_SOCKET_PATH=/tmp/posse-e2e-x/herdr.sock python3 raw.py",
		"env -u HERDR_SOCKET_PATH XDG_CONFIG_HOME=/tmp/posse-e2e-x/xdg sh close.sh",
		"echo hi | sh",
		"posse holler working 'still going'",
		dir + "/posse brief",
		"POSSE_HOME=/tmp/posse-e2e-x/posse ./posse-copy ride --brief b.md",
		"go test ./internal/e2e/...",
		"curl https://example.com",
		"find . -name '*.go' -exec grep -l herdr {} +",
	} {
		if refused, reason := guardCommand(command, scope); refused {
			t.Errorf("guard refused %q: %#v", command, reason)
		}
	}
}
