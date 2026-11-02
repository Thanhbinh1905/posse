//go:build windows

package app

import (
	"time"

	"golang.org/x/sys/windows"
)

func waitForPickerByte(fd uintptr, timeout time.Duration) (bool, error) {
	ready, err := windows.WaitForSingleObject(windows.Handle(fd), uint32(timeout.Milliseconds()))
	return ready == windows.WAIT_OBJECT_0, err
}
