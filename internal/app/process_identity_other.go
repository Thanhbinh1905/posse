//go:build !linux

package app

import (
	"fmt"
	"syscall"
)

type sandboxProcessHandle struct {
	pid         int
	signalGuard func(syscall.Signal) error
}

func sandboxProcessExecutableIdentity(pid int) (string, error) {
	return "", fmt.Errorf("process executable identity is unsupported on this platform for PID %d", pid)
}

func sandboxProcessGroupID(pid int) (int, error) {
	return 0, fmt.Errorf("process group is unsupported on this platform for PID %d", pid)
}

func sandboxProcessParentID(pid int) (int, error) {
	return 0, fmt.Errorf("process ancestry is unsupported on this platform for PID %d", pid)
}

func processIdentityCapabilityError() string {
	return "exact process-instance signaling with pidfds is unavailable on this platform"
}

func openSandboxProcessHandle(pid int) (*sandboxProcessHandle, error) {
	return nil, fmt.Errorf("process-instance signaling is unsupported on this platform; refusing to signal PID %d", pid)
}

func (handle *sandboxProcessHandle) PID() int { return handle.pid }

func (handle *sandboxProcessHandle) Identity() string { return "" }

func (handle *sandboxProcessHandle) ExecutableIdentity() string { return "" }

func (handle *sandboxProcessHandle) Signal(syscall.Signal) error {
	return fmt.Errorf("process-instance signaling is unsupported on this platform")
}

func (handle *sandboxProcessHandle) Alive() (bool, error) {
	return false, fmt.Errorf("process-instance signaling is unsupported on this platform")
}

func (handle *sandboxProcessHandle) Close() error { return nil }
