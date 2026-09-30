//go:build e2e

package e2e

import (
	"debug/buildinfo"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDownDoesNotSignalUnrelatedExecutableNamedLookout(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()

	source := filepath.Join(f.root, "unrelated.go")
	helper := filepath.Join(f.root, "bin", "unrelated")
	ready := filepath.Join(f.root, "unrelated.ready")
	code := `package main
import ("os"; "time")
func main() {
	_ = os.WriteFile(os.Getenv("UNRELATED_READY"), []byte("ready"), 0600)
	for { time.Sleep(time.Hour) }
}`
	if err := os.WriteFile(source, []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", helper, source)
	build.Dir, build.Env = f.repo, f.env
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build unrelated process: %v\n%s", err, output)
	}

	unrelated := exec.Command(helper, "lookout")
	unrelated.Dir = f.repo
	unrelated.Env = append(f.env, "UNRELATED_READY="+ready)
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = unrelated.Process.Kill()
		_ = unrelated.Wait()
	}()
	if !waitForCondition(5*time.Second, func() bool {
		_, err := os.Stat(ready)
		return err == nil
	}) {
		t.Fatal("unrelated process did not start")
	}

	renamed := filepath.Join(f.root, "bin", "renamed-posse")
	data, err := os.ReadFile(f.binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(renamed, data, 0o700); err != nil {
		t.Fatal(err)
	}
	lookout := exec.Command(renamed, "lookout", "--timeout", "30000")
	lookout.Dir, lookout.Env = f.repo, f.env
	if err := lookout.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = lookout.Process.Kill()
		_ = lookout.Wait()
	}()
	if !waitForCondition(5*time.Second, func() bool { return processIsAlive(lookout.Process.Pid) }) {
		t.Fatal("renamed Posse Lookout did not remain running")
	}

	command := exec.Command(f.binary, "down")
	command.Dir, command.Env = f.repo, f.env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("posse down: %v\n%s", err, output)
	}
	if !processIsAlive(unrelated.Process.Pid) {
		t.Fatalf("posse down signaled unrelated executable %q lookout\n%s", helper, output)
	}
	if processIsAlive(lookout.Process.Pid) {
		t.Fatalf("posse down failed to stop renamed Posse executable %q\n%s", renamed, output)
	}
	_ = lookout.Wait()
}

func TestDownDoesNotSignalDifferentMainPackageInPosseModule(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()

	module := moduleRoot(t)
	helperDir, err := os.MkdirTemp(filepath.Join(module, "cmd"), "review-unrelated-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(helperDir)
	source := `package main
import ("os"; "time")
func main() {
	_ = os.WriteFile(os.Getenv("UNRELATED_READY"), []byte("ready"), 0600)
	for { time.Sleep(time.Hour) }
}
`
	if err := os.WriteFile(filepath.Join(helperDir, "main.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(module, helperDir)
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(f.root, "bin", "other-command")
	compile := exec.Command("go", "build", "-o", helper, "./"+filepath.ToSlash(relative))
	compile.Dir, compile.Env = module, f.env
	if output, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("build unrelated main package: %v\n%s", err, output)
	}
	info, err := buildinfo.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	if info.Main.Path != "github.com/thanhbinh1905/posse" || info.Path == "github.com/thanhbinh1905/posse/cmd/posse" {
		t.Fatalf("unrelated helper build identity = Main.Path %q, Path %q", info.Main.Path, info.Path)
	}
	ready := filepath.Join(f.root, "other-command.ready")
	unrelated := exec.Command(helper, "lookout")
	unrelated.Dir, unrelated.Env = f.repo, append(f.env, "UNRELATED_READY="+ready)
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = unrelated.Process.Kill()
		_ = unrelated.Wait()
	}()
	if !waitForCondition(5*time.Second, func() bool { _, err := os.Stat(ready); return err == nil }) {
		t.Fatal("unrelated command did not start")
	}

	command := exec.Command(f.binary, "down")
	command.Dir, command.Env = f.repo, f.env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("posse down: %v\n%s", err, output)
	}
	if !processIsAlive(unrelated.Process.Pid) {
		t.Fatalf("posse down signaled another main package from the Posse module (Path=%q)\n%s", info.Path, output)
	}
}

func processIsAlive(pid int) bool {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return false
	}
	fields := strings.Fields(string(stat[end+1:]))
	return len(fields) > 0 && fields[0] != "Z" && fields[0] != "X"
}
