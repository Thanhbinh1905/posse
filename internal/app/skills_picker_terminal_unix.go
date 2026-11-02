//go:build !windows

package app

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

func waitForPickerByte(fd uintptr, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, nil
		}
		milliseconds := int(remaining / time.Millisecond)
		if milliseconds < 1 {
			milliseconds = 1
		}
		ready, err := unix.Poll(poll, milliseconds)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return ready > 0, err
	}
}
