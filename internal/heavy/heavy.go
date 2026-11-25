package heavy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"golang.org/x/sys/unix"
)

type machineConfig struct {
	Heavy struct {
		CPUSlots      *int `toml:"cpu_slots"`
		ReviewerSlots *int `toml:"reviewer_slots"`
	} `toml:"heavy"`
}

func defaultSlots(class string, processors int) int {
	if class == "reviewer" {
		return 1
	}
	return max(1, processors/6)
}

func slots(class string) (int, error) {
	count := defaultSlots(class, runtime.NumCPU())
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(configHome) {
		configHome = filepath.Join(os.Getenv("HOME"), ".config")
	}
	path := filepath.Join(configHome, "posse", "machine.toml")
	var cfg machineConfig
	if _, err := toml.DecodeFile(path, &cfg); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("read machine config %s: %w", path, err)
	}
	if class == "cpu" && cfg.Heavy.CPUSlots != nil {
		count = *cfg.Heavy.CPUSlots
	}
	if class == "reviewer" && cfg.Heavy.ReviewerSlots != nil {
		count = *cfg.Heavy.ReviewerSlots
	}
	if count < 1 {
		return 0, fmt.Errorf("heavy.%s_slots must be positive", class)
	}
	return count, nil
}

func lockDirectory() string {
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(runtimeDir) {
		return filepath.Join(runtimeDir, "posse", "heavy")
	}
	return filepath.Join("/tmp", "posse-"+strconv.Itoa(os.Getuid()), "heavy")
}

func tryAcquire(dir, class string, count int) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	for i := range count {
		file, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("%s-%d.lock", class, i)), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return file, nil
		}
		file.Close()
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return nil, err
		}
	}
	return nil, nil
}

func acquire(ctx context.Context, dir, class string, count int) (*os.File, error) {
	for {
		file, err := tryAcquire(dir, class, count)
		if err != nil || file != nil {
			return file, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

type record struct {
	PID     int       `json:"pid"`
	Home    string    `json:"home"`
	Project string    `json:"project"`
	Task    string    `json:"task"`
	Marker  string    `json:"marker"`
	Command string    `json:"command"`
	Since   time.Time `json:"since"`
}

func IsWaiting(home, project, task string) bool {
	entries, err := os.ReadDir(lockDirectory())
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "wait-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(lockDirectory(), entry.Name()))
		if err != nil {
			continue
		}
		var waiting record
		if json.Unmarshal(data, &waiting) != nil || waiting.PID < 2 || (home != "" && waiting.Home != home) || waiting.Project != project || waiting.Task != task || (os.Getenv("POSSE_TASK_ID") != "" && waiting.Marker != os.Getenv("POSSE_TASK_ID")) {
			continue
		}
		if err := syscall.Kill(waiting.PID, 0); err == nil || errors.Is(err, syscall.EPERM) {
			return true
		}
		_ = os.Remove(filepath.Join(lockDirectory(), entry.Name()))
	}
	return false
}

func HolderSummary() []string {
	entries, err := os.ReadDir(lockDirectory())
	if err != nil {
		return nil
	}
	var holders []string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), "wait-") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(lockDirectory(), entry.Name()))
		if err != nil {
			continue
		}
		var holder record
		if json.Unmarshal(data, &holder) != nil || holder.PID < 2 {
			continue
		}
		lockFile, err := os.OpenFile(filepath.Join(lockDirectory(), strings.TrimSuffix(entry.Name(), ".json")+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			continue
		}
		lockErr := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if lockErr == nil {
			_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
		}
		_ = lockFile.Close()
		if lockErr == nil {
			continue
		}
		slot := strings.TrimSuffix(entry.Name(), ".json")
		label := fmt.Sprintf("%s pid %d %s", slot, holder.PID, holder.Command)
		if holder.Project != "" && holder.Task != "" {
			label = fmt.Sprintf("%s %s pid %d %s", holder.Project, holder.Task, holder.PID, holder.Command)
		}
		holders = append(holders, label)
	}
	return holders
}

func IsWaitingTask(project, task, marker string) bool {
	entries, err := os.ReadDir(lockDirectory())
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "wait-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(lockDirectory(), entry.Name()))
		if err != nil {
			continue
		}
		var waiting record
		if json.Unmarshal(data, &waiting) == nil && waiting.PID > 1 && waiting.Project == project && waiting.Task == task && waiting.Marker == marker {
			if err := syscall.Kill(waiting.PID, 0); err == nil || errors.Is(err, syscall.EPERM) {
				return true
			}
			_ = os.Remove(filepath.Join(lockDirectory(), entry.Name()))
		}
	}
	return false
}

func Acquire(ctx context.Context, class, command string) (*os.File, time.Duration, error) {
	count, err := slots(class)
	if err != nil {
		return nil, 0, err
	}
	started := time.Now()
	dir := lockDirectory()
	file, err := tryAcquire(dir, class, count)
	if err != nil {
		return nil, 0, err
	}
	waitingPath := filepath.Join(dir, fmt.Sprintf("wait-%d.json", os.Getpid()))
	if file == nil {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, 0, err
		}
		entry, _ := json.Marshal(record{PID: os.Getpid(), Home: os.Getenv("POSSE_HOME"), Project: os.Getenv("POSSE_PROJECT"), Task: os.Getenv("POSSE_TASK_SEQ"), Marker: os.Getenv("POSSE_TASK_ID"), Command: command, Since: started})
		if err := os.WriteFile(waitingPath, entry, 0o600); err != nil {
			return nil, 0, err
		}
		file, err = acquire(ctx, dir, class, count)
		if err != nil {
			_ = os.Remove(waitingPath)
			return nil, 0, err
		}
		_ = os.Remove(waitingPath)
	}
	ownerPath := strings.TrimSuffix(file.Name(), ".lock") + ".json"
	owner, _ := json.Marshal(record{PID: os.Getpid(), Home: os.Getenv("POSSE_HOME"), Project: os.Getenv("POSSE_PROJECT"), Task: os.Getenv("POSSE_TASK_SEQ"), Marker: os.Getenv("POSSE_TASK_ID"), Command: command, Since: time.Now()})
	if err := os.WriteFile(ownerPath, owner, 0o600); err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	return file, time.Since(started), nil
}

func Release(file *os.File) error {
	if file == nil {
		return nil
	}
	ownerPath := strings.TrimSuffix(file.Name(), ".lock") + ".json"
	_ = os.Remove(ownerPath)
	return file.Close()
}

func Run(ctx context.Context, class string, args []string, stdout, stderr io.Writer) error {
	if class == "status" {
		_, err := fmt.Fprintln(stdout, strings.Join(HolderSummary(), "\n"))
		return err
	}
	if class != "cpu" && class != "reviewer" {
		return fmt.Errorf("unknown heavy class %q", class)
	}
	if len(args) == 0 {
		return errors.New("posse heavy requires a command after --")
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	started := time.Now()
	file, waited, err := Acquire(ctx, class, strings.Join(args, " "))
	if err != nil {
		return err
	}
	defer Release(file)
	holderPath := strings.TrimSuffix(file.Name(), ".lock") + ".json"
	holderData, _ := json.Marshal(record{PID: os.Getpid(), Home: os.Getenv("POSSE_HOME"), Project: os.Getenv("POSSE_PROJECT"), Task: os.Getenv("POSSE_TASK_SEQ"), Marker: os.Getenv("POSSE_TASK_ID"), Command: strings.Join(args, " "), Since: time.Now()})
	if err := os.WriteFile(holderPath, holderData, 0o600); err != nil {
		return err
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{file}
	if err := cmd.Start(); err != nil {
		return err
	}
	holderData, _ = json.Marshal(record{PID: cmd.Process.Pid, Home: os.Getenv("POSSE_HOME"), Project: os.Getenv("POSSE_PROJECT"), Task: os.Getenv("POSSE_TASK_SEQ"), Marker: os.Getenv("POSSE_TASK_ID"), Command: strings.Join(args, " "), Since: time.Now()})
	if err := os.WriteFile(holderPath, holderData, 0o600); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		_ = cmd.Wait()
		return err
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	var result error
	select {
	case result = <-finished:
	case <-ctx.Done():
		if cmd.Process != nil && cmd.Process.Pid > 1 {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
		select {
		case result = <-finished:
		case <-time.After(2 * time.Second):
			if cmd.Process != nil && cmd.Process.Pid > 1 {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			result = <-finished
		}
		if result == nil {
			result = ctx.Err()
		}
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "waited %s, ran %s\n", waited.Round(time.Millisecond), time.Since(started.Add(waited)).Round(time.Millisecond))
	return result
}

func HeavyCommand(args []string) (string, []string, error) {
	if len(args) == 1 && args[0] == "--status" {
		return "status", nil, nil
	}
	class := "cpu"
	if len(args) > 0 && strings.HasPrefix(args[0], "--class=") {
		class = strings.TrimPrefix(args[0], "--class=")
		args = args[1:]
	} else if len(args) > 1 && args[0] == "--class" {
		class, args = args[1], args[2:]
	}
	if (class != "cpu" && class != "reviewer") || len(args) == 0 || args[0] != "--" {
		return "", nil, errors.New("usage: posse heavy [--class cpu|reviewer] -- <command> | --status")
	}
	return class, args[1:], nil
}
