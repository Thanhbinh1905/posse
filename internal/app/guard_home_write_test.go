package app

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func homeWriteScope(t *testing.T) guardScope {
	scope, _ := guardFixture(t)
	scope.writable = []string{"/home/u/.posse/pool/posse/sandbox-1", "/home/u/.posse/scratch/posse/t1"}
	return scope
}

func TestGuardRefusesWorkerWritesToTheLivePosseHome(t *testing.T) {
	scope := homeWriteScope(t)
	for _, command := range []string{
		"printf %s /usr/bin > ~/.posse/user-shell-path",
		"echo x >> /home/u/.posse/user-shell-path",
		"echo x &> $HOME/.posse/user-shell-path",
		"cat > ~/.posse/user-shell-path <<'EOF'\n/usr/bin\nEOF",
		"cp backup ~/.posse/user-shell-path",
		"cp -t ~/.posse backup",
		"cp -vt ~/.posse backup",
		"mv -ft ~/.posse backup",
		"install -Dt ~/.posse backup",
		"cp -t/home/u/.posse backup",
		"cp -vt/home/u/.posse backup",
		"cp --target-directory ~/.posse backup",
		"install --target-directory ~/.posse backup",
		"install -m 600 backup ~/.posse/user-shell-path",
		"mv ~/.posse/user-shell-path backup",
		"ln -sf /tmp/x ~/.posse/user-shell-path",
		"echo x | tee -a ~/.posse/user-shell-path",
		"rm -f ~/.posse/state.db",
		"touch ~/.posse/user-shell-path",
		"mkdir -p ~/.posse/projects/lab",
		"chmod 600 ~/.posse/user-shell-path",
		"truncate -s 0 ~/.posse/user-shell-path",
		"dd if=/dev/null of=/home/u/.posse/user-shell-path",
		"sed -i 's/a/b/' ~/.posse/user-shell-path",
		"perl -0pi -e 's/a/b/' ~/.posse/user-shell-path",
		"cd ~/.posse && touch user-shell-path",
		"bash -c 'echo x > ~/.posse/user-shell-path'",
		"{ echo x; } > ~/.posse/user-shell-path",
		"if true; then echo x; fi > ~/.posse/user-shell-path",
		"echo $(echo x > ~/.posse/user-shell-path)",
		"sudo tee ~/.posse/user-shell-path < backup",
		"echo x > ~/.posse/pool/posse/sandbox-2/notes.txt",
		"echo x > ~/.posse/scratch/posse/t2/notes.txt",
		"echo x > ~/.posse/pool/posse/sandbox-1/../sandbox-2/notes.txt",
	} {
		refused, reason := guardCommand(command, scope)
		if !refused || reason.rule != ruleWorkerHomeWrite {
			t.Errorf("guard did not refuse %q by %s: %v %#v", command, ruleWorkerHomeWrite, refused, reason)
			continue
		}
		if !strings.Contains(reason.why, "/home/u/.posse") || !strings.Contains(reason.why, "Sandbox") {
			t.Errorf("refusal of %q does not name the home and the allowed places: %q", command, reason.why)
		}
	}
}

func TestGuardAllowsWorkerWritesOutsideTheLivePosseHome(t *testing.T) {
	scope := homeWriteScope(t)
	for _, command := range []string{
		"cat ~/.posse/user-shell-path",
		"cp ~/.posse/user-shell-path ~/.posse/scratch/posse/t1/backup",
		"cp -vt ~/.posse/scratch/posse/t1 ~/.posse/user-shell-path",
		"cp --target-directory ~/.posse/scratch/posse/t1 ~/.posse/user-shell-path",
		"echo x > ~/.posse/pool/posse/sandbox-1/notes.txt",
		"cd ~/.posse/pool/posse/sandbox-1 && echo x > notes.txt",
		"rm -rf ~/.posse/scratch/posse/t1/lab",
		"sed -n p ~/.posse/user-shell-path",
		"perl -ne print ~/.posse/user-shell-path",
		"echo x 2>&1 >/dev/null",
		"echo x > /tmp/out",
		"LAB=$(mktemp -d); export POSSE_HOME=$LAB/posse; mkdir -p $POSSE_HOME; echo x > $POSSE_HOME/user-shell-path",
		"chmod 600 ./notes.txt",
	} {
		if refused, reason := guardCommand(command, scope); refused {
			t.Errorf("guard refused %q: %#v", command, reason)
		}
	}
}

func TestRunGuardRefusesWorkerFileToolWritesToTheLivePosseHome(t *testing.T) {
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
	t.Setenv(workerHomeEnv, home)
	t.Chdir(root)
	service := testService(home, herdr.NewFake())
	live := filepath.Join(home, "user-shell-path")
	for _, input := range []string{
		`{"tool_name":"Write","tool_input":{"file_path":"` + live + `","content":"/usr/bin"}}`,
		`{"tool_name":"Edit","tool_input":{"file_path":"` + live + `","old_string":"a","new_string":"b"}}`,
		`{"tool_name":"Bash","tool_input":{"command":"printf %s /usr/bin > ` + live + `"}}`,
	} {
		var stderr bytes.Buffer
		if code := service.RunGuard(strings.NewReader(input), &stderr); code != guardExitBlocked || !strings.Contains(stderr.String(), "by guard rule "+ruleWorkerHomeWrite+":") || !strings.Contains(stderr.String(), live) {
			t.Errorf("guard let a Worker write the live posse home with %s: code=%d stderr=%s", input, code, stderr.String())
		}
	}
	for _, input := range []string{
		`{"tool_name":"Read","tool_input":{"file_path":"` + live + `"}}`,
		`{"tool_name":"Write","tool_input":{"file_path":"` + filepath.Join(home, "scratch", "posse", "t1", "notes.txt") + `","content":"x"}}`,
		`{"tool_name":"Write","tool_input":{"file_path":"` + filepath.Join(home, "pool", "posse", "sandbox-1", "notes.txt") + `","content":"x"}}`,
		`{"tool_name":"Write","tool_input":{"file_path":"` + filepath.Join(root, "notes.txt") + `","content":"x"}}`,
	} {
		var stderr bytes.Buffer
		if code := service.RunGuard(strings.NewReader(input), &stderr); code != 0 {
			t.Errorf("guard refused %s: code=%d stderr=%s", input, code, stderr.String())
		}
	}
}

func TestRunGuardLimitsWritesToTheWorkerTaskSandboxScratchAndCandidates(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Fix", MergeMode: "local", PaneID: "w9:p1", PaneLabel: "posse:shop:t1"})
	if err != nil {
		t.Fatal(err)
	}
	sandbox := filepath.Join(home, "pool", "shop", "sandbox-1")
	if err := db.UpdateTaskSandboxPath(ctx, taskID, 0, sandbox); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, entry := range guardEnv {
		name, value, _ := strings.Cut(entry, "=")
		t.Setenv(name, value)
	}
	t.Setenv(workerHomeEnv, home)
	t.Chdir(root)
	service := testService(home, herdr.NewFake())
	write := func(path string) int {
		var stderr bytes.Buffer
		return service.RunGuard(strings.NewReader(`{"tool_name":"Write","tool_input":{"file_path":"`+path+`","content":"x"}}`), &stderr)
	}
	for _, path := range []string{filepath.Join(sandbox, "notes.txt"), filepath.Join(home, "scratch", "shop", "t1", "notes.txt"), filepath.Join(home, "projects", "shop", "tasks", "t1", "candidates", "shop", "g1", "app.yaml")} {
		if code := write(path); code != 0 {
			t.Errorf("guard refused a write in the Worker's own Task place %s: code=%d", path, code)
		}
	}
	for _, path := range []string{filepath.Join(home, "pool", "shop", "sandbox-2", "notes.txt"), filepath.Join(home, "scratch", "shop", "t2", "notes.txt"), filepath.Join(home, "projects", "shop", "tasks", "t2", "candidates", "shop", "g1", "app.yaml"), filepath.Join(home, "projects", "shop", "tasks", "t1", "launch.md")} {
		if code := write(path); code != guardExitBlocked {
			t.Errorf("guard let a Worker write another Task's place %s: code=%d", path, code)
		}
	}
}
