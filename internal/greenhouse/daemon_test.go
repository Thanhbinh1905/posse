package greenhouse

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestDaemonEnvironmentDropsTaskAndPaneIdentity(t *testing.T) {
	d := Daemon{Binary: "/home/user/.local/bin/greenhouse", Home: "/posse/greenhouse", Env: []string{"PATH=/bin", "POSSE_TASK_ID=marker", "POSSE_HOME=/posse", "HERDR_PANE_ID=p1", "GREENHOUSE_HOME=/user/greenhouse", "GIT_DIR=/repo/.git", "PI_CODING_AGENT_DIR=/agent"}}
	got := d.environment()
	want := []string{"PATH=/bin", "PI_CODING_AGENT_DIR=/agent", "GREENHOUSE_HOME=/posse/greenhouse"}
	if !slices.Equal(got, want) {
		t.Fatalf("environment() = %v, want %v", got, want)
	}
}

func TestDaemonEnvironmentFindsGreenhouseAndPiOutsideCallerPATH(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"greenhouse", "pi"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	d := Daemon{Binary: filepath.Join(bin, "greenhouse"), Home: filepath.Join(t.TempDir(), "greenhouse"), Env: []string{"PATH=/usr/bin:/bin"}}
	command := exec.Command("/bin/sh", "-c", "command -v greenhouse; command -v pi")
	command.Env = d.daemonEnvironment()
	output, err := command.Output()
	if err != nil {
		t.Fatalf("resolve daemon tools: %v", err)
	}
	want := filepath.Join(bin, "greenhouse") + "\n" + filepath.Join(bin, "pi") + "\n"
	if string(output) != want {
		t.Fatalf("daemon tools = %q, want %q", output, want)
	}
}

func TestAXIReportsTransientServerEOF(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "greenhouse")
	script := `#!/bin/sh
printf '%s\n' '{"version":1,"outcome":"error","error":"EOF"}'
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Binary: binary, Home: filepath.Join(dir, "home")}
	_, err := d.axi(context.Background(), dir, []byte(`{"version":2,"op":"attach"}`))
	if !errors.Is(err, errUnavailable) {
		t.Fatalf("axi error = %v, want transient daemon unavailability", err)
	}
}

func TestEnsureRecordsTheRunningDaemonExecutable(t *testing.T) {
	home := t.TempDir()
	process := exec.Command("/bin/sleep", "30")
	for _, entry := range os.Environ() {
		if key, _, _ := strings.Cut(entry, "="); key != "POSSE_TASK_ID" {
			process.Env = append(process.Env, entry)
		}
	}
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})
	if err := os.WriteFile(filepath.Join(home, "pid"), []byte(strconv.Itoa(process.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(home, "socket"))
	if err != nil {
		t.Skipf("Unix sockets are unavailable: %v", err)
	}
	defer listener.Close()
	state, err := json.Marshal(DaemonState{PID: process.Process.Pid, Binary: "/configured/greenhouse", Range: Range{Min: 1, Max: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, stateFile), state, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TASK_ID", "")
	d := &Daemon{Binary: "/configured/greenhouse", Home: home, InTask: func(int) bool { return false }}
	got, err := d.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(process.Process.Pid), "exe"))
	if err != nil {
		t.Skipf("process executable lookup is unavailable: %v", err)
	}
	if got.Binary != want {
		t.Fatalf("Ensure() binary = %q, want %q", got.Binary, want)
	}
	persisted, err := ReadState(home)
	if err != nil || persisted.Binary != want {
		t.Fatalf("persisted daemon binary = %q, %v; want %q", persisted.Binary, err, want)
	}
}

func TestRunningExecutableReportsTheProcessExecutable(t *testing.T) {
	got, err := RunningExecutable(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.Readlink("/proc/self/exe")
	if err != nil {
		t.Skipf("process executable lookup is unavailable: %v", err)
	}
	if got != want {
		t.Fatalf("RunningExecutable() = %q, want %q", got, want)
	}
}

func TestProtocolErrorNamesBothRanges(t *testing.T) {
	err := (&ProtocolError{Daemon: Range{Min: 1, Max: 1}}).Error()
	if want := "the Greenhouse daemon speaks protocol 1 and Posse needs protocol 2"; err[:len(want)] != want {
		t.Fatalf("ProtocolError = %q", err)
	}
	if (Range{Min: 1, Max: 2}).String() != "1-2" || !(Range{Min: 1, Max: 2}).Includes(Protocol) || (Range{Min: 1, Max: 1}).Includes(Protocol) {
		t.Fatal("Range comparison is wrong")
	}
}
