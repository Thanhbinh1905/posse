//go:build e2e && !linux

package e2e

import "os/exec"

func configureE2ELockOwner(*exec.Cmd) {}
