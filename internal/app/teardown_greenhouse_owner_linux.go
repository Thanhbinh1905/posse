//go:build linux

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/greenhouse"
	"github.com/thanhbinh1905/posse/internal/store"
)

func stopTaskGreenhouseReviewOwners(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task) error {
	repositories := []string{project.Name}
	if project.IsWorkspace() {
		members, err := db.TaskRepos(ctx, task.ID)
		if err != nil {
			return err
		}
		repositories = repositories[:0]
		for _, member := range members {
			repositories = append(repositories, member.Repo)
		}
	}
	for _, repository := range repositories {
		roundDir := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), "greenhouse")
		ownerPath := parallelReviewOwnerPath(roundDir, repository)
		owner, err := readParallelReviewOwner(ownerPath)
		if err != nil {
			return err
		}
		if owner == nil {
			continue
		}
		if owner.PID < 2 {
			return fmt.Errorf("refusing unsafe Greenhouse review owner PID %d", owner.PID)
		}
		pids, err := processIDsInRoots([]ownedProcessRoot{{path: roundDir, includeOpenFiles: true, includeMappedFiles: true}})
		if err != nil {
			return err
		}
		found := false
		for _, pid := range pids {
			found = found || pid == owner.PID
		}
		if !found {
			lockPath := parallelReviewLockPath(roundDir, repository)
			inFlight, err := parallelReviewInFlight(lockPath)
			if err != nil {
				return err
			}
			if inFlight {
				return fmt.Errorf("greenhouse review lock for Task %s is held by an unverified owner", taskIDString(task.Seq))
			}
			current, err := readParallelReviewOwner(ownerPath)
			if err != nil {
				return err
			}
			if current != nil && current.PID == owner.PID {
				if err := os.Remove(ownerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			if err := greenhouse.ClearPending(filepath.Join(roundDir, repository+".pending.json")); err != nil {
				return err
			}
			continue
		}
		args, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(owner.PID), "cmdline"))
		if err != nil {
			if processGone(err) {
				continue
			}
			return err
		}
		argv := strings.Split(string(args), "\x00")
		if !greenhouseOwnerArgument(argv, "_greenhouse") || !greenhouseOwnerArgument(argv, "review") || !greenhouseOwnerArgument(argv, taskIDString(task.Seq)) {
			return fmt.Errorf("greenhouse owner PID %d does not match Task %s review command", owner.PID, taskIDString(task.Seq))
		}
		handle, err := openSandboxProcessHandle(owner.PID)
		if err != nil {
			if processGone(err) {
				continue
			}
			return err
		}
		if err := handle.Signal(syscall.SIGTERM); err != nil {
			_ = handle.Close()
			return err
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			alive, aliveErr := handle.Alive()
			if aliveErr != nil || !alive {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		alive, aliveErr := handle.Alive()
		if aliveErr != nil {
			_ = handle.Close()
			return aliveErr
		}
		if alive {
			if err := handle.Signal(syscall.SIGKILL); err != nil {
				_ = handle.Close()
				return err
			}
			for time.Now().Before(deadline.Add(2 * time.Second)) {
				alive, aliveErr = handle.Alive()
				if aliveErr != nil || !alive {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			if aliveErr != nil || alive {
				_ = handle.Close()
				if aliveErr != nil {
					return aliveErr
				}
				return fmt.Errorf("greenhouse review owner PID %d survived teardown", owner.PID)
			}
		}
		_ = handle.Close()
		current, err := readParallelReviewOwner(ownerPath)
		if err != nil {
			return err
		}
		if current != nil && current.PID == owner.PID {
			if err := os.Remove(ownerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := greenhouse.ClearPending(filepath.Join(roundDir, repository+".pending.json")); err != nil {
			return err
		}
	}
	return nil
}

func greenhouseOwnerArgument(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
