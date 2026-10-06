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
		"herdr workspace close --group wGN",
		"herdr workspace close --group=true wGN",
		"herdr workspace close wGN -g",
		"herdr workspace close w1",
		`herdr workspace close "$HERDR_WORKSPACE_ID" --group`,
		`herdr api call workspace.close '{"workspace_id":"w1","close_group":true}'`,
		`herdr api call worktree.remove '{"workspace_id":"w2","force":true}'`,
		"herdr worktree remove w2 --force",
		"herdr worktree create --workspace w1 --branch erase",
		"herdr worktree open --workspace w1 --path /tmp/m",
		"env -u HERDR_SOCKET_PATH herdr worktree remove w2 --force",
		"bash -c 'herdr workspace close --group wGN'",
		"herdr tab close w1:t1",
		// Riders share the Lead's workspace: their own ids reach the Lead and every sibling.
		`herdr tab close "$HERDR_TAB_ID"`,
		`herdr workspace close "$HERDR_WORKSPACE_ID"`,
		"herdr tab move w1:t2 --index 0",
		"herdr tab focus w1:t1",
		"herdr tab rename w1:t1 lead",
		"herdr pane move w1:p2 --new-workspace",
		"herdr pane close w1:p1",
		"herdr pane send-text w1:p1 'hello'",
		"herdr pane run w1:p1 'printf SHELL_READY'",
		"herdr agent prompt posse-lead 'hi'",
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
		"command -v herdr workspace close w1",
		"command -V herdr workspace close w1 --group",
		"command -v herdr; herdr workspace close w1 --group",
		"command -v $(herdr workspace close w1)",
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
		"herdr worktree list --workspace w1",
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
		"env -i HOME=/tmp/posse-e2e-lab/home XDG_CONFIG_HOME=/tmp/posse-e2e-lab/xdg POSSE_HOME=/tmp/posse-e2e-lab/posse PATH=/usr/local/bin:/usr/bin:/bin herdr server",
		"if true; then export HERDR_SOCKET_PATH=/tmp/posse-e2e-x/herdr.sock; else export HERDR_SOCKET_PATH=/tmp/posse-e2e-x/herdr.sock; fi; herdr workspace close w1",
	} {
		if refused, reason := guardHerdrCommand(command, guardEnv); refused {
			t.Errorf("guard refused %q: %#v", command, reason)
		}
	}
}

func TestGuardAllowsProvenPythonInspections(t *testing.T) {
	scope, _ := guardFixture(t)
	for _, command := range []string{
		`python3 -c "from pathlib import Path; print(Path('internal/herdr/adapter_test.go').read_text())"`,
		`python3 -c "print('herdr')"`,
		`python3 -c "import sys; print('herdr runtime:', sys.version)"`,
		`python3 -c "import importlib.metadata; print(importlib.metadata.version('herdr'))"`,
	} {
		if refused, reason := guardCommand(command, scope); refused {
			t.Errorf("guard refused proven read-only Python inspection %q: %#v", command, reason)
		}
	}
}

func TestGuardKeepsOpaquePythonHerdrAccessBlocked(t *testing.T) {
	scope, _ := guardFixture(t)
	for _, command := range []string{
		`python3 -c "from pathlib import Path; print(Path('internal/herdr/adapter_test.go').read_text()); import subprocess; subprocess.run(['herdr'])"`,
		`python3 -c "import socket, os; socket.socket(socket.AF_UNIX).connect(os.environ['HERDR_SOCKET_PATH'])"`,
		`python3 -c "print('herdr'); __import__('os').system('herdr workspace close w1')"`,
	} {
		if refused, reason := guardCommand(command, scope); !refused || reason.why == "" {
			t.Errorf("guard did not conservatively refuse opaque Python command %q: %#v", command, reason)
		}
	}
}

func TestGuardExplainsUnknownDynamicHerdrTarget(t *testing.T) {
	scope, _ := guardFixture(t)
	command := `R=$(printf %s /tmp/isolated); HERDR_SOCKET_PATH=$R/herdr.sock herdr workspace close w1`
	if refused, reason := guardCommand(command, scope); !refused || !strings.Contains(reason.why, "cannot tell which Herdr server it reaches") {
		t.Fatalf("guard did not explain the unverified dynamic Herdr target: refused=%v reason=%#v", refused, reason)
	}
}

func TestGuardAllowsReadOnlyHerdrProbesAndPlainMentions(t *testing.T) {
	for _, command := range []string{
		"command -v herdr",
		"command -V herdr",
		"builtin command -v herdr",
		"which herdr",
		"type herdr",
		"herdr --version",
		"herdr --help",
		"printf '%s\\n' /tmp/herdr-linux-x86_64",
		"curl -fsSL https://example.test/herdr-linux-x86_64 -o /tmp/herdr-linux-x86_64",
	} {
		if refused, reason := guardHerdrCommand(command, guardEnv); refused {
			t.Errorf("guard refused harmless probe %q: %#v", command, reason)
		}
	}
}

func TestGuardAllowsDownloadingAndRunningAnIsolatedHerdrBinary(t *testing.T) {
	commands := []string{
		`tool_dir="/tmp/posse-tools/herdr"; mkdir -p "$tool_dir"; herdr="$tool_dir/herdr"; curl -fsSL https://github.com/herdrdev/herdr/releases/download/v0.9.1/herdr-linux-x86_64 -o "$herdr"; chmod 755 "$herdr"; "$herdr" --version`,
		`tool_dir="/tmp/posse-tools/herdr"; mkdir -p "$tool_dir"; herdr="$tool_dir/herdr"; gh release download v0.9.1 --repo herdrdev/herdr --pattern herdr-linux-x86_64 --dir "$tool_dir"; chmod +x "$herdr"`,
		`cp /tmp/source/herdr /tmp/dest/herdr; chmod +x /tmp/dest/herdr`,
		`curl --unix-socket /tmp/posse-e2e-x/herdr.sock http://example.test/`,
	}
	for _, command := range commands {
		if refused, reason := guardHerdrCommand(command, guardEnv); refused {
			t.Errorf("guard refused download or isolated run %q: %#v", command, reason)
		}
	}

	root, err := os.MkdirTemp(os.TempDir(), "posse-e2e-t91-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	isolatedEnv := herdr.IsolatedTestEnvironment(root)
	if _, err := herdr.ValidateIsolatedEnvironment(isolatedEnv); err != nil {
		t.Fatalf("test isolation fixture is invalid: %v", err)
	}
	command := `herdr="/tmp/posse-tools/herdr"; env -i ` + strings.Join(quoteArgv(isolatedEnv), " ") + ` "$herdr" workspace close w1`
	if refused, reason := guardHerdrCommand(command, guardEnv); refused {
		t.Errorf("guard refused a downloaded binary targeting an isolated server: %#v", reason)
	}

	command = `herdr="/tmp/posse-tools/herdr"; "$herdr" workspace close w1`
	if refused, reason := guardHerdrCommand(command, guardEnv); !refused {
		t.Errorf("guard allowed downloaded binary to reach the User's session: %#v", reason)
	}
}

func TestGuardEnvUnsetsAndCleanEnvironmentReachSameSocket(t *testing.T) {
	for _, tc := range []struct {
		name, configHome string
		blocked          bool
	}{
		{"default socket", "/home/u/.config", true},
		{"isolated socket", "/tmp/posse-e2e-x/xdg", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commands := []string{
				"env -u HERDR_SOCKET_PATH -u HERDR_PANE_ID -u HERDR_ENV -u HERDR_BIN_PATH XDG_CONFIG_HOME=" + tc.configHome + " herdr workspace close w1",
				"env -i HOME=/home/u XDG_CONFIG_HOME=" + tc.configHome + " PATH=/usr/bin herdr workspace close w1",
				"R=$(mktemp -d); env -u HERDR_SOCKET_PATH -u HERDR_PANE_ID -u HERDR_ENV -u HERDR_BIN_PATH XDG_CONFIG_HOME=" + tc.configHome + " POSSE_HOME=$R/posse HOME=$R/home herdr workspace close w1",
			}
			for _, command := range commands {
				blocked, reason := guardHerdrCommand(command, guardEnv)
				if blocked != tc.blocked {
					t.Errorf("guard %q blocked=%v, want %v: %#v", command, blocked, tc.blocked, reason)
				}
			}
		})
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
		"curl --unix-socket " + alias + " http://example.test/",
		"ln -f $HERDR_SOCKET_PATH ./new.sock && HERDR_SOCKET_PATH=./new.sock herdr workspace rename w1 changed",
	} {
		if refused, _ := guardHerdrCommand(command, env); !refused {
			t.Errorf("allowed %q", command)
		}
	}
}

func TestRiderGuardBlocksDirectGitPush(t *testing.T) {
	scope, _ := guardFixture(t)
	for _, command := range []string{"git push origin HEAD", "env git -C repo push origin main", "sh -c 'git push'"} {
		if refused, reason := guardCommand(command, scope); !refused || !strings.Contains(reason.why, "posse publish") {
			t.Errorf("direct Rider push allowed: %q (%v, %#v)", command, refused, reason)
		}
	}
	if refused, reason := guardCommand("git status --short", scope); refused {
		t.Fatalf("Rider read blocked: %#v", reason)
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
