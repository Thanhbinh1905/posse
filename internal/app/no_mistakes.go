package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
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

func ensureNoMistakesInitialized(ctx context.Context, root string) error {
	if _, err := exec.LookPath("no-mistakes"); err != nil {
		return axi.Failure("no_mistakes_unavailable", "no-mistakes is not installed or not on PATH", false, "Install no-mistakes, then run `no-mistakes init` in this repository")
	}
	output, err := commandOutputArgs(ctx, root, "no-mistakes", "axi", "status")
	if strings.Contains(strings.ToLower(output), "repo not initialized") {
		return axi.Failure("no_mistakes_uninitialized", "no-mistakes reports that this repository is not initialized", false, "Run `no-mistakes init`")
	}
	if err != nil {
		return axi.Failure("no_mistakes_status_failed", "could not verify no-mistakes repository status", true, strings.TrimSpace(output))
	}
	return nil
}

func isNoMistakesPolicySetting(key string) bool {
	return key == "defaults.merge_mode" || key == "defaults.review" ||
		strings.HasPrefix(key, "repositories.") && (strings.HasSuffix(key, ".merge_mode") || strings.HasSuffix(key, ".review"))
}

func noMistakesDeprecatedWarning(setting string) axi.Object {
	return axi.Object{
		{Key: "code", Value: "deprecated"},
		{Key: "message", Value: setting + " uses deprecated no-mistakes; migrate to the built-in validation settings in docs/spec.md section 25"},
	}
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
	command := execgroup.CommandContext(commandContext, name, args...)
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
	command := execgroup.CommandContext(commandContext, name, args...)
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
	if name == "gh" || name == "glab" || name == "no-mistakes" {
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

func externalCommandEnvironment(name string) []string {
	env := os.Environ()
	if name == "git" || name == "gh" || name == "glab" || name == "no-mistakes" {
		env = replaceEnvironment(env, "GIT_TERMINAL_PROMPT", "0")
	}
	if name == "gh" || name == "glab" || name == "no-mistakes" {
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
