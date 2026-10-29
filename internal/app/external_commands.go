package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/thanhbinh1905/posse/internal/atomicfile"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/execgroup"
)

var externalCommandTimeout = 20 * time.Second

const externalCommandWaitDelay = execgroup.WaitDelay

func externalCommandTimeoutValue() time.Duration {
	if os.Getenv("POSSE_TEST_HERDR") == "1" {
		if value := strings.TrimSpace(os.Getenv("POSSE_TEST_EXTERNAL_COMMAND_TIMEOUT")); value != "" {
			if timeout, err := time.ParseDuration(value); err == nil && timeout > 0 {
				return timeout
			}
		}
	}
	return externalCommandTimeout
}

func commandOutputArgs(ctx context.Context, cwd, name string, args ...string) (string, error) {
	return commandOutputArgsWithTimeout(ctx, timeoutForExternalCommand(name, args), cwd, name, args...)
}

func commandOutputArgsWithTimeout(ctx context.Context, timeout time.Duration, cwd, name string, args ...string) (string, error) {
	commandContext := ctx
	cancel := func() {}
	if timeout > 0 {
		commandContext, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	command := execgroup.CommandContext(commandContext, externalCommandPath(name), args...)
	command.Dir = cwd
	command.Env = externalCommandEnvironment(name)
	output, err := command.CombinedOutput()
	if commandContext.Err() != nil {
		if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
			return string(output), fmt.Errorf("%s timed out after %s: %w", name, timeout, context.DeadlineExceeded)
		}
		return string(output), commandContext.Err()
	}
	return string(output), err
}

func commandStdoutArgs(ctx context.Context, cwd, name string, args ...string) (string, error) {
	return commandStdoutArgsWithEnv(ctx, cwd, nil, name, args...)
}

func commandStdoutArgsWithEnv(ctx context.Context, cwd string, extraEnv []string, name string, args ...string) (string, error) {
	timeout := timeoutForExternalCommand(name, args)
	commandContext := ctx
	cancel := func() {}
	if timeout > 0 {
		commandContext, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	command := execgroup.CommandContext(commandContext, externalCommandPath(name), args...)
	command.Dir = cwd
	command.Env = externalCommandEnvironment(name)
	for _, entry := range extraEnv {
		key, value, found := strings.Cut(entry, "=")
		if found {
			command.Env = replaceEnvironment(command.Env, key, value)
		}
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if commandContext.Err() != nil {
		if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
			return stdout.String(), fmt.Errorf("%s timed out after %s: %w", name, timeout, context.DeadlineExceeded)
		}
		return stdout.String(), commandContext.Err()
	}
	if err != nil && stderr.Len() > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), err
}

func timeoutForExternalCommand(name string, args []string) time.Duration {
	if name == "gh" || name == "glab" {
		return externalCommandTimeoutValue()
	}
	if name == "git" {
		for _, arg := range args {
			if arg == "fetch" || arg == "ls-remote" {
				return externalCommandTimeoutValue()
			}
		}
	}
	return 0
}

// userShellPathFile holds the PATH of the User's shell. A process started
// outside it, such as the Project watch the Herdr plugin starts, inherits the
// Herdr server's PATH, which can resolve an older gh or glab.
const userShellPathFile = "user-shell-path"

var userShellPathMu sync.Mutex
var recordedUserShellPath string

// isUserCommandEntry reports whether this process can be shown to run from the
// User's shell: it has a terminal on stdin, or runs in a Herdr pane such as a
// Lead or Worker agent's, and carries no watch or plugin marker. A process
// started by cron, systemd or another service has neither.
func isUserCommandEntry() bool {
	if os.Getenv(watchRuntimeTokenEnv) != "" || os.Getenv("HERDR_PLUGIN_ID") != "" {
		return false
	}
	return isatty.IsTerminal(os.Stdin.Fd()) || os.Getenv("HERDR_PANE_ID") != ""
}

// externalCommandPath resolves gh and glab the same way in every posse
// process: first in the User's shell PATH, then in this process's PATH.
func externalCommandPath(name string) string {
	if name != "gh" && name != "glab" {
		return name
	}
	path := os.Getenv("PATH")
	if isUserCommandEntry() {
		recordUserShellPath(path)
	} else if userPath := readUserShellPath(); userPath != "" {
		path = userPath + string(os.PathListSeparator) + path
	}
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return name
}

// persistUserShellPath is false in the package tests so that the PATH of one
// test never leaks into the PATH resolution of another through the home.
var persistUserShellPath = true

func recordUserShellPath(path string) {
	home, err := config.DefaultHome()
	if err != nil || path == "" || !persistUserShellPath {
		return
	}
	file := filepath.Join(home, userShellPathFile)
	userShellPathMu.Lock()
	defer userShellPathMu.Unlock()
	if recordedUserShellPath == file+"\x00"+path {
		return
	}
	if existing, err := os.ReadFile(file); err != nil || string(existing) != path {
		if atomicfile.Write(file, []byte(path), 0o600) != nil {
			return
		}
	}
	recordedUserShellPath = file + "\x00" + path
}

func readUserShellPath() string {
	home, err := config.DefaultHome()
	if err != nil {
		return ""
	}
	path, err := os.ReadFile(filepath.Join(home, userShellPathFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(path))
}

func externalCommandEnvironment(name string) []string {
	env := os.Environ()
	if name == "git" || name == "gh" || name == "glab" {
		env = replaceEnvironment(env, "GIT_TERMINAL_PROMPT", "0")
	}
	if name == "gh" || name == "glab" {
		env = replaceEnvironment(env, "GH_PROMPT_DISABLED", "1")
	}
	return env
}

func replaceEnvironment(env []string, key, value string) []string {
	filtered := make([]string, 0, len(env)+1)
	for _, entry := range env {
		name, _, found := strings.Cut(entry, "=")
		if !found || name != key {
			filtered = append(filtered, entry)
		}
	}
	return append(filtered, key+"="+value)
}
