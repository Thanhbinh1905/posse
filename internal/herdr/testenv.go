package herdr

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// IsolatedTestEnvironment strips every inherited HERDR_* value and redirects all
// persisted state to a short disposable test root.
func IsolatedTestEnvironment(root string) []string {
	values := make([]string, 0, len(os.Environ())+8)
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found && strings.HasPrefix(key, "HERDR_") {
			continue
		}
		if found && (key == "HOME" || key == "POSSE_HOME" || key == "CLAUDE_CONFIG_DIR" || key == "CODEX_HOME" || key == "PI_CODING_AGENT_DIR" || key == "XDG_CONFIG_HOME" || key == "POSSE_TEST_HERDR" || key == "GIT_CONFIG_GLOBAL" || key == "GIT_CONFIG_NOSYSTEM" || key == "LANG" || key == "LC_ALL" || key == "TERM" || key == "SHELL") {
			continue
		}
		values = append(values, entry)
	}
	return append(values,
		"HOME="+filepath.Join(root, "home"),
		"XDG_CONFIG_HOME="+filepath.Join(root, "xdg"),
		"POSSE_HOME="+filepath.Join(root, "posse"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(root, "claude"),
		"CODEX_HOME="+filepath.Join(root, "codex"),
		"PI_CODING_AGENT_DIR="+filepath.Join(root, "pi"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"POSSE_TEST_HERDR=1",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"TERM=xterm-256color",
		// script(1) runs the E2E desktop client through SHELL. With /bin/sh
		// (or no SHELL), its pseudo-terminal captures no sidebar frames.
		"SHELL=/bin/bash",
	)
}

//go:embed isolated_update_config.toml
var isolatedUpdateConfig string

func WriteIsolatedConfig(root string) (string, error) {
	return WriteIsolatedConfigWithShell(root, "/bin/sh")
}

func WriteIsolatedConfigWithShell(root, shell string) (string, error) {
	configDir := filepath.Join(root, "xdg", "herdr")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return "", err
	}
	configPath := filepath.Join(configDir, "config.toml")
	contents := "[terminal]\ndefault_shell = " + strconv.Quote(shell) + "\n\n[worktrees]\ndirectory = " + strconv.Quote(filepath.Join(root, "worktrees")) + "\n\n" + strings.TrimSpace(isolatedUpdateConfig) + "\n"
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		return "", err
	}
	return configPath, nil
}

func ValidateIsolatedEnvironment(values []string) (string, error) {
	env := environmentMap(values)
	root := env["POSSE_TEST_ROOT"]
	if root == "" {
		configHome := env["XDG_CONFIG_HOME"]
		if filepath.Base(configHome) != "xdg" {
			return "", &Error{Code: "unsafe_test_environment", Message: "isolated Herdr test requires XDG_CONFIG_HOME under a posse test root"}
		}
		root = filepath.Dir(configHome)
	}
	rootParent := filepath.Dir(root)
	scratchParent := filepath.Dir(rootParent) == "/tmp" && strings.HasPrefix(filepath.Base(rootParent), "posse-") && strings.HasSuffix(filepath.Base(rootParent), "-scratch")
	if env["POSSE_TEST_HERDR"] != "1" || !strings.HasPrefix(filepath.Base(root), "posse-e2e-") || (rootParent != "/tmp" && !scratchParent) || !filepath.IsAbs(root) {
		return "", &Error{Code: "unsafe_test_environment", Message: "isolated Herdr mutation requires a /tmp/posse-e2e-* test root"}
	}
	for key := range env {
		if strings.HasPrefix(key, "HERDR_") {
			return "", &Error{Code: "unsafe_test_environment", Message: fmt.Sprintf("isolated Herdr environment retained %s", key)}
		}
	}
	if !inside(root, env["HOME"]) || !inside(root, env["XDG_CONFIG_HOME"]) || !inside(root, env["POSSE_HOME"]) || !inside(root, env["CLAUDE_CONFIG_DIR"]) || !inside(root, env["CODEX_HOME"]) || !inside(root, env["PI_CODING_AGENT_DIR"]) {
		return "", &Error{Code: "unsafe_test_environment", Message: "isolated state paths must stay inside the test root"}
	}
	if env["GIT_CONFIG_GLOBAL"] != "/dev/null" || env["GIT_CONFIG_NOSYSTEM"] != "1" {
		return "", &Error{Code: "unsafe_test_environment", Message: "Git global and system config must be disabled in tests"}
	}
	configPath := filepath.Join(env["XDG_CONFIG_HOME"], "herdr", "config.toml")
	config, err := os.ReadFile(configPath)
	configuredShell := isolatedDefaultShell(string(config))
	validShell := configuredShell == "/bin/sh"
	if !validShell && inside(root, configuredShell) {
		if info, statErr := os.Lstat(configuredShell); statErr == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			validShell = true
		}
	}
	if err != nil || !validShell || !strings.Contains(string(config), strconv.Quote(filepath.Join(root, "worktrees"))) {
		return "", &Error{Code: "unsafe_test_environment", Message: "isolated Herdr config must use /bin/sh or an executable shell inside the test root, and a test-root worktrees directory", Cause: err}
	}
	if !strings.Contains(string(config), strings.TrimSpace(isolatedUpdateConfig)) {
		return "", &Error{Code: "unsafe_test_environment", Message: "isolated Herdr config must disable background update network checks"}
	}
	return root, nil
}

func isolatedDefaultShell(config string) string {
	for _, line := range strings.Split(config, "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(key) != "default_shell" {
			continue
		}
		shell, err := strconv.Unquote(strings.TrimSpace(value))
		if err == nil {
			return shell
		}
		return ""
	}
	return ""
}

func inside(root, path string) bool {
	if path == "" {
		return false
	}
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

type ServerProcess struct {
	command  *exec.Cmd
	waitOnce sync.Once
	waitErr  error
}

func (p *ServerProcess) Wait() error {
	if p == nil || p.command == nil {
		return nil
	}
	p.waitOnce.Do(func() { p.waitErr = p.command.Wait() })
	return p.waitErr
}

func (p *ServerProcess) PID() int {
	if p == nil || p.command == nil || p.command.Process == nil {
		return 0
	}
	return p.command.Process.Pid
}

func (p *ServerProcess) Kill() error {
	if p == nil || p.command == nil || p.command.Process == nil {
		return nil
	}
	return p.command.Process.Kill()
}

func TestRootName() string {
	prefix := os.Getenv("POSSE_E2E_TMP_PREFIX")
	if prefix == "" {
		prefix = "posse-e2e-"
	}
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
}
