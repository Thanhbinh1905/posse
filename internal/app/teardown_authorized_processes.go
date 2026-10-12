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

func releaseMountWithAuthorization(ctx context.Context, db *store.DB, project store.Project, task store.Task, clean string, approvedDiscard bool, authorization *mountProcessAuthorization) ([]string, error) {
	mounts, err := db.Mounts(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	var mount store.Mount
	for _, candidate := range mounts {
		if candidate.ID == task.MountID {
			mount = candidate
			break
		}
	}
	if mount.ID == 0 || mount.TaskID != task.ID || (mount.State != "held" && mount.State != "releasing") {
		return nil, nil
	}
	if mount.State == "releasing" || approvedDiscard {
		if err := requireNoMountProcesses(mount.Path, approvedDiscard); err != nil {
			return nil, err
		}
	}
	if mount.State == "releasing" {
		return nil, finishReleasingMount(ctx, db, project, mount, task)
	}
	var killed []string
	if !approvedDiscard {
		killed, err = stopMountProcessesAuthorized(mount.Path, authorization)
		if err != nil {
			return killed, err
		}
	}
	if err := requireNoMountProcesses(mount.Path, approvedDiscard); err != nil {
		return killed, err
	}
	var resetErr error
	if project.IsWorkspace() {
		resetErr = releaseWorkspaceMount(ctx, db, project, mount, clean)
	} else {
		// Teardown is local and must not wait on origin. The next Mount
		// acquisition fetches before starting new work.
		resetErr = resetWorktree(ctx, mount.Path, projectRepoTarget(project), clean, false)
	}
	if err := resetErr; err != nil {
		if errors.Is(err, errMountDefaultRefUnavailable) {
			return killed, err
		}
		if breakErr := breakMount(ctx, db, project, task, mount, "Mount could not be reset during release"); breakErr != nil {
			return killed, errors.Join(err, breakErr)
		}
		if approvedDiscard {
			return killed, nil
		}
		return killed, err
	}
	return killed, withMountStateLock(ctx, db, func() error {
		if err := db.BeginMountRelease(ctx, mount.ID, task.ID); err != nil {
			return err
		}
		return finishReleasingMountLocked(ctx, db, project, mount, task)
	})
}

func stopMountProcessesAuthorized(path string, authorization *mountProcessAuthorization) ([]string, error) {
	if message := processIdentityCapabilityError(); message != "" {
		return nil, fmt.Errorf("%s; refusing to signal processes or reset the Mount", message)
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
	pids, err := mountProcessIDs(root)
	if err != nil {
		return nil, err
	}
	allPIDs := make([]int, 0, len(pids))
	handles := make(map[int]*mountProcessHandle, len(pids))
	transients := make(map[int]*mountProcessHandle)
	defer func() {
		for _, handle := range transients {
			_ = handle.Close()
		}
	}()
	for _, pid := range pids {
		handle := authorization.handle(pid)
		if handle == nil {
			child, found, childErr := trackShortLivedMountProcess(pid, root, authorization)
			if childErr != nil {
				return nil, fmt.Errorf("could not verify Mount child process %d: %w", pid, childErr)
			}
			if found {
				transients[pid] = child
				continue
			}
			alive, aliveErr := unretainedMountProcessAlive(pid)
			if aliveErr != nil {
				return nil, fmt.Errorf("could not verify unretained Mount process %d: %w", pid, aliveErr)
			}
			if alive {
				return nil, fmt.Errorf("unverified process %d appeared in the Mount; preserving its work", pid)
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
	if err := verifyMountProcessSet(root, handles, transients, authorization, false); err != nil {
		return nil, err
	}
	for _, handle := range handles {
		if err := handle.Signal(syscall.SIGTERM); err != nil {
			return nil, err
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := verifyMountProcessSet(root, handles, transients, authorization, true); err != nil {
			return nil, err
		}
		alive, err := mountHandlesInMount(handles, root)
		if err != nil {
			return nil, err
		}
		if len(alive) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	alive, err := mountHandlesInMount(handles, root)
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
		if err := verifyMountProcessSet(root, handles, transients, authorization, true); err != nil {
			return nil, err
		}
		survivors, err := mountProcessIDs(root)
		if err != nil {
			return nil, err
		}
		if len(survivors) == 0 {
			return processLabels(allPIDs), nil
		}
		stillAlive, err := mountHandlesInMount(handles, root)
		if err != nil {
			return nil, err
		}
		if len(stillAlive) == 0 {
			return nil, fmt.Errorf("unverified Mount processes appeared during teardown: %v", survivors)
		}
		time.Sleep(25 * time.Millisecond)
	}
	survivors, err := mountProcessIDs(root)
	if err != nil {
		return nil, err
	}
	return processLabels(allPIDs), fmt.Errorf("mount processes survived teardown: %v", survivors)
}

func verifyMountProcessSet(root string, handles, transients map[int]*mountProcessHandle, authorization *mountProcessAuthorization, waitForTransients bool) error {
	pids, err := mountProcessIDs(root)
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
				child, isChild, childErr = trackShortLivedMountProcess(pid, root, authorization)
				if childErr != nil {
					return fmt.Errorf("cannot verify Mount child process %d during teardown: %w", pid, childErr)
				}
				if isChild {
					transients[pid] = child
				}
			}
			if child != nil {
				alive, childErr := verifyMountProcessHandleIfAlive(pid, child)
				if childErr != nil {
					return childErr
				}
				if alive && waitForTransients {
					if err := waitForMountProcessExit(pid, root, child); err != nil {
						return err
					}
				}
				continue
			}
			alive, aliveErr := unretainedMountProcessAlive(pid)
			if aliveErr != nil {
				return fmt.Errorf("cannot verify Mount process %d during teardown: %w", pid, aliveErr)
			}
			if alive {
				return fmt.Errorf("unverified process %d appeared in the Mount during teardown; preserving its work", pid)
			}
			continue
		}
		if _, err := verifyMountProcessHandleIfAlive(pid, handle); err != nil {
			return err
		}
	}
	return nil
}

func waitForMountProcessExit(pid int, root string, handle *mountProcessHandle) error {
	deadline := time.Now().Add(300 * time.Millisecond)
	for {
		alive, err := verifyMountProcessHandleIfAlive(pid, handle)
		if err != nil {
			return err
		}
		if !alive || !processInMount(pid, root) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("short-lived process %d remained in the Mount after retained pane processes stopped; preserving its work", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func mountHandlesInMount(handles map[int]*mountProcessHandle, root string) ([]*mountProcessHandle, error) {
	var alive []*mountProcessHandle
	for _, handle := range handles {
		stillAlive, err := handle.Alive()
		if err != nil {
			return nil, fmt.Errorf("check Mount process %d: %w", handle.PID(), err)
		}
		if stillAlive && processInMount(handle.PID(), root) {
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

func stopTaskScratchProcessesAuthorized(home string, project store.Project, task store.Task, authorization *mountProcessAuthorization) ([]string, error) {
	root, err := validatedTaskScratchRoot(home, project, task)
	if err != nil {
		return nil, err
	}
	return stopAuthorizedOwnedProcesses([]ownedProcessRoot{{path: root, includeOpenFiles: true, includeMappedFiles: true}}, authorization)
}
