//go:build linux

package app

import (
	"bytes"
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func TestSkillPickerEscapeCancelsOnOpenTerminal(t *testing.T) {
	masterFD, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlSetPointerInt(masterFD, unix.TIOCSPTLCK, 0); err != nil {
		unix.Close(masterFD)
		t.Fatal(err)
	}
	terminalNumber, err := unix.IoctlGetUint32(masterFD, unix.TIOCGPTN)
	if err != nil {
		unix.Close(masterFD)
		t.Fatal(err)
	}
	slaveFD, err := unix.Open(fmt.Sprintf("/dev/pts/%d", terminalNumber), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Close(masterFD)
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(masterFD), "pty-master")
	slave := os.NewFile(uintptr(slaveFD), "pty-slave")
	defer master.Close()
	defer slave.Close()
	state, err := term.MakeRaw(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer term.Restore(int(slave.Fd()), state)

	type outcome struct {
		saved bool
		err   error
	}
	result := make(chan outcome, 1)
	go func() {
		_, saved, err := runSkillPicker(slave, &bytes.Buffer{}, skillPicker{choices: testPickerChoices()})
		result <- outcome{saved: saved, err: err}
	}()
	if _, err := master.Write([]byte{0x1b}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.saved || got.err != nil {
			t.Fatalf("Escape cancellation = saved %v, err %v", got.saved, got.err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("picker waited for another terminal byte after Escape")
	}
}
