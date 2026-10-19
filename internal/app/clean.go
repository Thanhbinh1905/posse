package app

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

type cleanTaskItem struct {
	project      store.Project
	task         store.Task
	mount        store.Mount
	scratchBytes int64
	mountBytes   int64
}

func (s *Service) clean(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("clean", args, map[string]flagSpec{"yes": {boolean: true}, "dry-run": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("clean does not take positional arguments")
	}
	if parsed.Bool("yes") && parsed.Bool("dry-run") {
		return axi.Usage("clean cannot combine --yes and --dry-run")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	role := s.configCallerRole(ctx.Context, db)
	var projects []store.Project
	switch role {
	case "user":
		projects, err = db.Projects(ctx.Context)
	case "lead":
		project, projectErr := s.projectForCWD(ctx.Context, db)
		if projectErr != nil {
			return projectErr
		}
		if err := s.requireLead(ctx.Context, db, project); err != nil {
			return err
		}
		projects = []store.Project{project}
	default:
		return axi.Failure("user_only", "clean is reserved for the Lead or User", false)
	}
	if err != nil {
		return err
	}

	items := []cleanTaskItem{}
	for _, project := range projects {
		tasks, err := db.Tasks(ctx.Context, project.ID, true)
		if err != nil {
			return err
		}
		for _, task := range tasks {
			if !cleanableTaskState(task.State) {
				continue
			}
			scratchPath, err := taskScratchPath(home, project, task)
			if err != nil {
				return err
			}
			scratchRoot, err := validatedTaskScratchRoot(home, project, task)
			if err != nil {
				return err
			}
			if filepath.Clean(scratchRoot) != filepath.Clean(scratchPath) {
				return fmt.Errorf("task scratch root resolved unexpectedly")
			}
			scratchBytes, err := pathBytes(scratchRoot)
			if err != nil {
				return err
			}
			item := cleanTaskItem{project: project, task: task, scratchBytes: scratchBytes}
			scratchProcesses, err := taskScratchProcessDetails(home, project, task)
			if err != nil {
				return err
			}
			scratchHasProcesses := len(scratchProcesses) > 0
			mount, mountErr := db.MountByTask(ctx.Context, task.ID)
			if mountErr == nil {
				item.mount = mount
				if (mount.State == "held" || mount.State == "releasing") && safeManagedMountPath(home, project.Name, mount.Path) {
					item.mountBytes, err = pathBytes(mount.Path)
					if err != nil {
						return err
					}
				}
			} else if !store.IsNotFound(mountErr) {
				return mountErr
			}
			if task.State != store.StateTornDown || scratchBytes > 0 || scratchHasProcesses || len(taskContainmentProcessDetails(task)) > 0 || (item.mount.State == "held" || item.mount.State == "releasing") {
				items = append(items, item)
			}
		}
	}

	rows := make([]any, 0, len(items))
	for _, item := range items {
		processes := taskContainmentProcessDetails(item.task)
		if scratchProcesses, scratchErr := taskScratchProcessDetails(home, item.project, item.task); scratchErr == nil {
			processes = mergeTaskProcessDetails(processes, scratchProcesses)
		}
		if item.task.WorktreePath != "" {
			mountProcesses, mountErr := mountTaskProcessDetails(item.task.WorktreePath)
			if mountErr == nil {
				processes = mergeTaskProcessDetails(processes, mountProcesses)
			}
		}
		processRows := make([]any, 0, len(processes))
		for _, process := range processes {
			processRows = append(processRows, map[string]any{"pid": process.PID, "command": process.Command, "ports": process.Ports})
		}
		mountBytes := int64(0)
		if (item.mount.State == "held" || item.mount.State == "releasing") && safeManagedMountPath(home, item.project.Name, item.mount.Path) {
			mountBytes = item.mountBytes
		}
		rows = append(rows, map[string]any{
			"project": item.project.Name, "task": taskIDString(item.task.Seq), "title": item.task.Title,
			"state": item.task.State, "scratch_bytes": item.scratchBytes, "mount_bytes": mountBytes,
			"mount_state": item.mount.State, "processes": processRows,
			"result": map[bool]string{true: "pending", false: "dry_run"}[parsed.Bool("yes")],
		})
	}

	if parsed.Bool("yes") {
		for index, item := range items {
			if item.task.State == store.StateTornDown {
				err = s.cleanTornDownTask(ctx.Context, db, home, item.project, item.task, item.mount)
			} else {
				cfg, configErr := config.Load(home, item.project.Name)
				if configErr != nil {
					err = configError(configErr)
				} else {
					_, err = s.unsaddleTask(ctx.Context, db, item.project, cfg, item.task, false, "")
				}
			}
			row := rows[index].(map[string]any)
			if err != nil {
				if noticeErr := s.recordTeardownBlocked(ctx.Context, db, item.project, item.task, err); noticeErr != nil {
					err = fmt.Errorf("%w; record teardown_blocked Notice: %v", err, noticeErr)
				}
				row["result"] = "blocked"
				row["detail"] = err.Error()
				err = nil
			} else {
				row["result"] = "cleaned"
			}
		}
	}
	help := "Run `posse clean --yes` to unsaddle finished Tasks, stop their recorded processes, and reclaim their Task scratch and held Mounts"
	return ctx.Print(axi.Object{{Key: "clean", Value: rows}, {Key: "dry_run", Value: !parsed.Bool("yes")}, {Key: "help", Value: []any{help}}})
}

func cleanableTaskState(state store.State) bool {
	return state == store.StateReported || state == store.StateLanded || state == store.StateTornDown
}

func (s *Service) cleanTornDownTask(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task, mount store.Mount) error {
	if _, err := stopTaskProcessContainment(ctx, task); err != nil {
		return err
	}
	if mount.ID != 0 && mount.TaskID == task.ID && (mount.State == "held" || mount.State == "releasing") {
		cfg, err := config.Load(home, project.Name)
		if err != nil {
			return configError(err)
		}
		if !safeManagedMountPath(home, project.Name, mount.Path) || filepath.Clean(task.WorktreePath) != filepath.Clean(mount.Path) {
			return fmt.Errorf("refuse to clean Task %s Mount without a validated path", taskIDString(task.Seq))
		}
		if _, err := s.releaseMountWithAuthorization(ctx, db, project, task, cfg.Remuda.Clean, false, nil); err != nil {
			return err
		}
	}
	if err := verifyNoTaskOwnedProcesses(ctx, db, home, project, task); err != nil {
		return err
	}
	if err := removeTaskScratch(home, project, task); err != nil {
		return err
	}
	return nil
}
