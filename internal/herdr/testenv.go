package herdr

import (
	"crypto/sha256"
	_ "embed"
	"errors"
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
		if found && (strings.HasPrefix(key, "HERDR_") || key == "CLAUDECODE" || key == "CLAUDE_AGENT_ID" || key == "CODEX_CI" || key == "CODEX_THREAD_ID" || key == "PI_CODING_AGENT" || key == "PI_SESSION_ID" || key == "OPENCODE_SESSION_ID") {
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
		"POSSE_TEST_HERDR_SOCKET_PATH="+IsolatedTestSocketPath(root),
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

// IsolatedTestSocketPath keeps fixture sockets below a conservative Unix
// domain socket path limit, using a fixture-specific sibling when needed.
func IsolatedTestSocketPath(root string) string {
	if absolute, err := filepath.Abs(root); err == nil {
		root = absolute
	}
	root = filepath.Clean(root)
	defaultPath := filepath.Join(root, "xdg", "herdr", "herdr.sock")
	if len(defaultPath) < 90 {
		return defaultPath
	}
	digest := sha256.Sum256([]byte(root))
	return filepath.Join(filepath.Dir(root), fmt.Sprintf("h%x", digest[:8]), "herdr.sock")
}

func IsolatedTestSocketDir(root string) string {
	return filepath.Dir(IsolatedTestSocketPath(root))
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
	rootAbs, rootErr := filepath.Abs(root)
	tmpDir := env["TMPDIR"]
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	tmpDirAbs, tmpDirErr := filepath.Abs(tmpDir)
	if tmpDirErr == nil {
		tmpDirAbs, tmpDirErr = filepath.EvalSymlinks(tmpDirAbs)
	}
	scratchRoot := filepath.Clean(env["POSSE_E2E_TMP_ROOT"])
	insideScratch := safeTaskScratchRoot(scratchRoot) && inside(scratchRoot, rootAbs)
	resolvedRoot, resolveErr := filepath.EvalSymlinks(rootAbs)
	parent := filepath.Dir(resolvedRoot)
	configuredTemp := filepath.IsAbs(tmpDir) && tmpDirErr == nil && filepath.Clean(tmpDirAbs) != string(filepath.Separator) && parent == filepath.Clean(tmpDirAbs)
	// A fixture may override its child TMPDIR with a subdirectory after being
	// created directly under this test process's Task scratch directory.
	processTemp, processTempErr := filepath.EvalSymlinks(os.TempDir())
	configuredTemp = configuredTemp || processTempErr == nil && filepath.IsAbs(processTemp) && processTemp != string(filepath.Separator) && parent == filepath.Clean(processTemp)
	parentIsTemp := parent == "/tmp" || parent == "/var/tmp" || parent == filepath.Clean(os.TempDir()) || configuredTemp
	insideTemp := false
	for _, tempRoot := range []string{"/tmp", "/var/tmp"} {
		relative, err := filepath.Rel(tempRoot, resolvedRoot)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			insideTemp = true
			break
		}
	}
	if env["POSSE_TEST_HERDR"] != "1" || rootErr != nil || tmpDirErr != nil || resolveErr != nil || !filepath.IsAbs(root) || !strings.HasPrefix(filepath.Base(rootAbs), "posse-e2e-") || !parentIsTemp && !insideScratch || !insideTemp && !insideScratch && !configuredTemp {
		return "", &Error{Code: "unsafe_test_environment", Message: "isolated Herdr mutation requires a /tmp/posse-e2e-* root, a fixture under the configured E2E scratch root, or a fixture directly under TMPDIR"}
	}
	root = rootAbs
	for key := range env {
		if strings.HasPrefix(key, "HERDR_") {
			return "", &Error{Code: "unsafe_test_environment", Message: fmt.Sprintf("isolated Herdr environment retained %s", key)}
		}
	}
	if socket := env["POSSE_TEST_HERDR_SOCKET_PATH"]; socket != "" {
		if socket != IsolatedTestSocketPath(root) || len(socket) >= 108 {
			return "", &Error{Code: "unsafe_test_environment", Message: "isolated Herdr socket path is not the expected short fixture socket"}
		}
		socketDir := filepath.Dir(socket)
		if socketDir == filepath.Join(root, "xdg", "herdr") {
			socketDirResolved, err := filepath.EvalSymlinks(socketDir)
			if err != nil || socketDirResolved != filepath.Join(resolvedRoot, "xdg", "herdr") {
				return "", &Error{Code: "unsafe_test_environment", Message: "isolated Herdr socket must stay inside its fixture", Cause: err}
			}
		} else {
			socketParent, err := filepath.EvalSymlinks(filepath.Dir(socketDir))
			if err != nil || socketParent != filepath.Dir(resolvedRoot) {
				return "", &Error{Code: "unsafe_test_environment", Message: "isolated Herdr socket directory must stay beside its fixture", Cause: err}
			}
			if info, err := os.Lstat(socketDir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0) {
				return "", &Error{Code: "unsafe_test_environment", Message: "isolated Herdr socket directory is not a private directory"}
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return "", &Error{Code: "unsafe_test_environment", Message: "could not inspect isolated Herdr socket directory", Cause: err}
			}
		}
		if info, err := os.Lstat(socket); err == nil && (info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0) {
			return "", &Error{Code: "unsafe_test_environment", Message: "isolated Herdr socket path is occupied by a non-socket"}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", &Error{Code: "unsafe_test_environment", Message: "could not inspect isolated Herdr socket path", Cause: err}
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

func safeTaskScratchRoot(root string) bool {
	if filepath.Clean(root) != root || filepath.Dir(root) != "/tmp" || !strings.HasPrefix(filepath.Base(root), "posse-") || !strings.HasSuffix(filepath.Base(root), "-scratch") {
		return false
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	resolved, err := filepath.EvalSymlinks(root)
	return err == nil && resolved == root
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
	testRoot string
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

func (p *ServerProcess) TestRoot() string {
	if p == nil {
		return ""
	}
	return p.testRoot
}

func (p *ServerProcess) Kill() error {
	if p == nil || p.command == nil || p.command.Process == nil {
		return nil
	}
	if p.command.Cancel != nil {
		err := p.command.Cancel()
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	return p.command.Process.Kill()
}

func TestRootName() string {
	prefix := os.Getenv("POSSE_E2E_TMP_PREFIX")
	if prefix == "" {
		prefix = "posse-e2e-"
	}
	return prefix + strconv.FormatInt(time.Now().UnixNano(), 36)
}
