package app

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

type watchExecutableIdentity struct {
	Path   string
	Device uint64
	Inode  uint64
}

type watchProcess struct {
	PID             int
	Executable      watchExecutableIdentity
	PaneID          string
	WorkingDir      string
	Project         string
	Kind            string
	PollOnly        bool
	PollOnlyBounded bool
	QuietRoutine    bool
	StopHook        bool
	ForceKilled     bool
	WatchProjectID  int64
	WatchToken      string
	WatchStartedAt  int64
}

var watchStopGracePeriod = 5 * time.Second
var watchExecutableVerifier = isPosseExecutable

var watchProcRoot = "/proc"

func inspectWatchProcess(home string, pid int) (watchProcess, bool) {
	if runtime.GOOS == "linux" {
		root := filepath.Join(watchProcRoot, strconv.Itoa(pid))
		environment, err := os.ReadFile(filepath.Join(root, "environ"))
		if err != nil {
			return watchProcess{}, false
		}
		command, err := os.ReadFile(filepath.Join(root, "cmdline"))
		if err != nil {
			return watchProcess{}, false
		}
		process, found := watchFromArgs(pid, splitProcessArgs(string(command)), parseProcessEnvironment(string(environment)), home)
		if !found {
			return watchProcess{}, false
		}
		executable, verified := inspectWatchExecutable(pid)
		if !verified || !watchExecutableVerifier(executable.Path) {
			return watchProcess{}, false
		}
		process.Executable = executable
		return process, true
	}
	processes, err := systemWatchProcesses(home)
	if err != nil {
		return watchProcess{}, false
	}
	for _, process := range processes {
		if process.PID == pid {
			return process, true
		}
	}
	return watchProcess{}, false
}

func systemWatchProcesses(home string) ([]watchProcess, error) {
	if runtime.GOOS == "darwin" {
		output, err := exec.Command("ps", "-axEww", "-o", "pid=,command=").Output()
		if err != nil {
			return nil, fmt.Errorf("list processes: %w", err)
		}
		return watchProcessesInListingWithExecutableLookup(string(output), home, func(pid int, _ string) (watchExecutableIdentity, bool) {
			executable, verified := inspectWatchExecutable(pid)
			return executable, verified && watchExecutableVerifier(executable.Path)
		}), nil
	}
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("watch process discovery is unsupported on %s", runtime.GOOS)
	}
	entries, err := os.ReadDir(watchProcRoot)
	if err != nil {
		return nil, fmt.Errorf("read process table: %w", err)
	}
	processes := make([]watchProcess, 0)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		root := filepath.Join(watchProcRoot, entry.Name())
		command, err := os.ReadFile(filepath.Join(root, "cmdline"))
		if err != nil {
			continue // Processes can exit between ReadDir and inspection.
		}
		args := splitProcessArgs(string(command))
		// Reading every process environment makes discovery scale with the
		// machine's total environment size, so skip non-Watch commands first.
		if !isWatchCommandArgs(args) {
			continue
		}
		environment, err := os.ReadFile(filepath.Join(root, "environ"))
		if err != nil {
			continue
		}
		process, found := watchFromArgs(pid, args, parseProcessEnvironment(string(environment)), home)
		if !found {
			continue
		}
		executable, verified := inspectWatchExecutable(pid)
		if !verified || !watchExecutableVerifier(executable.Path) {
			continue
		}
		process.Executable = executable
		if workingDir, err := os.Readlink(filepath.Join(root, "cwd")); err == nil {
			process.WorkingDir = workingDir
		}
		processes = append(processes, process)
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].PID < processes[j].PID })
	return processes, nil
}

func watchProcessesInListingWithVerifier(listing, home string, isPosseExecutable func(string) bool) []watchProcess {
	return watchProcessesInListingWithExecutableLookup(listing, home, func(_ int, argv0 string) (watchExecutableIdentity, bool) {
		if !isPosseExecutable(argv0) {
			return watchExecutableIdentity{}, false
		}
		return watchExecutableIdentity{Path: argv0}, true
	})
}

func watchProcessesInListingWithExecutableLookup(listing, home string, lookup func(int, string) (watchExecutableIdentity, bool)) []watchProcess {
	var processes []watchProcess
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		commandAndEnvironment := fields[1:]
		environmentAt := len(commandAndEnvironment)
		for i, field := range commandAndEnvironment {
			if processEnvironmentField(field) {
				environmentAt = i
				break
			}
		}
		args := commandAndEnvironment[:environmentAt]
		environment := parseProcessEnvironment(strings.Join(commandAndEnvironment[environmentAt:], "\x00"))
		process, found := watchFromArgs(pid, args, environment, home)
		if !found {
			continue
		}
		executable, verified := lookup(pid, args[0])
		if !verified {
			continue
		}
		process.Executable = executable
		processes = append(processes, process)
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].PID < processes[j].PID })
	return processes
}

func processEnvironmentField(field string) bool {
	key, _, found := strings.Cut(field, "=")
	if !found || key == "" {
		return false
	}
	for _, char := range key {
		if char != '_' && (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

func parseProcessEnvironment(raw string) map[string]string {
	environment := make(map[string]string)
	for _, value := range strings.Split(raw, "\x00") {
		key, value, found := strings.Cut(value, "=")
		if found {
			environment[key] = value
		}
	}
	return environment
}

func splitProcessArgs(raw string) []string {
	args := strings.Split(raw, "\x00")
	if len(args) > 0 && args[len(args)-1] == "" {
		args = args[:len(args)-1]
	}
	return args
}

// isWatchCommandArgs reports whether args name a Watch command. The former
// name is recognized for ownership discovery and the supervised poller's narrow
// re-entry adapter, never as general CLI input.
func isWatchCommandArgs(args []string) bool {
	return len(args) >= 2 && (args[1] == "watch" || args[1] == "lookout" || args[1] == "_claude-stop")
}

func watchFromArgs(pid int, args []string, environment map[string]string, home string) (watchProcess, bool) {
	if !isWatchCommandArgs(args) || !sameWatchHome(processHome(environment), home) {
		return watchProcess{}, false
	}
	// Pre-upgrade supervised processes retain their private binding names.
	// Their matching database identity is required before re-entry is allowed.
	binding := func(current, historical string) (string, bool) {
		value := environment[current]
		if args[1] != "lookout" || environment[historical] == "" {
			return value, true
		}
		if value != "" && value != environment[historical] {
			return "", false
		}
		return environment[historical], true
	}
	projectBinding, projectOK := binding(watchRuntimeProjectEnv, "POSSE_LOOKOUT_PROJECT_ID")
	tokenBinding, tokenOK := binding(watchRuntimeTokenEnv, "POSSE_LOOKOUT_TOKEN")
	startedBinding, startedOK := binding(watchRuntimeStartedEnv, "POSSE_LOOKOUT_STARTED_AT")
	if !projectOK || !tokenOK || !startedOK {
		return watchProcess{}, false
	}
	process := watchProcess{
		PID:            pid,
		PaneID:         environment["HERDR_PANE_ID"],
		WorkingDir:     environment["PWD"],
		Kind:           "lead",
		StopHook:       args[1] == "_claude-stop",
		WatchToken:     tokenBinding,
		WatchStartedAt: parseProcessInt64(startedBinding),
	}
	process.WatchProjectID = parseProcessInt64(projectBinding)
	for index := 2; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "--poll-only":
			process.PollOnly = true
			process.Kind = "watch"
		case "--quiet-routine":
			process.QuietRoutine = true
		case "--timeout":
			if index+1 < len(args) && parseProcessInt64(args[index+1]) > 0 {
				process.PollOnlyBounded = true
				index++
			}
		default:
			if strings.HasPrefix(arg, "--timeout=") && parseProcessInt64(strings.TrimPrefix(arg, "--timeout=")) > 0 {
				process.PollOnlyBounded = true
			}
		}
	}
	return process, true
}

// Go's module path identifies the repository, not the executable. The main
// package path identifies Posse's actual CLI entry point and allows renamed
// copies of that executable without accepting other commands from the module.
func isPosseExecutable(path string) bool {
	info, err := buildinfo.ReadFile(path)
	return err == nil && info.Main.Path == "github.com/thanhbinh1905/posse" && info.Path == "github.com/thanhbinh1905/posse/cmd/posse"
}

func parseProcessInt64(value string) int64 {
	parsed, _ := strconv.ParseInt(value, 10, 64)
	return parsed
}

func processHome(environment map[string]string) string {
	if home := environment["POSSE_HOME"]; home != "" {
		return home
	}
	if home := environment["HOME"]; home != "" {
		return filepath.Join(home, ".posse")
	}
	return ""
}

func sameWatchHome(processHome, home string) bool {
	if processHome == "" || home == "" {
		return false
	}
	processPath, err := filepath.Abs(processHome)
	if err != nil {
		return false
	}
	homePath, err := filepath.Abs(home)
	return err == nil && filepath.Clean(processPath) == filepath.Clean(homePath)
}

func (s *Service) runningWatches(ctx context.Context, home string) ([]watchProcess, error) {
	processes, err := systemWatchProcesses(home)
	if err != nil {
		return nil, err
	}
	if len(processes) == 0 {
		return processes, nil
	}
	db, err := store.OpenReadOnly(home)
	if errors.Is(err, os.ErrNotExist) {
		for i := range processes {
			processes[i].Project = "unknown"
		}
		return processes, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Project data for watches: %w", err)
	}
	defer db.Close()
	projects, err := db.Projects(ctx)
	if err != nil {
		return nil, fmt.Errorf("read Projects for watches: %w", err)
	}
	for i := range processes {
		processes[i].Project = "unknown"
		for _, project := range projects {
			if processBelongsToProject(processes[i], project) {
				processes[i].Project = project.Name
				break
			}
		}
	}
	return processes, nil
}

// A Lead-side watch belongs to this Project, not the dedicated poll-only
// tab. Process discovery is checked on each delivery attempt, so a killed
// watch leaves no durable claim that could block the typed fallback.
func liveLeadWatch(project store.Project, processes []watchProcess) bool {
	for _, process := range processes {
		if process.PollOnly || !watchPIDRunning(process.PID) {
			continue
		}
		if process.PaneID != "" && process.PaneID == project.LeadPaneID || processWorkingInProject(process, project) {
			return true
		}
	}
	return false
}

func processBelongsToProject(process watchProcess, project store.Project) bool {
	if process.PaneID != "" && (process.PaneID == project.LeadPaneID || project.HerdrWorkspaceID != "" && strings.HasPrefix(process.PaneID, project.HerdrWorkspaceID+":")) {
		return true
	}
	return processWorkingInProject(process, project)
}

func supervisedWatchProcessesForProject(processes []watchProcess, project store.Project) []watchProcess {
	var supervised []watchProcess
	for _, process := range processes {
		if process.PollOnly && process.WatchProjectID == project.ID && process.WatchToken != "" && process.WatchStartedAt > 0 {
			supervised = append(supervised, process)
		}
	}
	return supervised
}

func processWorkingInProject(process watchProcess, project store.Project) bool {
	if process.WorkingDir == "" || project.Root == "" {
		return false
	}
	workingDir, err := filepath.Abs(process.WorkingDir)
	if err != nil {
		return false
	}
	projectRoot, err := filepath.Abs(project.Root)
	return err == nil && pathWithin(filepath.Clean(projectRoot), filepath.Clean(workingDir))
}

func stopWatchProcesses(ctx context.Context, home string, processes []watchProcess) ([]watchProcess, error) {
	if len(processes) == 0 {
		return processes, nil
	}
	for _, process := range processes {
		if !watchPIDRunning(process.PID) {
			continue
		}
		if !watchExecutableIdentityMatches(process) {
			if !watchPIDRunning(process.PID) {
				continue
			}
			return nil, fmt.Errorf("refusing to signal Watch PID %d with unverified executable identity", process.PID)
		}
	}
	markers := make([]string, 0, len(processes))
	for _, process := range processes {
		marker := watchUpdateStopMarker(home, process.PID)
		if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
			removeWatchUpdateMarkers(markers)
			return nil, fmt.Errorf("prepare watch stop notice: %w", err)
		}
		if err := os.WriteFile(marker, []byte("update\n"), 0o600); err != nil {
			removeWatchUpdateMarkers(markers)
			return nil, fmt.Errorf("prepare watch stop notice: %w", err)
		}
		markers = append(markers, marker)
	}
	defer removeWatchUpdateMarkers(markers)

	for _, process := range processes {
		if !watchPIDRunning(process.PID) {
			continue
		}
		if !watchExecutableIdentityMatches(process) {
			if !watchPIDRunning(process.PID) {
				continue
			}
			return nil, fmt.Errorf("refusing to signal Watch PID %d after executable identity changed", process.PID)
		}
		if err := syscall.Kill(process.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return nil, fmt.Errorf("send SIGTERM to watch PID %d: %w", process.PID, err)
		}
	}
	deadline := time.Now().Add(watchStopGracePeriod)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if allWatchesStopped(processes) {
			return processes, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	for i := range processes {
		if !watchPIDRunning(processes[i].PID) {
			continue
		}
		if !watchExecutableIdentityMatches(processes[i]) {
			return nil, fmt.Errorf("refusing to SIGKILL Watch PID %d after executable identity changed", processes[i].PID)
		}
		if err := syscall.Kill(processes[i].PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return nil, fmt.Errorf("send SIGKILL to watch PID %d: %w", processes[i].PID, err)
		}
		processes[i].ForceKilled = true
	}
	killDeadline := time.Now().Add(time.Second)
	for time.Now().Before(killDeadline) && !allWatchesStopped(processes) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !allWatchesStopped(processes) {
		return nil, fmt.Errorf("watch processes remain after SIGKILL: %s", formatWatchProcesses(processes))
	}
	return processes, nil
}

func watchExecutableIdentityMatches(process watchProcess) bool {
	if process.Executable.Path == "" || process.Executable.Inode == 0 {
		return false
	}
	current, ok := inspectWatchExecutable(process.PID)
	return ok && current == process.Executable
}

func allWatchesStopped(processes []watchProcess) bool {
	for _, process := range processes {
		if watchPIDRunning(process.PID) {
			return false
		}
	}
	return true
}

func watchPIDRunning(pid int) bool {
	if pid < 1 {
		return false
	}
	if runtime.GOOS == "linux" {
		stat, err := os.ReadFile(filepath.Join(watchProcRoot, strconv.Itoa(pid), "stat"))
		if err == nil {
			if closeParen := strings.LastIndexByte(string(stat), ')'); closeParen >= 0 {
				fields := strings.Fields(string(stat[closeParen+1:]))
				if len(fields) > 0 && fields[0] == "Z" {
					return false
				}
			}
		}
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func watchUpdateStopMarker(home string, pid int) string {
	return filepath.Join(home, "watch-stops", strconv.Itoa(pid))
}

func removeWatchUpdateMarkers(markers []string) {
	directories := make(map[string]bool)
	for _, marker := range markers {
		_ = os.Remove(marker)
		directories[filepath.Dir(marker)] = true
	}
	for directory := range directories {
		_ = os.Remove(directory)
	}
}

func watchProcessInListingWithVerifier(listing, paneID, home string, isPosseExecutable func(string) bool) bool {
	for _, process := range watchProcessesInListingWithVerifier(listing, home, isPosseExecutable) {
		if process.PaneID == paneID && process.PollOnly {
			return true
		}
	}
	return false
}

func formatWatchProcesses(processes []watchProcess) string {
	rows := make([]string, 0, len(processes))
	for _, process := range processes {
		rows = append(rows, fmt.Sprintf("PID %d Project %s (%s)", process.PID, process.Project, process.Kind))
	}
	return strings.Join(rows, ", ")
}

func watchStopReason(home string, pid int) string {
	marker := watchUpdateStopMarker(home, pid)
	if err := os.Remove(marker); err == nil {
		_ = os.Remove(filepath.Dir(marker))
		return "update"
	}
	marker = watchReexecMarker(home, pid)
	if err := os.Remove(marker); err == nil {
		_ = os.Remove(filepath.Dir(marker))
		return "update"
	}
	installed, installedOK := currentWatchExecutable(home)
	running, runningOK := inspectWatchExecutable(pid)
	if installedOK && runningOK && (installed.Device != running.Device || installed.Inode != running.Inode) {
		return "update"
	}
	return "signal"
}
