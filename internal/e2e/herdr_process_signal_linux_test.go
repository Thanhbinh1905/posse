//go:build e2e && linux

package e2e

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

func signalIsolatedHerdrServer(server isolatedHerdrServerProcess, signal syscall.Signal) error {
	// Bind the signal to the inspected process instance so PID reuse cannot
	// turn cleanup into a signal to an unrelated process.
	fd, err := unix.PidfdOpen(server.PID, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if e2eProcessStartTicks(server.PID) != server.StartTicks {
		return fmt.Errorf("isolated Herdr server PID %d changed identity", server.PID)
	}
	for {
		err := unix.PidfdSendSignal(fd, unix.Signal(signal), nil, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		return err
	}
}
