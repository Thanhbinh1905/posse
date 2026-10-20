package app

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"syscall"

	"github.com/thanhbinh1905/posse/internal/store"
)

// mountProcessAuthorization retains process handles established while Herdr
// ownership is proved. CWD discovery during cleanup may verify these handles,
// but must not authorize a new process.
type sandboxProcessAuthorization struct {
	handles       map[int]*sandboxProcessHandle
	processGroups map[int][]*sandboxProcessHandle
}

func newSandboxProcessAuthorization() *sandboxProcessAuthorization {
	return &sandboxProcessAuthorization{handles: make(map[int]*sandboxProcessHandle), processGroups: make(map[int][]*sandboxProcessHandle)}
}

func (authorization *sandboxProcessAuthorization) bind(pid int) (*sandboxProcessHandle, error) {
	if pid <= 1 {
		return nil, fmt.Errorf("cannot bind invalid process ID %d", pid)
	}
	if handle := authorization.handles[pid]; handle != nil {
		return handle, nil
	}
	handle, err := openSandboxProcessHandle(pid)
	if err != nil {
		return nil, err
	}
	authorization.handles[pid] = handle
	return handle, nil
}

func (authorization *sandboxProcessAuthorization) captureDescendants() error {
	if authorization == nil || processIdentityCapabilityError() != "" {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	children := make(map[int][]int, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		parent, err := sandboxProcessParentID(pid)
		if err == nil && parent > 1 && parent != pid {
			children[parent] = append(children[parent], pid)
		}
	}
	queue := make([]int, 0, len(authorization.handles))
	for pid := range authorization.handles {
		queue = append(queue, pid)
	}
	seen := make(map[int]bool, len(children))
	var candidates []int
	for len(queue) != 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, pid := range children[parent] {
			if seen[pid] || authorization.handle(pid) != nil {
				continue
			}
			seen[pid] = true
			candidates = append(candidates, pid)
			queue = append(queue, pid)
		}
	}
	sort.Ints(candidates)
	for _, pid := range candidates {
		descendant, err := hasRetainedSandboxProcessAncestor(pid, authorization)
		if processGone(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("verify descendant process %d: %w", pid, err)
		}
		if !descendant {
			continue
		}
		handle, err := openSandboxProcessHandle(pid)
		if processGone(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("retain descendant process %d: %w", pid, err)
		}
		descendant, err = hasRetainedSandboxProcessAncestor(pid, authorization)
		if processGone(err) || err == nil && !descendant {
			_ = handle.Close()
			continue
		}
		if err != nil {
			_ = handle.Close()
			return fmt.Errorf("recheck descendant process %d: %w", pid, err)
		}
		authorization.handles[pid] = handle
		if groupID, groupErr := sandboxProcessGroupID(pid); groupErr == nil {
			authorization.retainProcessGroup(groupID, handle)
		} else if !processGone(groupErr) {
			return fmt.Errorf("verify descendant process group %d: %w", pid, groupErr)
		}
	}
	return nil
}

func (authorization *sandboxProcessAuthorization) Close() {
	if authorization == nil {
		return
	}
	for _, handle := range authorization.handles {
		_ = handle.Close()
	}
}

func (authorization *sandboxProcessAuthorization) handle(pid int) *sandboxProcessHandle {
	if authorization == nil {
		return nil
	}
	return authorization.handles[pid]
}

func (authorization *sandboxProcessAuthorization) setSignalGuard(pid int, guard func(syscall.Signal) error) {
	if handle := authorization.handle(pid); handle != nil {
		handle.signalGuard = guard
	}
}

func (authorization *sandboxProcessAuthorization) guardSignal(pid int, signal syscall.Signal) error {
	handle := authorization.handle(pid)
	if handle == nil {
		return fmt.Errorf("PID %d was not retained during Task ownership proof; preserving it", pid)
	}
	alive, err := verifySandboxProcessHandleIfAlive(pid, handle)
	if err != nil {
		return err
	}
	if !alive {
		return nil
	}
	if handle.signalGuard == nil {
		return nil
	}
	return handle.signalGuard(signal)
}

func (authorization *sandboxProcessAuthorization) retainProcessGroup(pgid int, handle *sandboxProcessHandle) {
	if authorization == nil || pgid <= 1 || handle == nil {
		return
	}
	if authorization.processGroups == nil {
		authorization.processGroups = make(map[int][]*sandboxProcessHandle)
	}
	for _, existing := range authorization.processGroups[pgid] {
		if existing.PID() == handle.PID() {
			return
		}
	}
	authorization.processGroups[pgid] = append(authorization.processGroups[pgid], handle)
}

func (authorization *sandboxProcessAuthorization) verifyIfAlive(pid int) (bool, error) {
	handle := authorization.handle(pid)
	if handle == nil {
		// Enumeration can outlive a child that exited before handle retention.
		// Proving exit is safe; observing a live PID grants no cleanup authority.
		if alive, err := unretainedSandboxProcessAlive(pid); err != nil || !alive {
			return false, err
		}
		return false, fmt.Errorf("unverified process %d appeared in the Sandbox; preserving its work", pid)
	}
	return verifySandboxProcessHandleIfAlive(pid, handle)
}

func verifySandboxProcessHandleIfAlive(pid int, handle *sandboxProcessHandle) (bool, error) {
	bootID, startTime, err := store.ProcessIdentityForPID(pid)
	if err != nil {
		if processGone(err) {
			return false, nil
		}
		return false, fmt.Errorf("cannot verify Sandbox process %d identity: %w", pid, err)
	}
	if handle.Identity() != bootID+"/"+startTime {
		return false, fmt.Errorf("sandbox PID %d changed process instance; preserving its work", pid)
	}
	alive, err := handle.Alive()
	if err != nil {
		return false, fmt.Errorf("cannot verify Sandbox process %d liveness: %w", pid, err)
	}
	return alive, nil
}

func unretainedSandboxProcessAlive(pid int) (bool, error) {
	handle, err := openSandboxProcessHandle(pid)
	if err != nil {
		if processGone(err) {
			return false, nil
		}
		return false, err
	}
	defer handle.Close()
	return handle.Alive()
}

// trackShortLivedSandboxProcess retains a handle only to verify natural exit.
// The process must descend from, or share a group with, an exact retained pane
// process; it is never authorized for signaling.
func trackShortLivedSandboxProcess(pid int, root string, authorization *sandboxProcessAuthorization) (*sandboxProcessHandle, bool, error) {
	if authorization == nil || !processInSandbox(pid, root) {
		return nil, false, nil
	}
	authorizedAncestor, err := hasRetainedSandboxProcessAncestor(pid, authorization)
	if err != nil {
		return nil, false, err
	}
	if !authorizedAncestor {
		groupID, groupErr := sandboxProcessGroupID(pid)
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
	child, err := openSandboxProcessHandle(pid)
	if err != nil {
		if processGone(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if !processInSandbox(pid, root) {
		_ = child.Close()
		return nil, false, nil
	}
	return child, true, nil
}

func verifiedRetainedProcessGroupMember(groupID int, handles []*sandboxProcessHandle) (bool, error) {
	for _, handle := range handles {
		alive, err := verifySandboxProcessHandleIfAlive(handle.PID(), handle)
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
		currentGroup, err := sandboxProcessGroupID(handle.PID())
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

func hasRetainedSandboxProcessAncestor(pid int, authorization *sandboxProcessAuthorization) (bool, error) {
	current := pid
	seen := make(map[int]struct{})
	for range 1024 {
		parent, err := sandboxProcessParentID(current)
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
			alive, verifyErr := verifySandboxProcessHandleIfAlive(parent, handle)
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
