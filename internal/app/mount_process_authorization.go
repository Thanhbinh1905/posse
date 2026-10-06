package app

import (
	"fmt"

	"github.com/thanhbinh1905/posse/internal/store"
)

// mountProcessAuthorization retains process handles established while Herdr
// ownership is proved. CWD discovery during cleanup may verify these handles,
// but must not authorize a new process.
type mountProcessAuthorization struct {
	handles map[int]*mountProcessHandle
}

func newMountProcessAuthorization() *mountProcessAuthorization {
	return &mountProcessAuthorization{handles: make(map[int]*mountProcessHandle)}
}

func (authorization *mountProcessAuthorization) bind(pid int) (*mountProcessHandle, error) {
	if pid <= 1 {
		return nil, fmt.Errorf("cannot bind invalid process ID %d", pid)
	}
	if handle := authorization.handles[pid]; handle != nil {
		return handle, nil
	}
	handle, err := openMountProcessHandle(pid)
	if err != nil {
		return nil, err
	}
	authorization.handles[pid] = handle
	return handle, nil
}

func (authorization *mountProcessAuthorization) Close() {
	if authorization == nil {
		return
	}
	for _, handle := range authorization.handles {
		_ = handle.Close()
	}
}

func (authorization *mountProcessAuthorization) handle(pid int) *mountProcessHandle {
	if authorization == nil {
		return nil
	}
	return authorization.handles[pid]
}

func (authorization *mountProcessAuthorization) verify(pid int) error {
	handle := authorization.handle(pid)
	if handle == nil {
		return fmt.Errorf("unverified process %d appeared in the Mount; preserving its work", pid)
	}
	bootID, startTime, err := store.ProcessIdentityForPID(pid)
	if err != nil {
		return fmt.Errorf("cannot verify Mount process %d identity: %w", pid, err)
	}
	if handle.Identity() != bootID+"/"+startTime {
		return fmt.Errorf("mount PID %d changed process instance; preserving its work", pid)
	}
	return nil
}
