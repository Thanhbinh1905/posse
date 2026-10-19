package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

const (
	watchRuntimeProjectEnv = "POSSE_WATCH_PROJECT_ID"
	watchRuntimeTokenEnv   = "POSSE_WATCH_TOKEN"
	watchRuntimeStartedEnv = "POSSE_WATCH_STARTED_AT"
)

var watchStartupGracePeriod = 5 * time.Second
var watchRuntimeHeartbeatInterval = 5 * time.Second
var watchRuntimeStaleAfter = 30 * time.Second

func watchRuntimeLogPath(home string, project store.Project) string {
	return filepath.Join(home, "projects", project.Name, "watch.log")
}

func watchRuntimeLockPath(home string, project store.Project) string {
	return filepath.Join(home, "projects", project.Name, "watch.lock")
}

func watchRuntimeFailureSummary(project store.Project, reason string) string {
	return fmt.Sprintf("Watch runtime for %s is unhealthy (%s); recovery was attempted", project.Name, reason)
}

func watchExecutableAt(path string) (watchExecutableIdentity, bool) {
	if path == "" || !watchExecutableVerifier(path) {
		return watchExecutableIdentity{}, false
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return watchExecutableIdentity{}, false
	}
	if canonical, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = canonical
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return watchExecutableIdentity{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 {
		return watchExecutableIdentity{}, false
	}
	return watchExecutableIdentity{Path: resolved, Device: uint64(stat.Dev), Inode: stat.Ino}, true
}

func currentWatchExecutable(home string) (watchExecutableIdentity, bool) {
	if manifest, found, err := readSetupManifest(filepath.Join(home, setupManifestName)); err == nil && found && manifest.Binary != "" {
		if identity, ok := watchExecutableAt(manifest.Binary); ok {
			return identity, true
		}
	}
	path, err := os.Executable()
	if err != nil {
		return watchExecutableIdentity{}, false
	}
	return watchExecutableAt(path)
}

func watchProcessMatchesWatch(process watchProcess, projectID int64, watch store.WatchRuntime) bool {
	return process.PID == watch.PID && process.WatchProjectID == projectID && process.WatchToken == watch.Token && process.WatchStartedAt == watch.StartedAt
}

func (s *Service) ensureWatchRuntime(ctx context.Context, db *store.DB, home string, project store.Project) error {
	if project.IsDown() || project.HerdrWorkspaceID == "" || os.Getenv(watchRuntimeTokenEnv) != "" {
		return nil
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	if !watchExecutableVerifier(binary) {
		return nil
	}
	lockPath := watchRuntimeLockPath(home, project)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return fmt.Errorf("create Watch state directory: %w", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open Watch lock: %w", err)
	}
	defer lock.Close()
	if err := lockWatchRuntime(ctx, lock); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	fresh, err := db.ProjectByID(ctx, project.ID)
	if err != nil {
		return err
	}
	if fresh.IsDown() || fresh.HerdrWorkspaceID == "" {
		return nil
	}
	project = fresh
	state, err := db.WatchRuntime(ctx, project.ID)
	if err != nil {
		return err
	}
	processes, err := systemWatchProcesses(home)
	if err != nil {
		return err
	}
	var projectProcesses []watchProcess
	for _, process := range processes {
		if process.PollOnly && (process.WatchProjectID == project.ID || process.WatchProjectID == 0 && processWorkingInProject(process, project)) {
			projectProcesses = append(projectProcesses, process)
		}
	}

	var stored *watchProcess
	if state.PID > 0 {
		for index := range projectProcesses {
			process := &projectProcesses[index]
			if watchProcessMatchesWatch(*process, project.ID, state) && watchPIDRunning(process.PID) {
				stored = process
				break
			}
		}
	}
	if stored != nil && time.Since(time.UnixMilli(state.HeartbeatAt)) > watchRuntimeStaleAfter {
		// A wall-clock heartbeat can look old immediately after suspend. Give a
		// live process time to publish fresh progress before declaring it stale.
		refreshed, err := waitForWatchRuntimeHeartbeat(ctx, db, project.ID, state)
		if err != nil {
			return err
		}
		if refreshed {
			state, err = db.WatchRuntime(ctx, project.ID)
			if err != nil {
				return err
			}
		}
	}
	if stored != nil && time.Since(time.UnixMilli(state.HeartbeatAt)) <= watchRuntimeStaleAfter {
		return stopDuplicateWatchRuntimes(ctx, home, projectProcesses, stored.PID)
	}

	if state.PID > 0 {
		if stored != nil {
			reason := "stale heartbeat"
			if _, err := db.RaiseWatchRuntimeFailure(ctx, project.ID, state.Token, watchRuntimeFailureSummary(project, reason), currentTime()); err != nil {
				return err
			}
			if err := stopStoredWatchRuntime(ctx, home, *stored); err != nil {
				return fmt.Errorf("stop stale Project Watch: %w", err)
			}
		}
		// Missing process rows are common after a reboot and do not prove that
		// the watch was unhealthy while the machine was running.
		if err := db.ClearWatchRuntime(ctx, project.ID, state.PID, state.Token); err != nil {
			return err
		}
	}

	// Adopt a live orphan if a parent exited between Start and persisting its
	// PID. Only the process's private Project id and random token identify it.
	for _, process := range projectProcesses {
		if process.WatchToken == "" && watchPIDRunning(process.PID) {
			if err := stopStoredWatchRuntime(ctx, home, process); err != nil {
				return fmt.Errorf("stop legacy Project Watch: %w", err)
			}
		}
	}
	for _, candidate := range projectProcesses {
		if candidate.WatchToken == "" || candidate.PID < 1 || !watchPIDRunning(candidate.PID) {
			continue
		}
		startedAt := candidate.WatchStartedAt
		if startedAt == 0 {
			startedAt = currentTime()
		}
		record := store.WatchRuntime{
			PID: candidate.PID, Token: candidate.WatchToken, StartedAt: startedAt, HeartbeatAt: currentTime(),
			ExecutablePath: candidate.Executable.Path, ExecutableDevice: candidate.Executable.Device, ExecutableInode: candidate.Executable.Inode,
		}
		if err := db.SetWatchRuntime(ctx, project.ID, record); err != nil {
			return err
		}
		return stopDuplicateWatchRuntimes(ctx, home, projectProcesses, candidate.PID)
	}

	return s.startWatchRuntime(ctx, db, home, project, binary)
}

func waitForWatchRuntimeHeartbeat(ctx context.Context, db *store.DB, projectID int64, stale store.WatchRuntime) (bool, error) {
	grace := 2 * watchRuntimeHeartbeatInterval
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := db.WatchRuntime(ctx, projectID)
		if err != nil {
			return false, err
		}
		if state.PID != stale.PID || state.Token != stale.Token {
			return false, nil
		}
		if state.HeartbeatAt > stale.HeartbeatAt && time.Since(time.UnixMilli(state.HeartbeatAt)) <= watchRuntimeStaleAfter {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, nil
		case <-ticker.C:
		}
	}
}

func lockWatchRuntime(ctx context.Context, file *os.File) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("lock Project Watch: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func stopDuplicateWatchRuntimes(ctx context.Context, home string, processes []watchProcess, keepPID int) error {
	for _, process := range processes {
		if process.PID == keepPID || !watchPIDRunning(process.PID) {
			continue
		}
		if err := stopStoredWatchRuntime(ctx, home, process); err != nil {
			return fmt.Errorf("stop duplicate Project Watch: %w", err)
		}
	}
	return nil
}

func stopStoredWatchRuntime(ctx context.Context, home string, process watchProcess) error {
	if !watchPIDRunning(process.PID) {
		return nil
	}
	if process.Executable.Inode == 0 {
		return fmt.Errorf("refusing to signal Watch PID %d with an unknown executable identity", process.PID)
	}
	owned, err := storedWatchProcessIsCurrent(home, process)
	if err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("refusing to signal Watch PID %d after its process identity changed", process.PID)
	}
	if err := syscall.Kill(process.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(watchStopGracePeriod)
	for time.Now().Before(deadline) && watchPIDRunning(process.PID) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if watchPIDRunning(process.PID) {
		return fmt.Errorf("watch PID %d did not stop gracefully; refusing to interrupt Project maintenance", process.PID)
	}
	return nil
}

func storedWatchProcessIsCurrent(home string, expected watchProcess) (bool, error) {
	processes, err := systemWatchProcesses(home)
	if err != nil {
		return false, err
	}
	for _, process := range processes {
		if process.PID == expected.PID && process.WatchProjectID == expected.WatchProjectID && process.WatchToken == expected.WatchToken && process.WatchStartedAt == expected.WatchStartedAt && process.Executable.Device == expected.Executable.Device && process.Executable.Inode == expected.Executable.Inode {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) startWatchRuntime(ctx context.Context, db *store.DB, home string, project store.Project, binary string) error {
	identity, ok := watchExecutableAt(binary)
	if !ok {
		return fmt.Errorf("cannot verify Posse executable %q", binary)
	}
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		return fmt.Errorf("create Watch identity: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	startedAt := currentTime()
	logPath := watchRuntimeLogPath(home, project)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return fmt.Errorf("create Project Watch log directory: %w", err)
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open Project Watch log: %w", err)
	}
	command := exec.Command(binary, "watch", "--poll-only")
	command.Dir = project.Root
	command.Env = watchRuntimeEnvironment(os.Environ(), home, project, token, startedAt)
	command.Stdout, command.Stderr = log, log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		log.Close()
		return fmt.Errorf("start hidden Project Watch: %w", err)
	}
	_ = log.Close()
	watch := store.WatchRuntime{
		PID: command.Process.Pid, Token: token, StartedAt: startedAt, HeartbeatAt: startedAt,
		ExecutablePath: identity.Path, ExecutableDevice: identity.Device, ExecutableInode: identity.Inode,
	}
	if err := db.SetWatchRuntime(ctx, project.ID, watch); err != nil {
		_ = command.Process.Kill()
		_ = command.Process.Release()
		return fmt.Errorf("record hidden Project Watch: %w", err)
	}
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("release hidden Project Watch process: %w", err)
	}
	return nil
}

func watchRuntimeEnvironment(parent []string, home string, project store.Project, token string, startedAt int64) []string {
	values := make(map[string]string, len(parent)+5)
	for _, entry := range parent {
		key, value, found := strings.Cut(entry, "=")
		if !found || key == "" || key == watchRuntimeProjectEnv || key == watchRuntimeTokenEnv || key == watchRuntimeStartedEnv || key == "POSSE_HOME" || key == "HERDR_PANE_ID" || key == "HERDR_WORKSPACE_ID" || key == "HERDR_TAB_ID" || key == "HERDR_ENV" || key == "HERDR_PLUGIN_EVENT" || key == "HERDR_PLUGIN_EVENT_JSON" {
			continue
		}
		values[key] = value
	}
	values["POSSE_HOME"] = home
	values[watchRuntimeProjectEnv] = strconv.FormatInt(project.ID, 10)
	values[watchRuntimeTokenEnv] = token
	values[watchRuntimeStartedEnv] = strconv.FormatInt(startedAt, 10)
	values["PWD"] = project.Root
	env := make([]string, 0, len(values))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}

func (s *Service) superviseWatchRuntimes(ctx *axi.Context, command string, args []string) {
	// A Notice acknowledgement is delivery work, not a Project maintenance boundary.
	if os.Getenv(watchRuntimeTokenEnv) != "" || command == "ack" || command == "down" || command == "watch" || command == "lookout" || command == "remove" || command == "_update-preflight" {
		return
	}
	binary, err := os.Executable()
	if err != nil || !watchExecutableVerifier(binary) {
		return
	}
	home, err := s.homePath()
	if err != nil {
		return
	}
	if _, err := os.Stat(filepath.Join(home, "posse.db")); errors.Is(err, os.ErrNotExist) {
		return
	}
	db, home, err := s.openDB()
	if err != nil {
		return
	}
	defer db.Close()
	projects, err := db.Projects(ctx.Context)
	if err != nil {
		return
	}
	selected := make([]store.Project, 0, len(projects))
	if command == "" || command == "doctor" || command == "recover" && containsArgument(args, "--all") {
		selected = projects
	} else {
		dir, dirErr := currentDir()
		if dirErr == nil {
			if project, found, lookupErr := registeredProjectFor(ctx.Context, db, dir); lookupErr == nil && found {
				selected = append(selected, project)
			}
		}
		if len(selected) == 0 {
			paneID := os.Getenv("HERDR_PANE_ID")
			if paneID != "" {
				if task, taskErr := db.TaskByPane(ctx.Context, paneID); taskErr == nil {
					if project, projectErr := db.ProjectByID(ctx.Context, task.ProjectID); projectErr == nil {
						selected = append(selected, project)
					}
				} else if project, leadErr := db.ProjectByLeadPane(ctx.Context, paneID); leadErr == nil {
					selected = append(selected, project)
				}
			}
		}
	}
	for _, project := range selected {
		if project.IsDown() || project.HerdrWorkspaceID == "" {
			continue
		}
		if err := s.ensureWatchRuntime(ctx.Context, db, home, project); err != nil {
			fmt.Fprintf(ctx.ErrOut, "Watch supervision for %s failed: %v\n", project.Name, err)
		}
	}
}

func containsArgument(args []string, value string) bool {
	for _, arg := range args {
		if arg == value {
			return true
		}
	}
	return false
}

func (s *Service) runProjectWatchRuntime(ctx *axi.Context, db *store.DB, project store.Project, timeout time.Duration, stopSignals <-chan os.Signal) error {
	token := os.Getenv(watchRuntimeTokenEnv)
	if token == "" {
		return s.runWatchLoop(ctx, db, project, timeout, stopSignals, nil)
	}
	projectID, err := strconv.ParseInt(os.Getenv(watchRuntimeProjectEnv), 10, 64)
	if err != nil || projectID != project.ID {
		return axi.Failure("watch_project_mismatch", "hidden Watch identity does not match this Project", false)
	}
	startedAt, err := strconv.ParseInt(os.Getenv(watchRuntimeStartedEnv), 10, 64)
	if err != nil || startedAt < 1 {
		return axi.Failure("watch_identity_invalid", "hidden Watch start identity is invalid", false)
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	identity, ok := watchExecutableAt(binary)
	if !ok {
		return fmt.Errorf("cannot identify hidden Watch executable %q", binary)
	}
	watch := store.WatchRuntime{PID: os.Getpid(), Token: token, StartedAt: startedAt, HeartbeatAt: currentTime(), ExecutablePath: identity.Path, ExecutableDevice: identity.Device, ExecutableInode: identity.Inode}
	if err := db.SetWatchRuntime(ctx.Context, project.ID, watch); err != nil {
		return err
	}
	defer db.ClearWatchRuntime(context.Background(), project.ID, os.Getpid(), token)
	return s.runWatchLoop(ctx, db, project, timeout, stopSignals, &watch)
}

func (s *Service) runWatchLoop(ctx *axi.Context, db *store.DB, project store.Project, timeout time.Duration, stopSignals <-chan os.Signal, watch *store.WatchRuntime) error {
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	lastFailure := ""
	watchStopped := make(chan struct{}, 1)
	if watch != nil {
		heartbeatStop := make(chan struct{})
		go func() {
			ticker := time.NewTicker(watchRuntimeHeartbeatInterval)
			defer ticker.Stop()
			for {
				select {
				case <-heartbeatStop:
					return
				case <-ticker.C:
					ok, err := db.HeartbeatWatchRuntime(ctx.Context, project.ID, watch.PID, watch.Token, currentTime())
					if err != nil {
						fmt.Fprintf(os.Stderr, "Watch heartbeat failed for %s: %v\n", project.Name, err)
						continue
					}
					if !ok {
						select {
						case watchStopped <- struct{}{}:
						default:
						}
						return
					}
					if err := s.reexecWatchIfUpdated(project.ID, watch); err != nil {
						fmt.Fprintf(os.Stderr, "Watch re-exec check failed for %s: %v\n", project.Name, err)
					}
				}
			}
		}()
		defer close(heartbeatStop)
	}
	for {
		select {
		case <-watchStopped:
			return nil
		default:
		}
		select {
		case sig := <-stopSignals:
			if sig == syscall.SIGTERM {
				return s.reportWatchStopped(ctx, project, true)
			}
			if err := s.reexecWatchIfUpdated(project.ID, watch); err != nil {
				return err
			}
		default:
		}
		fresh, err := db.ProjectByID(ctx.Context, project.ID)
		if err != nil {
			return err
		}
		if fresh.IsDown() || fresh.Root != project.Root {
			return nil
		}
		if _, err := s.prepareProjectWatchObservation(ctx.Context, db, fresh); err != nil && !errors.Is(err, errRecoveryHeld) && !errors.Is(err, errRecoveryDeferred) {
			recordWatchFailure(ctx, db, project.ID, &lastFailure, "reconcile", err)
		}
		if err := s.maintainProjectWatch(ctx.Context, db, fresh); err != nil {
			recordWatchFailure(ctx, db, project.ID, &lastFailure, "watch", err)
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return nil
		}
		select {
		case sig := <-stopSignals:
			if sig == syscall.SIGTERM {
				return s.reportWatchStopped(ctx, project, true)
			}
			if err := s.reexecWatchIfUpdated(project.ID, watch); err != nil {
				return err
			}
		case <-ctx.Context.Done():
			return ctx.Context.Err()
		case <-watchStopped:
			return nil
		case <-time.After(2 * time.Second):
			if watch == nil {
				if err := s.handleLeadWatchUpdate(ctx, project); err != nil {
					return err
				}
			}
		}
	}
}

func (s *Service) handleLeadWatchUpdate(ctx *axi.Context, project store.Project) error {
	home, err := s.homePath()
	if err != nil {
		return err
	}
	if !watchUpdatePending(home, os.Getpid()) {
		return nil
	}
	if len(os.Args) > 1 && os.Args[1] == "_claude-stop" {
		return s.reportWatchStopped(ctx, project, false)
	}
	return s.reexecWatchIfUpdated(project.ID, nil)
}

func watchReexecRequested(home string, pid int) bool {
	_, err := os.Stat(watchReexecMarker(home, pid))
	return err == nil
}

func watchUpdatePending(home string, pid int) bool {
	if watchReexecRequested(home, pid) {
		return true
	}
	installed, installedOK := currentWatchExecutable(home)
	running, runningOK := inspectWatchExecutable(pid)
	return installedOK && runningOK && (installed.Device != running.Device || installed.Inode != running.Inode)
}

func (s *Service) reexecWatchIfUpdated(projectID int64, watch *store.WatchRuntime) error {
	home, err := s.homePath()
	if err != nil {
		return err
	}
	marker := watchReexecMarker(home, os.Getpid())
	requested := false
	if _, err := os.Stat(marker); err == nil {
		requested = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	installed, installedOK := currentWatchExecutable(home)
	running, runningOK := inspectWatchExecutable(os.Getpid())
	if installedOK && runningOK && (installed.Device != running.Device || installed.Inode != running.Inode) {
		requested = true
	}
	if !requested {
		return nil
	}
	if !installedOK {
		return fmt.Errorf("cannot verify the installed Posse executable")
	}
	if watch != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if db, _, openErr := s.openDB(); openErr == nil {
			updated := *watch
			updated.ExecutablePath, updated.ExecutableDevice, updated.ExecutableInode = installed.Path, installed.Device, installed.Inode
			updated.HeartbeatAt = currentTime()
			if setErr := db.SetWatchRuntime(ctx, projectID, updated); setErr != nil {
				_ = db.Close()
				return setErr
			}
			_ = db.Close()
			*watch = updated
		}
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	args := watchReexecArguments(os.Args)
	if len(args) == 0 {
		args = []string{installed.Path}
	} else {
		args[0] = installed.Path
	}
	execProcess := syscall.Exec
	if s.reexecUpdate != nil {
		execProcess = s.reexecUpdate
	}
	if err := execProcess(installed.Path, args, os.Environ()); err != nil {
		return fmt.Errorf("re-exec updated Watch: %w", err)
	}
	return errors.New("watch re-exec returned unexpectedly")
}

func watchReexecArguments(original []string) []string {
	args := make([]string, 0, len(original))
	for index := 0; index < len(original); index++ {
		arg := original[index]
		if arg == "--ack" || arg == "--requeue" {
			if index+1 < len(original) {
				index++
			}
			continue
		}
		if strings.HasPrefix(arg, "--ack=") || strings.HasPrefix(arg, "--requeue=") {
			continue
		}
		args = append(args, arg)
	}
	return args
}

func watchReexecMarker(home string, pid int) string {
	return filepath.Join(home, "watch-reexec", strconv.Itoa(pid))
}

func sameWatchExecutableIdentity(left, right watchExecutableIdentity) bool {
	return left.Device == right.Device && left.Inode != 0 && left.Inode == right.Inode
}

func requestWatchReexec(home string, processes []watchProcess) error {
	if len(processes) == 0 {
		return nil
	}
	directory := filepath.Join(home, "watch-reexec")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	installed, installedOK := currentWatchExecutable(home)
	var failures []error
	for _, process := range processes {
		if !watchPIDRunning(process.PID) {
			continue
		}
		current, currentOK := inspectWatchExecutable(process.PID)
		if !currentOK {
			failures = append(failures, fmt.Errorf("cannot inspect Watch PID %d after update", process.PID))
			continue
		}
		if installedOK && sameWatchExecutableIdentity(current, installed) {
			continue // The process already re-execed while the update was installing.
		}
		if process.Executable.Inode == 0 || !sameWatchExecutableIdentity(current, process.Executable) {
			failures = append(failures, fmt.Errorf("refusing to request re-exec for Watch PID %d with a changed executable identity", process.PID))
			continue
		}
		marker := watchReexecMarker(home, process.PID)
		if err := os.WriteFile(marker, []byte("update\n"), 0o600); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
