package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

// workerHomeEnv is set in every Worker pane to the posse home that owns it.
const workerHomeEnv = "POSSE_WORKER_HOME"

// workerCommands are the only commands a Worker may run against its own posse
// home.
var workerCommands = map[string]bool{
	"brief":         true,
	"holler":        true,
	"publish":       true,
	"_context":      true,
	"config schema": true,
	"config show":   true,
}

// guardWorkers wraps every handler in the tree so a Worker cannot run
// state-changing commands against the posse home it belongs to.
func (s *Service) guardWorkers(command *axi.Command, path []string) {
	if command.Handler != nil {
		name := strings.Join(path, " ")
		next := command.Handler
		command.Handler = func(ctx *axi.Context, args []string) error {
			if err := s.refuseWorker(ctx.Context, name); err != nil {
				return err
			}
			return next(ctx, args)
		}
	}
	for _, sub := range command.Subcommands {
		s.guardWorkers(sub, append(append([]string(nil), path...), sub.Name))
	}
}

func (s *Service) refuseWorker(ctx context.Context, command string) error {
	if workerCommands[command] {
		return nil
	}
	home, err := s.homePath()
	if err != nil {
		return nil
	}
	if !s.workerCaller(ctx, home) {
		return nil
	}
	name := "posse"
	if command != "" {
		name += " " + command
	}
	return axi.Failure("worker_forbidden",
		"`"+name+"` cannot run from a Rider against the posse home "+home+"; Riders may run only `posse brief`, `posse holler`, `posse publish` and `posse config show|schema` there",
		false,
		"Experiment with posse under a temporary POSSE_HOME and an isolated Herdr server",
		"Report what the Lead should do with `posse holler needs-decision`")
}

// workerCaller reports whether this process runs on behalf of a Worker of
// home. It is a Worker when it, or any process it descends from, carries
// POSSE_WORKER_HOME for home in its environment or runs inside one of home's
// Mounts, or when its HERDR_PANE_ID is a Task's pane. Ancestors are read from
// /proc, so `env -u`, `cd` or a subshell cannot shed the identity: the agent
// process itself keeps the marker it was started with and its Mount cwd.
func (s *Service) workerCaller(ctx context.Context, home string) bool {
	if value := os.Getenv(workerHomeEnv); value != "" && samePath(value, home) {
		return true
	}
	ancestors := processAncestors(procRoot, os.Getpid())
	for _, process := range ancestors {
		if value, ok := process.env[workerHomeEnv]; ok && value != "" && samePath(value, home) {
			return true
		}
	}
	db, err := store.OpenReadOnly(home)
	if err != nil {
		return false
	}
	defer db.Close()
	if s.herdrContext != nil && s.herdrContext() {
		if paneID := os.Getenv("HERDR_PANE_ID"); paneID != "" {
			if _, err := db.TaskByPane(ctx, paneID); err == nil {
				return true
			}
		}
	}
	mounts := homeMounts(ctx, db)
	if cwd, err := os.Getwd(); err == nil && insideAny(cwd, mounts) {
		return true
	}
	for _, process := range ancestors {
		if process.cwd != "" && insideAny(process.cwd, mounts) {
			return true
		}
	}
	return false
}

// cwdInsideMount reports whether the working directory is inside any Mount.
func cwdInsideMount(ctx context.Context, db *store.DB) bool {
	cwd, err := os.Getwd()
	return err == nil && insideAny(cwd, homeMounts(ctx, db))
}

func homeMounts(ctx context.Context, db *store.DB) []string {
	projects, err := db.Projects(ctx)
	if err != nil {
		return nil
	}
	paths := []string{}
	for _, project := range projects {
		mounts, err := db.Mounts(ctx, project.ID)
		if err != nil {
			continue
		}
		for _, mount := range mounts {
			paths = append(paths, mount.Path)
		}
	}
	return paths
}

func insideAny(path string, roots []string) bool {
	for _, root := range roots {
		if pathInside(path, root) {
			return true
		}
	}
	return false
}

// procRoot is where process information is read; tests point it elsewhere.
var procRoot = "/proc"

type ancestorProcess struct {
	pid int
	cwd string
	env map[string]string
}

// processAncestors returns the parent chain of pid, nearest first, with each
// process's working directory and the environment it was started with. It is
// empty where /proc is unavailable.
func processAncestors(root string, pid int) []ancestorProcess {
	ancestors := []ancestorProcess{}
	for depth := 0; depth < 64; depth++ {
		parent := parentPID(root, pid)
		if parent <= 1 {
			return ancestors
		}
		process := ancestorProcess{pid: parent, env: map[string]string{}}
		directory := filepath.Join(root, strconv.Itoa(parent))
		if cwd, err := os.Readlink(filepath.Join(directory, "cwd")); err == nil {
			process.cwd = strings.TrimSuffix(cwd, " (deleted)")
		}
		if environ, err := os.ReadFile(filepath.Join(directory, "environ")); err == nil {
			for _, entry := range strings.Split(string(environ), "\x00") {
				if name, value, ok := strings.Cut(entry, "="); ok && (name == workerHomeEnv || name == "HERDR_PANE_ID") {
					process.env[name] = value
				}
			}
		}
		ancestors = append(ancestors, process)
		pid = parent
	}
	return ancestors
}

func parentPID(root string, pid int) int {
	stat, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return 0
	}
	fields := strings.Fields(string(stat)[end+1:])
	if len(fields) < 2 {
		return 0
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return parent
}

func samePath(left, right string) bool {
	if filepath.Clean(left) == filepath.Clean(right) {
		return true
	}
	leftResolved, leftErr := filepath.EvalSymlinks(left)
	rightResolved, rightErr := filepath.EvalSymlinks(right)
	return leftErr == nil && rightErr == nil && leftResolved == rightResolved
}
