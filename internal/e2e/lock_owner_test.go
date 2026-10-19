//go:build e2e

package e2e

import (
	"os"
	"os/exec"
)

func newE2ELockOwner(name, path string, env []string) *exec.Cmd {
	command := exec.Command("sh", "-c", `exec 9>"$1"; flock -x 9; printf 'locked\n'; exec sleep 60`, name, path)
	command.Env = e2eEnvironmentWithTaskMarker(env)
	configureE2ELockOwner(command)
	return command
}

func e2eEnvironmentWithTaskMarker(env []string) []string {
	if marker := os.Getenv("POSSE_TASK_ID"); marker != "" {
		return setEnv(env, "POSSE_TASK_ID", marker)
	}
	return env
}
