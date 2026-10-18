//go:build e2e && linux

package e2e

import (
	"os/exec"
	"syscall"
)

func configureE2ELockOwner(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
