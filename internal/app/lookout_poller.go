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
	lookoutPollerProjectEnv = "POSSE_LOOKOUT_PROJECT_ID"
	lookoutPollerTokenEnv   = "POSSE_LOOKOUT_TOKEN"
	lookoutPollerStartedEnv = "POSSE_LOOKOUT_STARTED_AT"
)

var lookoutStartupGracePeriod = 5 * time.Second
var lookoutPollerHeartbeatInterval = 5 * time.Second
var lookoutPollerStaleAfter = 30 * time.Second

func lookoutPollerLogPath(home string, project store.Project) string {
	return filepath.Join(home, "projects", project.Name, "lookout.log")
}

func lookoutPollerLockPath(home string, project store.Project) string {
	return filepath.Join(home, "projects", project.Name, "lookout.lock")
}

func lookoutPollerFailureSummary(project store.Project, reason string) string {
	return fmt.Sprintf("Lookout poller for %s is unhealthy (%s); recovery was attempted", project.Name, reason)
}

func lookoutExecutableAt(path string) (lookoutExecutableIdentity, bool) {
	if path == "" || !lookoutExecutableVerifier(path) {
		return lookoutExecutableIdentity{}, false
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return lookoutExecutableIdentity{}, false
	}
	if canonical, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = canonical
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return lookoutExecutableIdentity{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 {
		return lookoutExecutableIdentity{}, false
	}
	return lookoutExecutableIdentity{Path: resolved, Device: uint64(stat.Dev), Inode: stat.Ino}, true
}

func currentLookoutExecutable(home string) (lookoutExecutableIdentity, bool) {
	if manifest, found, err := readSetupManifest(filepath.Join(home, setupManifestName)); err == nil && found && manifest.Binary != "" {
		if identity, ok := lookoutExecutableAt(manifest.Binary); ok {
			return identity, true
		}
	}
	path, err := os.Executable()
	if err != nil {
		return lookoutExecutableIdentity{}, false
	}
	return lookoutExecutableAt(path)
}

func lookoutProcessMatchesPoller(process lookoutProcess, projectID int64, poller store.LookoutPoller) bool {
	return process.PID == poller.PID && process.PollerProjectID == projectID && process.PollerToken == poller.Token && process.PollerStartedAt == poller.StartedAt
}

func (s *Service) ensureLookoutPoller(ctx context.Context, db *store.DB, home string, project store.Project) error {
	if project.IsDown() || project.HerdrWorkspaceID == "" || os.Getenv(lookoutPollerTokenEnv) != "" {
		return nil
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	if !lookoutExecutableVerifier(binary) {
		return nil
	}
	lockPath := lookoutPollerLockPath(home, project)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return fmt.Errorf("create Lookout state directory: %w", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open Lookout lock: %w", err)
	}
	defer lock.Close()
	if err := lockLookoutPoller(ctx, lock); err != nil {
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
	state, err := db.LookoutPoller(ctx, project.ID)
	if err != nil {
		return err
	}
	processes, err := systemLookoutProcesses(home)
	if err != nil {
		return err
	}
	var projectProcesses []lookoutProcess
	for _, process := range processes {
		if process.PollOnly && (process.PollerProjectID == project.ID || process.PollerProjectID == 0 && processWorkingInProject(process, project)) {
			projectProcesses = append(projectProcesses, process)
		}
	}

	var healthy *lookoutProcess
	if state.PID > 0 {
		for index := range projectProcesses {
			process := &projectProcesses[index]
			if lookoutProcessMatchesPoller(*process, project.ID, state) && lookoutPIDRunning(process.PID) && time.Since(time.UnixMilli(state.HeartbeatAt)) <= lookoutPollerStaleAfter {
				healthy = process
				break
			}
		}
	}
	if healthy != nil {
		return stopDuplicateLookoutPollers(ctx, home, projectProcesses, healthy.PID)
	}

	reason := "missing poller"
	if state.PID > 0 {
		reason = "missing, stopped, or stale heartbeat"
		if _, err := db.RaiseLookoutPollerFailure(ctx, project.ID, state.Token, lookoutPollerFailureSummary(project, reason), currentTime()); err != nil {
			return err
		}
		var stale *lookoutProcess
		for index := range projectProcesses {
			process := &projectProcesses[index]
			if process.PID == state.PID && process.PollerToken == state.Token && process.PollerProjectID == project.ID && process.PollerStartedAt == state.StartedAt {
				stale = process
				break
			}
		}
		if stale != nil && lookoutPIDRunning(state.PID) {
			if err := stopStoredLookoutPoller(ctx, home, *stale); err != nil {
				return fmt.Errorf("stop stale Project Lookout: %w", err)
			}
		}
		if err := db.ClearLookoutPoller(ctx, project.ID, state.PID, state.Token); err != nil {
			return err
		}
	}

	// Adopt a live orphan if a parent exited between Start and persisting its
	// PID. Only the process's private Project id and random token identify it.
	for _, process := range projectProcesses {
		if process.PollerToken == "" && lookoutPIDRunning(process.PID) {
			if _, err := stopLookoutProcesses(ctx, home, []lookoutProcess{process}); err != nil {
				return fmt.Errorf("stop legacy Project Lookout: %w", err)
			}
		}
	}
	for _, candidate := range projectProcesses {
		if candidate.PollerToken == "" || candidate.PID < 1 || !lookoutPIDRunning(candidate.PID) {
			continue
		}
		startedAt := candidate.PollerStartedAt
		if startedAt == 0 {
			startedAt = currentTime()
		}
		record := store.LookoutPoller{
			PID: candidate.PID, Token: candidate.PollerToken, StartedAt: startedAt, HeartbeatAt: currentTime(),
			ExecutablePath: candidate.Executable.Path, ExecutableDevice: candidate.Executable.Device, ExecutableInode: candidate.Executable.Inode,
		}
		if err := db.SetLookoutPoller(ctx, project.ID, record); err != nil {
			return err
		}
		return stopDuplicateLookoutPollers(ctx, home, projectProcesses, candidate.PID)
	}

	return s.startLookoutPoller(ctx, db, home, project, binary)
}

func lockLookoutPoller(ctx context.Context, file *os.File) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("lock Project Lookout: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func stopDuplicateLookoutPollers(ctx context.Context, home string, processes []lookoutProcess, keepPID int) error {
	var duplicates []lookoutProcess
	for _, process := range processes {
		if process.PID != keepPID && lookoutPIDRunning(process.PID) {
			duplicates = append(duplicates, process)
		}
	}
	if len(duplicates) == 0 {
		return nil
	}
	_, err := stopLookoutProcesses(ctx, home, duplicates)
	return err
}

func stopStoredLookoutPoller(ctx context.Context, home string, process lookoutProcess) error {
	if !lookoutPIDRunning(process.PID) {
		return nil
	}
	if process.Executable.Inode == 0 {
		return fmt.Errorf("refusing to signal Lookout PID %d with an unknown executable identity", process.PID)
	}
	owned, err := storedPollerProcessIsCurrent(home, process)
	if err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("refusing to signal Lookout PID %d after its process identity changed", process.PID)
	}
	if err := syscall.Kill(process.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(lookoutStopGracePeriod)
	for time.Now().Before(deadline) && lookoutPIDRunning(process.PID) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if lookoutPIDRunning(process.PID) {
		owned, err := storedPollerProcessIsCurrent(home, process)
		if err != nil {
			return err
		}
		if !owned {
			return fmt.Errorf("refusing to kill Lookout PID %d after its process identity changed", process.PID)
		}
		if err := syscall.Kill(process.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return nil
}

func storedPollerProcessIsCurrent(home string, expected lookoutProcess) (bool, error) {
	processes, err := systemLookoutProcesses(home)
	if err != nil {
		return false, err
	}
	for _, process := range processes {
		if process.PID == expected.PID && process.PollerProjectID == expected.PollerProjectID && process.PollerToken == expected.PollerToken && process.PollerStartedAt == expected.PollerStartedAt && process.Executable.Device == expected.Executable.Device && process.Executable.Inode == expected.Executable.Inode {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) startLookoutPoller(ctx context.Context, db *store.DB, home string, project store.Project, binary string) error {
	identity, ok := lookoutExecutableAt(binary)
	if !ok {
		return fmt.Errorf("cannot verify Posse executable %q", binary)
	}
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		return fmt.Errorf("create Lookout identity: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	startedAt := currentTime()
	logPath := lookoutPollerLogPath(home, project)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return fmt.Errorf("create Project Lookout log directory: %w", err)
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open Project Lookout log: %w", err)
	}
	command := exec.Command(binary, "lookout", "--poll-only")
	command.Dir = project.Root
	command.Env = lookoutPollerEnvironment(os.Environ(), home, project, token, startedAt)
	command.Stdout, command.Stderr = log, log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		log.Close()
		return fmt.Errorf("start hidden Project Lookout: %w", err)
	}
	_ = log.Close()
	poller := store.LookoutPoller{
		PID: command.Process.Pid, Token: token, StartedAt: startedAt, HeartbeatAt: startedAt,
		ExecutablePath: identity.Path, ExecutableDevice: identity.Device, ExecutableInode: identity.Inode,
	}
	if err := db.SetLookoutPoller(ctx, project.ID, poller); err != nil {
		_ = command.Process.Kill()
		_ = command.Process.Release()
		return fmt.Errorf("record hidden Project Lookout: %w", err)
	}
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("release hidden Project Lookout process: %w", err)
	}
	return nil
}

func lookoutPollerEnvironment(parent []string, home string, project store.Project, token string, startedAt int64) []string {
	values := make(map[string]string, len(parent)+5)
	for _, entry := range parent {
		key, value, found := strings.Cut(entry, "=")
		if !found || key == "" || key == lookoutPollerProjectEnv || key == lookoutPollerTokenEnv || key == lookoutPollerStartedEnv || key == "POSSE_HOME" || key == "HERDR_PANE_ID" || key == "HERDR_WORKSPACE_ID" || key == "HERDR_TAB_ID" || key == "HERDR_ENV" || key == "HERDR_PLUGIN_EVENT" || key == "HERDR_PLUGIN_EVENT_JSON" {
			continue
		}
		values[key] = value
	}
	values["POSSE_HOME"] = home
	values[lookoutPollerProjectEnv] = strconv.FormatInt(project.ID, 10)
	values[lookoutPollerTokenEnv] = token
	values[lookoutPollerStartedEnv] = strconv.FormatInt(startedAt, 10)
	values["PWD"] = project.Root
	env := make([]string, 0, len(values))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}

func (s *Service) superviseLookoutPollers(ctx *axi.Context, command string, args []string) {
	if os.Getenv(lookoutPollerTokenEnv) != "" || command == "down" || command == "lookout" || command == "remove" || command == "_update-preflight" {
		return
	}
	binary, err := os.Executable()
	if err != nil || !lookoutExecutableVerifier(binary) {
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
		if err := s.ensureLookoutPoller(ctx.Context, db, home, project); err != nil {
			fmt.Fprintf(ctx.ErrOut, "Lookout supervision for %s failed: %v\n", project.Name, err)
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

func (s *Service) runProjectLookoutPoller(ctx *axi.Context, db *store.DB, project store.Project, timeout time.Duration, stopSignals <-chan os.Signal) error {
	token := os.Getenv(lookoutPollerTokenEnv)
	if token == "" {
		return s.watchProjectLookout(ctx, db, project, timeout, stopSignals, nil)
	}
	projectID, err := strconv.ParseInt(os.Getenv(lookoutPollerProjectEnv), 10, 64)
	if err != nil || projectID != project.ID {
		return axi.Failure("lookout_project_mismatch", "hidden Lookout identity does not match this Project", false)
	}
	startedAt, err := strconv.ParseInt(os.Getenv(lookoutPollerStartedEnv), 10, 64)
	if err != nil || startedAt < 1 {
		return axi.Failure("lookout_identity_invalid", "hidden Lookout start identity is invalid", false)
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	identity, ok := lookoutExecutableAt(binary)
	if !ok {
		return fmt.Errorf("cannot identify hidden Lookout executable %q", binary)
	}
	poller := store.LookoutPoller{PID: os.Getpid(), Token: token, StartedAt: startedAt, HeartbeatAt: currentTime(), ExecutablePath: identity.Path, ExecutableDevice: identity.Device, ExecutableInode: identity.Inode}
	if err := db.SetLookoutPoller(ctx.Context, project.ID, poller); err != nil {
		return err
	}
	defer db.ClearLookoutPoller(context.Background(), project.ID, os.Getpid(), token)
	return s.watchProjectLookout(ctx, db, project, timeout, stopSignals, &poller)
}

func (s *Service) watchProjectLookout(ctx *axi.Context, db *store.DB, project store.Project, timeout time.Duration, stopSignals <-chan os.Signal, poller *store.LookoutPoller) error {
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	lastFailure := ""
	pollerStopped := make(chan struct{}, 1)
	if poller != nil {
		heartbeatStop := make(chan struct{})
		go func() {
			ticker := time.NewTicker(lookoutPollerHeartbeatInterval)
			defer ticker.Stop()
			for {
				select {
				case <-heartbeatStop:
					return
				case <-ticker.C:
					ok, err := db.HeartbeatLookoutPoller(ctx.Context, project.ID, poller.PID, poller.Token, currentTime())
					if err != nil {
						fmt.Fprintf(os.Stderr, "Lookout heartbeat failed for %s: %v\n", project.Name, err)
						continue
					}
					if !ok {
						select {
						case pollerStopped <- struct{}{}:
						default:
						}
						return
					}
					if err := s.reexecLookoutIfUpdated(project.ID, poller); err != nil {
						fmt.Fprintf(os.Stderr, "Lookout re-exec check failed for %s: %v\n", project.Name, err)
					}
				}
			}
		}()
		defer close(heartbeatStop)
	}
	for {
		select {
		case <-pollerStopped:
			return nil
		default:
		}
		select {
		case sig := <-stopSignals:
			if sig == syscall.SIGTERM {
				return s.reportLookoutStopped(ctx, project, true)
			}
			if err := s.reexecLookoutIfUpdated(project.ID, poller); err != nil {
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
		if _, err := s.prepareProjectObservation(ctx.Context, db, fresh); err != nil && !errors.Is(err, errRecoveryHeld) && !errors.Is(err, errRecoveryDeferred) {
			recordLookoutFailure(ctx, db, project.ID, &lastFailure, "reconcile", err)
		}
		if err := s.maintainProjectWatch(ctx.Context, db, fresh); err != nil {
			recordLookoutFailure(ctx, db, project.ID, &lastFailure, "watch", err)
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return nil
		}
		select {
		case sig := <-stopSignals:
			if sig == syscall.SIGTERM {
				return s.reportLookoutStopped(ctx, project, true)
			}
			if err := s.reexecLookoutIfUpdated(project.ID, poller); err != nil {
				return err
			}
		case <-ctx.Context.Done():
			return ctx.Context.Err()
		case <-pollerStopped:
			return nil
		case <-time.After(2 * time.Second):
			if poller == nil {
				if err := s.handleLeadLookoutUpdate(ctx, project); err != nil {
					return err
				}
			}
		}
	}
}

func (s *Service) handleLeadLookoutUpdate(ctx *axi.Context, project store.Project) error {
	home, err := s.homePath()
	if err != nil {
		return err
	}
	if !lookoutUpdatePending(home, os.Getpid()) {
		return nil
	}
	if len(os.Args) > 1 && os.Args[1] == "_claude-stop" {
		return s.reportLookoutStopped(ctx, project, false)
	}
	return s.reexecLookoutIfUpdated(project.ID, nil)
}

func lookoutUpdatePending(home string, pid int) bool {
	if _, err := os.Stat(lookoutReexecMarker(home, pid)); err == nil {
		return true
	}
	installed, installedOK := currentLookoutExecutable(home)
	running, runningOK := inspectLookoutExecutable(pid)
	return installedOK && runningOK && (installed.Device != running.Device || installed.Inode != running.Inode)
}

func (s *Service) reexecLookoutIfUpdated(projectID int64, poller *store.LookoutPoller) error {
	home, err := s.homePath()
	if err != nil {
		return err
	}
	marker := lookoutReexecMarker(home, os.Getpid())
	requested := false
	if _, err := os.Stat(marker); err == nil {
		requested = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	installed, installedOK := currentLookoutExecutable(home)
	running, runningOK := inspectLookoutExecutable(os.Getpid())
	if installedOK && runningOK && (installed.Device != running.Device || installed.Inode != running.Inode) {
		requested = true
	}
	if !requested {
		return nil
	}
	if !installedOK {
		return fmt.Errorf("cannot verify the installed Posse executable")
	}
	if poller != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if db, _, openErr := s.openDB(); openErr == nil {
			updated := *poller
			updated.ExecutablePath, updated.ExecutableDevice, updated.ExecutableInode = installed.Path, installed.Device, installed.Inode
			updated.HeartbeatAt = currentTime()
			if setErr := db.SetLookoutPoller(ctx, projectID, updated); setErr != nil {
				_ = db.Close()
				return setErr
			}
			_ = db.Close()
			*poller = updated
		}
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	args := append([]string(nil), os.Args...)
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
		return fmt.Errorf("re-exec updated Lookout: %w", err)
	}
	return errors.New("lookout re-exec returned unexpectedly")
}

func lookoutReexecMarker(home string, pid int) string {
	return filepath.Join(home, "lookout-reexec", strconv.Itoa(pid))
}

func requestLookoutReexec(home string, processes []lookoutProcess) error {
	if len(processes) == 0 {
		return nil
	}
	directory := filepath.Join(home, "lookout-reexec")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	for _, process := range processes {
		if !lookoutPIDRunning(process.PID) {
			continue
		}
		if !lookoutExecutableIdentityMatches(process) {
			if !lookoutPIDRunning(process.PID) {
				continue
			}
			return fmt.Errorf("refusing to request re-exec for Lookout PID %d with a changed executable identity", process.PID)
		}
		marker := lookoutReexecMarker(home, process.PID)
		if err := os.WriteFile(marker, []byte("update\n"), 0o600); err != nil {
			return err
		}
	}
	return nil
}
