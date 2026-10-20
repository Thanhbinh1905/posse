package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
var watchRuntimeLogMaxBytes int64 = 1 << 20

const watchRuntimeRepairBudget = 2 * time.Second

func watchRuntimeLogPath(home string, project store.Project) string {
	return filepath.Join(home, "projects", project.Name, "watch.log")
}

func rotateWatchRuntimeLog(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("watch log is not a regular file")
	}
	if info.Size() < watchRuntimeLogMaxBytes {
		return nil
	}
	backup := path + ".1"
	if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(path, backup)
}

type watchRuntimeLogWriter struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	size   int64
	closed bool
}

func openWatchRuntimeLog(path string) (*watchRuntimeLogWriter, error) {
	if err := rotateWatchRuntimeLog(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	return &watchRuntimeLogWriter{path: path, file: file, size: info.Size()}, nil
}

func (writer *watchRuntimeLogWriter) rotateLocked() error {
	if err := writer.file.Close(); err != nil {
		return err
	}
	if err := rotateWatchRuntimeLog(writer.path); err != nil {
		return err
	}
	file, err := os.OpenFile(writer.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writer.file, writer.size = file, 0
	return nil
}

func (writer *watchRuntimeLogWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.closed {
		return 0, os.ErrClosed
	}
	limit := watchRuntimeLogMaxBytes
	if limit < 1 {
		limit = 1
	}
	written := 0
	for len(data) > 0 {
		if writer.size >= limit {
			if err := writer.rotateLocked(); err != nil {
				return written, err
			}
		}
		count := int64(len(data))
		if room := limit - writer.size; count > room {
			count = room
		}
		n, err := writer.file.Write(data[:int(count)])
		written += n
		writer.size += int64(n)
		data = data[n:]
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (writer *watchRuntimeLogWriter) Close() error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.closed {
		return nil
	}
	writer.closed = true
	return writer.file.Close()
}

func startWatchProcess(command *exec.Cmd) error {
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
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

func currentWatchExecutable(_ string) (watchExecutableIdentity, bool) {
	path, err := os.Executable()
	if err != nil {
		return watchExecutableIdentity{}, false
	}
	return watchExecutableAt(path)
}

func watchProcessMatchesWatch(process watchProcess, projectID int64, watch store.WatchRuntime) bool {
	return process.PID == watch.PID && process.WatchProjectID == projectID && process.WatchToken == watch.Token && process.WatchStartedAt == watch.StartedAt
}

func watchRuntimeProcessHealthy(home string, projectID int64, state store.WatchRuntime) bool {
	if state.PID < 1 || state.Token == "" || state.StartedAt < 1 || state.HeartbeatAt < 1 || time.Since(time.UnixMilli(state.HeartbeatAt)) > watchRuntimeStaleAfter {
		return false
	}
	process, ok := inspectWatchProcess(home, state.PID)
	if !ok || !process.PollOnly || !watchPIDRunning(state.PID) || !watchProcessMatchesWatch(process, projectID, state) {
		return false
	}
	return process.Executable.Device == state.ExecutableDevice && process.Executable.Inode == state.ExecutableInode
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

func stopProjectPollerAfterContention(ctx context.Context, home string, project store.Project) error {
	processes, err := systemWatchProcesses(home)
	if err != nil {
		return err
	}
	for _, process := range processes {
		if !process.PollOnly || process.WatchProjectID != project.ID {
			continue
		}
		if err := stopStoredWatchRuntimeForDown(ctx, home, process); err != nil {
			return err
		}
	}
	return nil
}

func stopStoredWatchRuntime(ctx context.Context, home string, process watchProcess) error {
	return stopStoredWatchRuntimeWithEscalation(ctx, home, process, false)
}

func stopStoredWatchRuntimeForDown(ctx context.Context, home string, process watchProcess) error {
	return stopStoredWatchRuntimeWithEscalation(ctx, home, process, true)
}

func stopStoredWatchRuntimeWithEscalation(ctx context.Context, home string, process watchProcess, escalate bool) error {
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
	if err := signalWatchProcessGroup(process.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
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
		if !escalate {
			return fmt.Errorf("watch PID %d did not stop gracefully; refusing to interrupt Project maintenance", process.PID)
		}
		owned, err := storedWatchProcessIsCurrent(home, process)
		if err != nil {
			return err
		}
		if !owned {
			return fmt.Errorf("refusing to SIGKILL Watch PID %d after its process identity changed", process.PID)
		}
		if err := signalWatchProcessGroup(process.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("send SIGKILL to wedged Watch PID %d: %w", process.PID, err)
		}
		killDeadline := time.Now().Add(time.Second)
		for time.Now().Before(killDeadline) && watchPIDRunning(process.PID) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(25 * time.Millisecond):
			}
		}
		if watchPIDRunning(process.PID) {
			return fmt.Errorf("watch PID %d remains after SIGKILL", process.PID)
		}
	}
	return nil
}

func signalWatchProcessGroup(pid int, signal syscall.Signal) error {
	if group, err := syscall.Getpgid(pid); err == nil && group == pid {
		return syscall.Kill(-pid, signal)
	}
	return syscall.Kill(pid, signal)
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
	log, err := openWatchRuntimeLog(logPath)
	if err != nil {
		return fmt.Errorf("open Project Watch log: %w", err)
	}
	if err := log.Close(); err != nil {
		return fmt.Errorf("close Project Watch log: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open Project Watch log: %w", err)
	}
	command := exec.Command(binary, "watch", "--poll-only")
	command.Dir = project.Root
	command.Env = watchRuntimeEnvironment(os.Environ(), home, project, token, startedAt)
	command.Stdout, command.Stderr = logFile, logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := startWatchProcess(command); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start hidden Project Watch: %w", err)
	}
	_ = logFile.Close()
	watch := store.WatchRuntime{
		PID: command.Process.Pid, Token: token, StartedAt: startedAt, HeartbeatAt: startedAt,
		ExecutablePath: identity.Path, ExecutableDevice: identity.Device, ExecutableInode: identity.Inode,
	}
	if err := db.SetWatchRuntime(ctx, project.ID, watch); err != nil {
		_ = command.Process.Kill()
		return fmt.Errorf("record hidden Project Watch: %w", err)
	}
	return nil
}

func watchRuntimeEnvironment(parent []string, home string, project store.Project, token string, startedAt int64) []string {
	values := make(map[string]string, len(parent)+5)
	for _, entry := range parent {
		key, value, found := strings.Cut(entry, "=")
		if !found || key == "" || watchRuntimeDropsEnvironmentVariable(key) {
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

func watchRuntimeDropsEnvironmentVariable(key string) bool {
	switch key {
	case "POSSE_HOME", "POSSE_TASK_ID", "POSSE_TASK_TEMP", "POSSE_WORKER_HOME",
		watchRuntimeProjectEnv, watchRuntimeTokenEnv, watchRuntimeStartedEnv,
		"HERDR_PANE_ID", "HERDR_WORKSPACE_ID", "HERDR_TAB_ID", "HERDR_PLUGIN_EVENT", "HERDR_PLUGIN_EVENT_JSON",
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE":
		return true
	default:
		return strings.HasPrefix(key, "POSSE_INTENT_")
	}
}

func (s *Service) superviseWatchRuntimes(ctx *axi.Context, command string, args []string) {
	// Notice receipts are delivery work, not a Project maintenance boundary. Handoffs also repair the observer.
	if os.Getenv(watchRuntimeTokenEnv) != "" || command == "ack" || command == "down" || command == "lookout" || command == "remove" || command == "_update-preflight" || command == "watch" && (containsArgument(args, "--repair") || containsArgument(args, "--receipt") || containsArgument(args, "--receipt-outcome") || containsArgument(args, "--receipt-token")) {
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
		if arg == value || strings.HasPrefix(arg, value+"=") {
			return true
		}
	}
	return false
}

type watchProgressContextKey struct{}

type watchProgressReporter struct {
	mu        sync.Mutex
	last      time.Time
	heartbeat func()
}

func (r *watchProgressReporter) Report() {
	r.mu.Lock()
	if time.Since(r.last) < watchRuntimeHeartbeatInterval {
		r.mu.Unlock()
		return
	}
	r.last = time.Now()
	r.mu.Unlock()
	r.heartbeat()
}

func withWatchProgress(ctx context.Context, progress func()) context.Context {
	return context.WithValue(ctx, watchProgressContextKey{}, progress)
}

func reportWatchProgress(ctx context.Context) {
	if progress, ok := ctx.Value(watchProgressContextKey{}).(func()); ok {
		progress()
	}
}

func (s *Service) watchMaintenanceInFlight() bool {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	return len(s.maintenance) > 0
}

func legacyPollerInstalledExecutable(home string) (watchExecutableIdentity, bool) {
	manifest, found, err := readSetupManifest(filepath.Join(home, setupManifestName))
	if err != nil || !found || manifest.Binary == "" {
		return watchExecutableIdentity{}, false
	}
	return watchExecutableAt(manifest.Binary)
}

func (s *Service) reexecLegacyWatch(target watchExecutableIdentity) error {
	args := watchReexecArguments(os.Args)
	if len(args) == 0 {
		args = []string{target.Path}
	} else {
		args[0] = target.Path
	}
	execProcess := syscall.Exec
	if s.reexecUpdate != nil {
		execProcess = s.reexecUpdate
	}
	if err := execProcess(target.Path, args, os.Environ()); err != nil {
		return fmt.Errorf("re-exec legacy poller into the installed Watch: %w", err)
	}
	return errors.New("legacy Watch re-exec returned unexpectedly")
}

func (s *Service) legacyPollerAdapter(ctx *axi.Context, args []string) (bool, error) {
	if len(args) != 1 || args[0] != "--poll-only" || len(os.Args) != 3 || os.Args[1] != "lookout" || os.Args[2] != "--poll-only" {
		return false, nil
	}
	home, err := s.homePath()
	if err != nil {
		return false, nil
	}
	environment := parseProcessEnvironment(strings.Join(os.Environ(), "\x00"))
	if environment["POSSE_LOOKOUT_PROJECT_ID"] == "" || environment["POSSE_LOOKOUT_TOKEN"] == "" || environment["POSSE_LOOKOUT_STARTED_AT"] == "" {
		return false, nil
	}
	process, ok := watchFromArgs(os.Getpid(), os.Args, environment, home)
	if !ok || !process.PollOnly || process.WatchProjectID < 1 || process.WatchToken == "" || process.WatchStartedAt < 1 {
		return false, nil
	}
	db, err := store.OpenReadOnly(home)
	if err != nil {
		return false, nil
	}
	project, err := db.ProjectByID(ctx.Context, process.WatchProjectID)
	if err != nil {
		_ = db.Close()
		return false, nil
	}
	runtime, err := db.WatchRuntime(ctx.Context, project.ID)
	if err != nil {
		_ = db.Close()
		return false, nil
	}
	workingDir, err := currentDir()
	if err != nil {
		_ = db.Close()
		return false, nil
	}
	registered, found, err := registeredProjectFor(ctx.Context, db, workingDir)
	_ = db.Close()
	if err != nil || !found || registered.ID != project.ID || runtime.PID != os.Getpid() || runtime.Token != process.WatchToken || runtime.StartedAt != process.WatchStartedAt {
		return false, nil
	}
	running, ok := inspectWatchExecutable(os.Getpid())
	if !ok || !watchExecutableVerifier(running.Path) || runtime.ExecutableDevice != running.Device || runtime.ExecutableInode != running.Inode {
		return false, nil
	}
	for _, binding := range []struct{ key, value string }{
		{watchRuntimeProjectEnv, strconv.FormatInt(project.ID, 10)},
		{watchRuntimeTokenEnv, runtime.Token},
		{watchRuntimeStartedEnv, strconv.FormatInt(runtime.StartedAt, 10)},
	} {
		if err := os.Setenv(binding.key, binding.value); err != nil {
			return true, err
		}
	}
	for _, key := range []string{"POSSE_LOOKOUT_PROJECT_ID", "POSSE_LOOKOUT_TOKEN", "POSSE_LOOKOUT_STARTED_AT"} {
		if err := os.Unsetenv(key); err != nil {
			return true, err
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "lookout" {
		os.Args[1] = "watch"
	}
	// The legacy protocol is the one-time exception to the spawned-binary baseline:
	// move an old lookout into the installed Watch before it enters the poll loop.
	if target, found := legacyPollerInstalledExecutable(home); found && !sameWatchExecutableIdentity(target, running) {
		return true, s.reexecLegacyWatch(target)
	}
	return true, s.wait(ctx, args)
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
	home, err := s.homePath()
	if err != nil {
		return err
	}
	log, err := openWatchRuntimeLog(watchRuntimeLogPath(home, project))
	if err != nil {
		return fmt.Errorf("open Project Watch log: %w", err)
	}
	defer log.Close()
	ctx.Out, ctx.ErrOut = log, log
	watch := store.WatchRuntime{PID: os.Getpid(), Token: token, StartedAt: startedAt, HeartbeatAt: currentTime(), ExecutablePath: identity.Path, ExecutableDevice: identity.Device, ExecutableInode: identity.Inode}
	if err := db.SetWatchRuntime(ctx.Context, project.ID, watch); err != nil {
		return err
	}
	defer db.ClearWatchRuntime(context.Background(), project.ID, os.Getpid(), token)
	return s.runWatchLoop(ctx, db, project, timeout, stopSignals, &watch)
}

func (s *Service) runWatchLoop(ctx *axi.Context, db *store.DB, project store.Project, timeout time.Duration, stopSignals <-chan os.Signal, watch *store.WatchRuntime) error {
	loopCtx := ctx.Context
	if watch != nil {
		progress := &watchProgressReporter{heartbeat: func() {
			heartbeatCtx, cancel := context.WithTimeout(loopCtx, time.Second)
			defer cancel()
			if _, err := db.HeartbeatWatchRuntime(heartbeatCtx, project.ID, watch.PID, watch.Token, currentTime()); err != nil {
				fmt.Fprintf(ctx.ErrOut, "Watch progress heartbeat failed for %s: %v\n", project.Name, err)
			}
		}}
		loopCtx = withWatchProgress(loopCtx, progress.Report)
	}
	defer func() {
		s.cancelWatchMaintenance()
		if !s.waitForWatchMaintenance(4 * time.Second) {
			fmt.Fprintln(ctx.ErrOut, "Watch maintenance shutdown timed out; unfinished poll claims will expire")
		}
	}()
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	lastFailure := ""
	for {
		if watch != nil {
			ok, err := db.HeartbeatWatchRuntime(loopCtx, project.ID, watch.PID, watch.Token, currentTime())
			if err != nil {
				fmt.Fprintf(ctx.ErrOut, "Watch heartbeat failed for %s: %v\n", project.Name, err)
			} else if !ok {
				return nil
			} else if !s.watchMaintenanceInFlight() {
				if err := s.reexecWatchIfUpdated(project.ID, watch); err != nil {
					fmt.Fprintf(ctx.ErrOut, "Watch re-exec check failed for %s: %v\n", project.Name, err)
				}
			}
		}
		select {
		case sig := <-stopSignals:
			if sig == syscall.SIGTERM {
				return s.reportWatchStopped(ctx, project, true)
			}
		default:
		}
		fresh, err := db.ProjectByID(loopCtx, project.ID)
		if err != nil {
			return err
		}
		if fresh.IsDown() || fresh.Root != project.Root {
			return nil
		}
		if _, err := s.prepareProjectWatchObservation(loopCtx, db, fresh); err != nil && !errors.Is(err, errRecoveryHeld) && !errors.Is(err, errRecoveryDeferred) {
			recordWatchFailure(ctx, db, project.ID, &lastFailure, "reconcile", err)
		}
		if err := s.maintainProjectWatch(loopCtx, db, fresh); err != nil {
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
		case <-ctx.Context.Done():
			return ctx.Context.Err()
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
	installed, installedOK := currentWatchExecutable(home)
	return requestWatchReexecForIdentity(home, processes, installed, installedOK)
}

func requestWatchReexecForIdentity(home string, processes []watchProcess, installed watchExecutableIdentity, installedOK bool) error {
	if len(processes) == 0 {
		return nil
	}
	directory := filepath.Join(home, "watch-reexec")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
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
