//go:build e2e && linux

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type ptySession struct {
	t      *testing.T
	master *os.File
	mu     sync.Mutex
	output bytes.Buffer
	done   chan error
}

func startPty(t *testing.T, command *exec.Cmd, rows, columns uint16) *ptySession {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: columns}); err != nil {
		t.Fatal(err)
	}
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	session := &ptySession{t: t, master: master, done: make(chan error, 1)}
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, err := master.Read(buffer)
			session.mu.Lock()
			session.output.Write(buffer[:n])
			session.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() { session.done <- command.Wait() }()
	t.Cleanup(func() { _ = command.Process.Kill() })
	return session
}

func (s *ptySession) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.output.String()
}

// send types keys once the screen shows ready, so the picker has left cooked
// mode and reads them as single keys.
func (s *ptySession) send(ready, keys string) {
	s.t.Helper()
	if !waitForCondition(10*time.Second, func() bool { return strings.Contains(s.text(), ready) }) {
		s.t.Fatalf("screen never showed %q:\n%q", ready, s.text())
	}
	if _, err := s.master.WriteString(keys); err != nil {
		s.t.Fatal(err)
	}
}

func (s *ptySession) wait() error {
	select {
	case err := <-s.done:
		return err
	case <-time.After(10 * time.Second):
		s.t.Fatalf("command did not exit:\n%q", s.text())
		return nil
	}
}

func TestSkillsAddPickerSavesTheToggledSkills(t *testing.T) {
	f := newSkillsFixture(t)
	if err := os.MkdirAll(f.home, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(f.binary, "skills", "add")
	command.Dir, command.Env = f.home, f.env
	session := startPty(t, command, 40, 100)
	session.send("0 selected, 0 already in config", "\x1b[B ")
	session.send("1 selected, 0 already in config", "jj ")
	session.send("2 selected, 0 already in config", "\r")
	if err := session.wait(); err != nil {
		t.Fatalf("picker exited with %v:\n%q", err, session.text())
	}
	for _, want := range []string{"Add Posse skills to", "config.toml", "posse-whats-next", "lead", "change", "review", "research", "space toggle"} {
		if !strings.Contains(session.text(), want) {
			t.Fatalf("picker screen lacks %q:\n%q", want, session.text())
		}
	}
	got := f.configured(t)
	if strings.Join(got["lead"], ",") != "posse-catch-up,posse-research" || len(got["change"]) != 0 {
		t.Fatalf("configured = %#v, want only the two toggled lead skills", got)
	}
}

func TestSkillsAddPickerQuitWritesNothing(t *testing.T) {
	f := newSkillsFixture(t)
	if err := os.MkdirAll(f.home, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(f.binary, "skills", "add")
	command.Dir, command.Env = f.home, f.env
	session := startPty(t, command, 40, 100)
	session.send("0 selected", "a")
	session.send("25 selected", "q")
	if err := session.wait(); err != nil {
		t.Fatalf("picker exited with %v:\n%q", err, session.text())
	}
	if _, err := os.Stat(f.home + "/config.toml"); !os.IsNotExist(err) {
		t.Fatalf("quit wrote a config file: %v", err)
	}
}
