package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestMain(m *testing.M) {
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(key, "HERDR_") {
			_ = os.Unsetenv(key)
		}
	}
	_ = os.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// A Service without an explicit Home must never reach the real posse home,
	// and a Worker running these tests must not carry its own Worker marker in.
	_ = os.Unsetenv(workerHomeEnv)
	home, err := os.MkdirTemp("", "posse-app-test-home-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("POSSE_HOME", home)
	codexHome, err := os.MkdirTemp("", "posse-app-test-codex-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("CODEX_HOME", codexHome)
	code := m.Run()
	_ = os.RemoveAll(home)
	_ = os.RemoveAll(codexHome)
	os.Exit(code)
}

func initRepo(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "init", "-b", "main")
	gitTest(t, root, "config", "user.name", "Posse Test")
	gitTest(t, root, "config", "user.email", "posse@example.test")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "add", "README.md")
	gitTest(t, root, "commit", "-m", "initial")
}

func gitTest(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	command.Env = []string{}
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found && (key == "GIT_CONFIG_GLOBAL" || key == "GIT_CONFIG_NOSYSTEM") {
			continue
		}
		command.Env = append(command.Env, entry)
	}
	command.Env = append(command.Env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func testService(home string, adapter herdr.Adapter) *Service {
	service := New(home, adapter)
	service.herdrContext = hasHerdrEnvironment
	return service
}
