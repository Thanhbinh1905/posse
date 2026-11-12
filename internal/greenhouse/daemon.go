package greenhouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/atomicfile"
)

const (
	stateFile = "posse-daemon.json"
	logFile   = "posse-daemon.log"
)

// Daemon is the Posse-owned Greenhouse daemon. Home is GREENHOUSE_HOME, always
// <POSSE_HOME>/greenhouse, never the User's own Greenhouse home.
type Daemon struct {
	Binary string
	Home   string
	// Env is the caller's environment. Task markers and Posse and Herdr
	// variables are removed before the daemon or a client sees it.
	Env []string
	// InTask reports whether a process runs inside a Posse Task's cgroup or
	// carries its marker.
	InTask func(pid int) bool
}

// DaemonState is what Posse recorded about the daemon it placed.
type DaemonState struct {
	PID    int    `json:"pid"`
	Binary string `json:"binary"`
	Range  Range  `json:"protocol"`
}

func HomeFor(posseHome string) string { return filepath.Join(posseHome, "greenhouse") }

func (d *Daemon) environment() []string {
	env := make([]string, 0, len(d.Env)+1)
	for _, entry := range d.Env {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "POSSE_") || strings.HasPrefix(key, "HERDR_") || key == "GREENHOUSE_HOME" ||
			key == "GIT_DIR" || key == "GIT_WORK_TREE" || key == "GIT_INDEX_FILE" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GREENHOUSE_HOME="+d.Home)
}

// Ensure returns the running daemon, starting and probing it first if needed.
// It refuses a daemon whose protocol range excludes version 2.
func (d *Daemon) Ensure(ctx context.Context) (DaemonState, error) {
	if err := d.refuseTaskCaller(); err != nil {
		return DaemonState{}, err
	}
	if err := os.MkdirAll(d.Home, 0o700); err != nil {
		return DaemonState{}, err
	}
	if err := os.Chmod(d.Home, 0o700); err != nil {
		return DaemonState{}, err
	}
	pid, live := d.livePID()
	if !live {
		var err error
		if pid, err = d.start(ctx); err != nil {
			return DaemonState{}, err
		}
	}
	if err := d.verifyPlacement(pid); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		return DaemonState{}, err
	}
	state, err := ReadState(d.Home)
	if err != nil || state.PID != pid {
		probed, err := d.probe(ctx)
		if err != nil {
			return DaemonState{}, err
		}
		state = DaemonState{PID: pid, Binary: d.Binary, Range: probed}
		encoded, _ := json.Marshal(state)
		if err := atomicfile.Write(filepath.Join(d.Home, stateFile), encoded, 0o600); err != nil {
			return DaemonState{}, err
		}
	}
	if !state.Range.Includes(Protocol) {
		return state, &ProtocolError{Daemon: state.Range}
	}
	return state, nil
}

func ReadState(home string) (DaemonState, error) {
	var state DaemonState
	data, err := os.ReadFile(filepath.Join(home, stateFile))
	if err != nil {
		return state, err
	}
	return state, json.Unmarshal(data, &state)
}

// LivePID returns the PID of the daemon serving home and whether it is alive.
func LivePID(home string) (int, bool) {
	return (&Daemon{Home: home}).livePID()
}

func (d *Daemon) livePID() (int, bool) {
	data, err := os.ReadFile(filepath.Join(d.Home, "pid"))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid < 2 || syscall.Kill(pid, 0) != nil {
		return pid, false
	}
	connection, err := net.DialTimeout("unix", filepath.Join(d.Home, "socket"), time.Second)
	if err != nil {
		return pid, false
	}
	_ = connection.Close()
	return pid, true
}

func (d *Daemon) start(ctx context.Context) (int, error) {
	log, err := os.OpenFile(filepath.Join(d.Home, logFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer log.Close()
	command := exec.Command(d.Binary, "daemon")
	command.Dir = d.Home
	command.Env = d.environment()
	command.Stdout, command.Stderr = log, log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return 0, fmt.Errorf("start Greenhouse daemon: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if pid, live := d.livePID(); live {
			return pid, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-deadline.C:
			return 0, fmt.Errorf("the Greenhouse daemon did not open its socket within 10s; see %s", filepath.Join(d.Home, logFile))
		case err := <-exited:
			// A concurrent start may own the home; its socket appears shortly.
			if pid, live := d.waitLive(2 * time.Second); live {
				return pid, nil
			}
			return 0, fmt.Errorf("the Greenhouse daemon exited before serving: %v; see %s", err, filepath.Join(d.Home, logFile))
		case <-ticker.C:
		}
	}
}

func (d *Daemon) waitLive(limit time.Duration) (int, bool) {
	for deadline := time.Now().Add(limit); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if pid, live := d.livePID(); live {
			return pid, true
		}
	}
	return 0, false
}

// probe reads the daemon's protocol range with a request outside every range,
// which a daemon refuses without changing anything. A daemon that answers
// without a protocol block predates negotiation and speaks only version 1.
func (d *Daemon) probe(ctx context.Context) (Range, error) {
	v, err := d.axi(ctx, d.Home, []byte(`{"version":999,"op":"inspect"}`))
	if err != nil {
		return Range{}, err
	}
	if v.Protocol == nil {
		return Range{Min: 1, Max: 1}, nil
	}
	return *v.Protocol, nil
}

// refuseTaskCaller keeps Greenhouse out of every Task: a daemon started from a
// Task process would join its cgroup and carry its marker, and that Task's
// Teardown would stop reviews of other Tasks.
func (d *Daemon) refuseTaskCaller() error {
	if os.Getenv("POSSE_TASK_ID") != "" || d.InTask(os.Getpid()) {
		return errors.New("a Task process never calls Greenhouse; run this from the Project Lead pane")
	}
	return nil
}

func (d *Daemon) verifyPlacement(pid int) error {
	if d.InTask(pid) {
		return fmt.Errorf("the Greenhouse daemon %d runs inside a Posse Task", pid)
	}
	environ, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return nil
	}
	for entry := range strings.SplitSeq(string(environ), "\x00") {
		if strings.HasPrefix(entry, "POSSE_TASK_ID=") {
			return fmt.Errorf("the Greenhouse daemon %d carries a Task marker", pid)
		}
	}
	return nil
}

// Idle reports whether no run of the daemon serving home is reviewing,
// cancelling or writing, from its versioned store file.
func Idle(home string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(home, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var store struct {
		Runs map[string]struct {
			Status string `json:"status"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(data, &store); err != nil {
		return false, fmt.Errorf("read Greenhouse store: %w", err)
	}
	for _, run := range store.Runs {
		if run.Status == "running" || run.Status == "cancelling" || run.Status == "writing" {
			return false, nil
		}
	}
	return true, nil
}

// Stop ends the daemon Posse placed in home with SIGTERM, the only stop
// Greenhouse documents, and waits for it to exit. It leaves a daemon Posse has
// no record of alone.
func Stop(home string, wait time.Duration) error {
	pid, live := LivePID(home)
	if !live {
		return nil
	}
	if state, err := ReadState(home); err != nil || state.PID != pid {
		return fmt.Errorf("the Greenhouse daemon %d serving %s was not placed by Posse", pid, home)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return nil
		}
	}
	return fmt.Errorf("the Greenhouse daemon %d did not exit within %s", pid, wait)
}
