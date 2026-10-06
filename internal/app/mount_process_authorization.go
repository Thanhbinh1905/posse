package app

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/thanhbinh1905/posse/internal/store"
)

// mountProcessAuthorization retains process handles established while Herdr
// ownership is proved. CWD discovery during cleanup may verify these handles,
// but must not authorize a new process.
type mountProcessAuthorization struct {
	handles       map[int]*mountProcessHandle
	processGroups map[int][]*mountProcessHandle
}

func newMountProcessAuthorization() *mountProcessAuthorization {
	return &mountProcessAuthorization{handles: make(map[int]*mountProcessHandle), processGroups: make(map[int][]*mountProcessHandle)}
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

func (authorization *mountProcessAuthorization) retainProcessGroup(pgid int, handle *mountProcessHandle) {
	if authorization == nil || pgid <= 1 || handle == nil {
		return
	}
	if authorization.processGroups == nil {
		authorization.processGroups = make(map[int][]*mountProcessHandle)
	}
	for _, existing := range authorization.processGroups[pgid] {
		if existing.PID() == handle.PID() {
			return
		}
	}
	authorization.processGroups[pgid] = append(authorization.processGroups[pgid], handle)
}

func (authorization *mountProcessAuthorization) verifyIfAlive(pid int) (bool, error) {
	handle := authorization.handle(pid)
	if handle == nil {
		return false, fmt.Errorf("unverified process %d appeared in the Mount; preserving its work", pid)
	}
	return verifyMountProcessHandleIfAlive(pid, handle)
}

func verifyMountProcessHandleIfAlive(pid int, handle *mountProcessHandle) (bool, error) {
	bootID, startTime, err := store.ProcessIdentityForPID(pid)
	if err != nil {
		if processGone(err) {
			return false, nil
		}
		return false, fmt.Errorf("cannot verify Mount process %d identity: %w", pid, err)
	}
	if handle.Identity() != bootID+"/"+startTime {
		return false, fmt.Errorf("mount PID %d changed process instance; preserving its work", pid)
	}
	alive, err := handle.Alive()
	if err != nil {
		return false, fmt.Errorf("cannot verify Mount process %d liveness: %w", pid, err)
	}
	return alive, nil
}

func unretainedMountProcessAlive(pid int) (bool, error) {
	handle, err := openMountProcessHandle(pid)
	if err != nil {
		if processGone(err) {
			return false, nil
		}
		return false, err
	}
	defer handle.Close()
	return handle.Alive()
}

// trackShortLivedMountProcess retains a handle only to verify natural exit.
// The process must descend from, or share a group with, an exact retained pane
// process; it is never authorized for signaling.
func trackShortLivedMountProcess(pid int, root string, authorization *mountProcessAuthorization) (*mountProcessHandle, bool, error) {
	if authorization == nil || !processInMount(pid, root) {
		return nil, false, nil
	}
	authorizedAncestor, err := hasRetainedMountProcessAncestor(pid, authorization)
	if err != nil {
		return nil, false, err
	}
	if !authorizedAncestor {
		groupID, groupErr := mountProcessGroupID(pid)
		if groupErr != nil {
			if processGone(groupErr) {
				return nil, false, nil
			}
			return nil, false, groupErr
		}
		verifiedGroupMember, groupErr := verifiedRetainedProcessGroupMember(groupID, authorization.processGroups[groupID])
		if groupErr != nil {
			return nil, false, groupErr
		}
		if !verifiedGroupMember {
			return nil, false, nil
		}
	}
	child, err := openMountProcessHandle(pid)
	if err != nil {
		if processGone(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if !processInMount(pid, root) {
		_ = child.Close()
		return nil, false, nil
	}
	return child, true, nil
}

func verifiedRetainedProcessGroupMember(groupID int, handles []*mountProcessHandle) (bool, error) {
	for _, handle := range handles {
		alive, err := verifyMountProcessHandleIfAlive(handle.PID(), handle)
		if err != nil {
			return false, err
		}
		if handle.PID() == groupID {
			// A live process in the group keeps this PGID tied to the original
			// group leader, even after that exact leader exits.
			return true, nil
		}
		if !alive {
			continue
		}
		currentGroup, err := mountProcessGroupID(handle.PID())
		if err != nil {
			if processGone(err) {
				continue
			}
			return false, err
		}
		if currentGroup == groupID {
			return true, nil
		}
	}
	return false, nil
}

func hasRetainedMountProcessAncestor(pid int, authorization *mountProcessAuthorization) (bool, error) {
	current := pid
	seen := make(map[int]struct{})
	for range 1024 {
		parent, err := mountProcessParentID(current)
		if err != nil {
			if processGone(err) {
				return false, nil
			}
			return false, err
		}
		if parent <= 1 || parent == current {
			return false, nil
		}
		if _, found := seen[parent]; found {
			return false, fmt.Errorf("process ancestry loop while verifying PID %d", pid)
		}
		seen[parent] = struct{}{}
		if handle := authorization.handle(parent); handle != nil {
			alive, verifyErr := verifyMountProcessHandleIfAlive(parent, handle)
			if verifyErr != nil {
				return false, verifyErr
			}
			if alive {
				return true, nil
			}
		}
		current = parent
	}
	return false, fmt.Errorf("process ancestry for PID %d exceeded 1024 parents", pid)
}

func processGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}
