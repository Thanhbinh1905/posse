//go:build !linux

package app

import (
	"fmt"
	"syscall"
)

type mountProcessHandle struct {
	pid int
}

func processIdentityCapabilityError() string {
	return "exact process-instance signaling with pidfds is unavailable on this platform"
}

func openMountProcessHandle(pid int) (*mountProcessHandle, error) {
	return nil, fmt.Errorf("process-instance signaling is unsupported on this platform; refusing to signal PID %d", pid)
}

func (handle *mountProcessHandle) PID() int { return handle.pid }

func (handle *mountProcessHandle) Identity() string { return "" }

func (handle *mountProcessHandle) Signal(syscall.Signal) error {
	return fmt.Errorf("process-instance signaling is unsupported on this platform")
}

func (handle *mountProcessHandle) Alive() (bool, error) {
	return false, fmt.Errorf("process-instance signaling is unsupported on this platform")
}

func (handle *mountProcessHandle) Close() error { return nil }
