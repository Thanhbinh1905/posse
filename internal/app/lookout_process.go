package app

import (
	"context"
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

type lookoutProcess struct {
	PID          int
	PaneID       string
	WorkingDir   string
	Project      string
	Kind         string
	PollOnly     bool
	QuietRoutine bool
	ForceKilled  bool
}

var lookoutStopGracePeriod = 5 * time.Second

var lookoutProcRoot = "/proc"

// Check the process rather than trusting a restored shell pane's label.
func lookoutProcessRunning(paneID, home string) bool {
	if runtime.GOOS == "linux" {
		return linuxLookoutProcessRunning(paneID, home)
	}
	processes, err := systemLookoutProcesses(home)
	if err != nil {
		return false
	}
	for _, process := range processes {
		if process.PaneID == paneID && process.PollOnly {
			return true
		}
	}
	return false
}

func linuxLookoutProcessRunning(paneID, home string) bool {
	entries, err := os.ReadDir(lookoutProcRoot)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		root := filepath.Join(lookoutProcRoot, entry.Name())
		environment, err := os.ReadFile(filepath.Join(root, "environ"))
		if err != nil {
			continue
		}
		env := parseProcessEnvironment(string(environment))
		if env["HERDR_PANE_ID"] != paneID || !sameLookoutHome(processHome(env), home) {
			continue
		}
		command, err := os.ReadFile(filepath.Join(root, "cmdline"))
		if err != nil {
			continue
		}
		process, found := lookoutFromArgs(pid, splitProcessArgs(string(command)), env, home)
		if found && process.PollOnly {
			return true
		}
	}
	return false
}

func systemLookoutProcesses(home string) ([]lookoutProcess, error) {
	if runtime.GOOS == "darwin" {
		output, err := exec.Command("ps", "-axEww", "-o", "pid=,command=").Output()
		if err != nil {
			return nil, fmt.Errorf("list processes: %w", err)
		}
		return lookoutProcessesInListing(string(output), home), nil
	}
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("lookout process discovery is unsupported on %s", runtime.GOOS)
	}
	entries, err := os.ReadDir(lookoutProcRoot)
	if err != nil {
		return nil, fmt.Errorf("read process table: %w", err)
	}
	processes := make([]lookoutProcess, 0)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		root := filepath.Join(lookoutProcRoot, entry.Name())
		environment, err := os.ReadFile(filepath.Join(root, "environ"))
		if err != nil {
			continue // Processes can exit between ReadDir and inspection.
		}
		command, err := os.ReadFile(filepath.Join(root, "cmdline"))
		if err != nil {
			continue
		}
		if process, found := lookoutFromArgs(pid, splitProcessArgs(string(command)), parseProcessEnvironment(string(environment)), home); found {
			if workingDir, err := os.Readlink(filepath.Join(root, "cwd")); err == nil {
				process.WorkingDir = workingDir
			}
			processes = append(processes, process)
		}
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].PID < processes[j].PID })
	return processes, nil
}

func lookoutProcessesInListing(listing, home string) []lookoutProcess {
	var processes []lookoutProcess
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
		if process, found := lookoutFromArgs(pid, args, environment, home); found {
			processes = append(processes, process)
		}
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

func lookoutFromArgs(pid int, args []string, environment map[string]string, home string) (lookoutProcess, bool) {
	if len(args) < 2 || !sameLookoutHome(processHome(environment), home) {
		return lookoutProcess{}, false
	}
	lookoutCommand := -1
	if args[1] == "lookout" {
		lookoutCommand = 1
	} else if len(args) > 2 && isShellExecutable(args[0]) && args[2] == "lookout" {
		// Linux represents an executable shell script as the interpreter plus
		// script path before the script's command arguments.
		lookoutCommand = 2
	}
	if lookoutCommand < 0 {
		return lookoutProcess{}, false
	}
	process := lookoutProcess{
		PID:        pid,
		PaneID:     environment["HERDR_PANE_ID"],
		WorkingDir: environment["PWD"],
		Kind:       "lead",
	}
	for _, arg := range args[lookoutCommand+1:] {
		switch arg {
		case "--poll-only":
			process.PollOnly = true
			process.Kind = "lookout_tab"
		case "--quiet-routine":
			process.QuietRoutine = true
		}
	}
	return process, true
}

func isShellExecutable(path string) bool {
	switch filepath.Base(path) {
	case "sh", "bash", "dash", "zsh", "fish":
		return true
	default:
		return false
	}
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

func sameLookoutHome(processHome, home string) bool {
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

func (s *Service) runningLookouts(ctx context.Context, home string) ([]lookoutProcess, error) {
	processes, err := systemLookoutProcesses(home)
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
		return nil, fmt.Errorf("read Project data for lookouts: %w", err)
	}
	defer db.Close()
	projects, err := db.Projects(ctx)
	if err != nil {
		return nil, fmt.Errorf("read Projects for lookouts: %w", err)
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

// A Lead-side lookout belongs to this Project, not the dedicated poll-only
// tab. Process discovery is checked on each delivery attempt, so a killed
// lookout leaves no durable claim that could block the typed fallback.
func liveLeadLookout(project store.Project, processes []lookoutProcess) bool {
	for _, process := range processes {
		if process.PollOnly || !lookoutPIDRunning(process.PID) {
			continue
		}
		if process.PaneID != "" && process.PaneID == project.LeadPaneID || processWorkingInProject(process, project) {
			return true
		}
	}
	return false
}

func processBelongsToProject(process lookoutProcess, project store.Project) bool {
	if process.PaneID != "" && (process.PaneID == project.LeadPaneID || project.HerdrWorkspaceID != "" && strings.HasPrefix(process.PaneID, project.HerdrWorkspaceID+":")) {
		return true
	}
	return processWorkingInProject(process, project)
}

func processWorkingInProject(process lookoutProcess, project store.Project) bool {
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

func stopLookoutProcesses(ctx context.Context, home string, processes []lookoutProcess) ([]lookoutProcess, error) {
	if len(processes) == 0 {
		return processes, nil
	}
	markers := make([]string, 0, len(processes))
	for _, process := range processes {
		marker := lookoutUpdateStopMarker(home, process.PID)
		if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
			removeLookoutUpdateMarkers(markers)
			return nil, fmt.Errorf("prepare lookout stop notice: %w", err)
		}
		if err := os.WriteFile(marker, []byte("update\n"), 0o600); err != nil {
			removeLookoutUpdateMarkers(markers)
			return nil, fmt.Errorf("prepare lookout stop notice: %w", err)
		}
		markers = append(markers, marker)
	}
	defer removeLookoutUpdateMarkers(markers)

	for _, process := range processes {
		if err := syscall.Kill(process.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return nil, fmt.Errorf("send SIGTERM to lookout PID %d: %w", process.PID, err)
		}
	}
	deadline := time.Now().Add(lookoutStopGracePeriod)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if allLookoutsStopped(processes) {
			return processes, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	for i := range processes {
		if !lookoutPIDRunning(processes[i].PID) {
			continue
		}
		if err := syscall.Kill(processes[i].PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return nil, fmt.Errorf("send SIGKILL to lookout PID %d: %w", processes[i].PID, err)
		}
		processes[i].ForceKilled = true
	}
	killDeadline := time.Now().Add(time.Second)
	for time.Now().Before(killDeadline) && !allLookoutsStopped(processes) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !allLookoutsStopped(processes) {
		return nil, fmt.Errorf("lookout processes remain after SIGKILL: %s", formatLookoutProcesses(processes))
	}
	return processes, nil
}

func allLookoutsStopped(processes []lookoutProcess) bool {
	for _, process := range processes {
		if lookoutPIDRunning(process.PID) {
			return false
		}
	}
	return true
}

func lookoutPIDRunning(pid int) bool {
	if pid < 1 {
		return false
	}
	if runtime.GOOS == "linux" {
		stat, err := os.ReadFile(filepath.Join(lookoutProcRoot, strconv.Itoa(pid), "stat"))
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

func lookoutUpdateStopMarker(home string, pid int) string {
	return filepath.Join(home, "lookout-stops", strconv.Itoa(pid))
}

func removeLookoutUpdateMarkers(markers []string) {
	directories := make(map[string]bool)
	for _, marker := range markers {
		_ = os.Remove(marker)
		directories[filepath.Dir(marker)] = true
	}
	for directory := range directories {
		_ = os.Remove(directory)
	}
}

func lookoutProcessInListing(listing, paneID, home string) bool {
	for _, process := range lookoutProcessesInListing(listing, home) {
		if process.PaneID == paneID && process.PollOnly {
			return true
		}
	}
	return false
}

func formatLookoutProcesses(processes []lookoutProcess) string {
	rows := make([]string, 0, len(processes))
	for _, process := range processes {
		rows = append(rows, fmt.Sprintf("PID %d Project %s (%s)", process.PID, process.Project, process.Kind))
	}
	return strings.Join(rows, ", ")
}

func lookoutStopReason(home string, pid int) string {
	marker := lookoutUpdateStopMarker(home, pid)
	if err := os.Remove(marker); err == nil {
		_ = os.Remove(filepath.Dir(marker))
		return "update"
	}
	return "signal"
}
