package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) releaseSandboxWithAuthorization(ctx context.Context, db *store.DB, project store.Project, task store.Task, clean string, approvedDiscard bool, authorization *sandboxProcessAuthorization) ([]string, error) {
	sandboxes, err := db.Sandboxes(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	var sandbox store.Sandbox
	for _, candidate := range sandboxes {
		if candidate.ID == task.SandboxID {
			sandbox = candidate
			break
		}
	}
	if sandbox.ID == 0 || sandbox.TaskID != task.ID || (sandbox.State != "held" && sandbox.State != "releasing") {
		return nil, nil
	}
	if sandbox.State == "releasing" || approvedDiscard {
		if err := requireNoSandboxProcesses(sandbox.Path, approvedDiscard); err != nil {
			return nil, err
		}
	}
	if sandbox.State == "releasing" {
		return nil, finishReleasingSandbox(ctx, db, project, sandbox, task)
	}
	var killed []string
	if !approvedDiscard {
		killed, err = stopSandboxProcessesAuthorized(sandbox.Path, authorization)
		if err != nil {
			return killed, err
		}
	}
	if err := requireNoSandboxProcesses(sandbox.Path, approvedDiscard); err != nil {
		return killed, err
	}
	var resetErr error
	if project.IsWorkspace() {
		resetErr = releaseWorkspaceSandbox(ctx, db, project, sandbox, clean, s.removeSandbox)
	} else {
		// Teardown is local and must not wait on origin. The next Sandbox
		// acquisition fetches before starting new work.
		resetErr = resetWorktree(ctx, sandbox.Path, projectRepoTarget(project), clean, false)
	}
	if err := resetErr; err != nil {
		if errors.Is(err, errSandboxDefaultRefUnavailable) {
			return killed, err
		}
		if breakErr := breakSandbox(ctx, db, project, task, sandbox, "Sandbox could not be reset during release"); breakErr != nil {
			return killed, errors.Join(err, breakErr)
		}
		if approvedDiscard {
			return killed, nil
		}
		return killed, err
	}
	return killed, withSandboxStateLock(ctx, db, func() error {
		if err := db.BeginSandboxRelease(ctx, sandbox.ID, task.ID); err != nil {
			return err
		}
		return finishReleasingSandboxLocked(ctx, db, project, sandbox, task)
	})
}

func stopSandboxProcessesAuthorized(path string, authorization *sandboxProcessAuthorization) ([]string, error) {
	if message := processIdentityCapabilityError(); message != "" {
		return nil, fmt.Errorf("%s; refusing to signal processes or reset the Sandbox", message)
	}
	root, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolved
	} else if !errors.Is(resolveErr, os.ErrNotExist) {
		return nil, resolveErr
	}
	pids, err := sandboxProcessIDs(root)
	if err != nil {
		return nil, err
	}
	allPIDs := make([]int, 0, len(pids))
	handles := make(map[int]*sandboxProcessHandle, len(pids))
	transients := make(map[int]*sandboxProcessHandle)
	defer func() {
		for _, handle := range transients {
			_ = handle.Close()
		}
	}()
	for _, pid := range pids {
		handle := authorization.handle(pid)
		if handle == nil {
			child, found, childErr := trackShortLivedSandboxProcess(pid, root, authorization)
			if childErr != nil {
				return nil, fmt.Errorf("could not verify Sandbox child process %d: %w", pid, childErr)
			}
			if found {
				transients[pid] = child
				continue
			}
			alive, aliveErr := unretainedSandboxProcessAlive(pid)
			if aliveErr != nil {
				return nil, fmt.Errorf("could not verify unretained Sandbox process %d: %w", pid, aliveErr)
			}
			if alive {
				return nil, fmt.Errorf("unverified process %d appeared in the Sandbox; preserving its work", pid)
			}
			continue
		}
		alive, verifyErr := authorization.verifyIfAlive(pid)
		if verifyErr != nil {
			return nil, verifyErr
		}
		if !alive {
			continue
		}
		handles[pid] = handle
		allPIDs = append(allPIDs, pid)
	}
	if err := verifySandboxProcessSet(root, handles, transients, authorization, false); err != nil {
		return nil, err
	}
	for _, handle := range handles {
		if err := handle.Signal(syscall.SIGTERM); err != nil {
			return nil, err
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := verifySandboxProcessSet(root, handles, transients, authorization, true); err != nil {
			return nil, err
		}
		alive, err := sandboxHandlesInSandbox(handles, root)
		if err != nil {
			return nil, err
		}
		if len(alive) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	alive, err := sandboxHandlesInSandbox(handles, root)
	if err != nil {
		return nil, err
	}
	for _, handle := range alive {
		if err := handle.Signal(syscall.SIGKILL); err != nil {
			return nil, err
		}
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := verifySandboxProcessSet(root, handles, transients, authorization, true); err != nil {
			return nil, err
		}
		survivors, err := sandboxProcessIDs(root)
		if err != nil {
			return nil, err
		}
		if len(survivors) == 0 {
			return processLabels(allPIDs), nil
		}
		stillAlive, err := sandboxHandlesInSandbox(handles, root)
		if err != nil {
			return nil, err
		}
		if len(stillAlive) == 0 {
			return nil, fmt.Errorf("unverified Sandbox processes appeared during teardown: %v", survivors)
		}
		time.Sleep(25 * time.Millisecond)
	}
	survivors, err := sandboxProcessIDs(root)
	if err != nil {
		return nil, err
	}
	return processLabels(allPIDs), fmt.Errorf("sandbox processes survived teardown: %v", survivors)
}

func verifySandboxProcessSet(root string, handles, transients map[int]*sandboxProcessHandle, authorization *sandboxProcessAuthorization, waitForTransients bool) error {
	pids, err := sandboxProcessIDs(root)
	if err != nil {
		return err
	}
	for _, pid := range pids {
		handle, found := handles[pid]
		if !found {
			child := transients[pid]
			if child == nil {
				var isChild bool
				var childErr error
				child, isChild, childErr = trackShortLivedSandboxProcess(pid, root, authorization)
				if childErr != nil {
					return fmt.Errorf("cannot verify Sandbox child process %d during teardown: %w", pid, childErr)
				}
				if isChild {
					transients[pid] = child
				}
			}
			if child != nil {
				alive, childErr := verifySandboxProcessHandleIfAlive(pid, child)
				if childErr != nil {
					return childErr
				}
				if alive && waitForTransients {
					if err := waitForSandboxProcessExit(pid, root, child); err != nil {
						return err
					}
				}
				continue
			}
			alive, aliveErr := unretainedSandboxProcessAlive(pid)
			if aliveErr != nil {
				return fmt.Errorf("cannot verify Sandbox process %d during teardown: %w", pid, aliveErr)
			}
			if alive {
				return fmt.Errorf("unverified process %d appeared in the Sandbox during teardown; preserving its work", pid)
			}
			continue
		}
		if _, err := verifySandboxProcessHandleIfAlive(pid, handle); err != nil {
			return err
		}
	}
	return nil
}

func waitForSandboxProcessExit(pid int, root string, handle *sandboxProcessHandle) error {
	deadline := time.Now().Add(300 * time.Millisecond)
	for {
		alive, err := verifySandboxProcessHandleIfAlive(pid, handle)
		if err != nil {
			return err
		}
		if !alive || !processInSandbox(pid, root) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("short-lived process %d remained in the Sandbox after retained pane processes stopped; preserving its work", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func sandboxHandlesInSandbox(handles map[int]*sandboxProcessHandle, root string) ([]*sandboxProcessHandle, error) {
	var alive []*sandboxProcessHandle
	for _, handle := range handles {
		stillAlive, err := handle.Alive()
		if err != nil {
			return nil, fmt.Errorf("check Sandbox process %d: %w", handle.PID(), err)
		}
		if stillAlive && processInSandbox(handle.PID(), root) {
			alive = append(alive, handle)
		}
	}
	return alive, nil
}

func processLabels(pids []int) []string {
	labels := make([]string, 0, len(pids))
	for _, pid := range pids {
		labels = append(labels, strconv.Itoa(pid))
	}
	return labels
}

func stopTaskScratchProcessesAuthorized(home string, project store.Project, task store.Task, authorization *sandboxProcessAuthorization) ([]string, error) {
	root, err := validatedTaskScratchRoot(home, project, task)
	if err != nil {
		return nil, err
	}
	return stopAuthorizedOwnedProcesses([]ownedProcessRoot{{path: root, includeOpenFiles: true, includeMappedFiles: true}}, authorization)
}
