//go:build linux

package app

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/thanhbinh1905/posse/internal/store"
)

type mountProcessHandle struct {
	pid       int
	fd        int
	bootID    string
	startTime string
}

func processIdentityCapabilityError() string { return "" }

func mountProcessGroupID(pid int) (int, error) {
	fields, err := mountProcessStatFields(pid)
	if err != nil {
		return 0, err
	}
	if len(fields) < 3 {
		return 0, fmt.Errorf("process stat for PID %d has no process group", pid)
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil || group <= 0 {
		return 0, fmt.Errorf("invalid process group for PID %d", pid)
	}
	return group, nil
}

func mountProcessParentID(pid int) (int, error) {
	fields, err := mountProcessStatFields(pid)
	if err != nil {
		return 0, err
	}
	if len(fields) < 2 {
		return 0, fmt.Errorf("process stat for PID %d has no parent process", pid)
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil || parent < 0 {
		return 0, fmt.Errorf("invalid parent process for PID %d", pid)
	}
	return parent, nil
}

func mountProcessStatFields(pid int) ([]string, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}
	commandEnd := strings.LastIndexByte(string(stat), ')')
	if commandEnd < 0 {
		return nil, fmt.Errorf("malformed process stat for PID %d", pid)
	}
	return strings.Fields(string(stat)[commandEnd+1:]), nil
}

func openMountProcessHandle(pid int) (*mountProcessHandle, error) {
	bootID, startTime, err := store.ProcessIdentityForPID(pid)
	if err != nil {
		return nil, err
	}
	fd, err := openPIDFD(pid)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
			return nil, fmt.Errorf("pidfd is unavailable for PID %d, refusing to signal it: %w", pid, err)
		}
		return nil, fmt.Errorf("open pidfd for PID %d: %w", pid, err)
	}
	handle := &mountProcessHandle{pid: pid, fd: fd, bootID: bootID, startTime: startTime}
	currentBootID, currentStartTime, err := store.ProcessIdentityForPID(pid)
	if err != nil || currentBootID != bootID || currentStartTime != startTime {
		_ = handle.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("PID %d changed process identity while opening pidfd", pid)
	}
	return handle, nil
}

func (handle *mountProcessHandle) PID() int { return handle.pid }

func (handle *mountProcessHandle) Identity() string { return handle.bootID + "/" + handle.startTime }

func openPIDFD(pid int) (int, error) {
	for {
		fd, err := unix.PidfdOpen(pid, 0)
		if !errors.Is(err, unix.EINTR) {
			return fd, err
		}
	}
}

func (handle *mountProcessHandle) Signal(signal syscall.Signal) error {
	for {
		err := unix.PidfdSendSignal(handle.fd, unix.Signal(signal), nil, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("signal process instance %d: %w", handle.pid, err)
		}
		return nil
	}
}

func (handle *mountProcessHandle) Alive() (bool, error) {
	poll := []unix.PollFd{{Fd: int32(handle.fd), Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(poll, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, err
		}
		return poll[0].Revents&unix.POLLIN == 0, nil
	}
}

func (handle *mountProcessHandle) Close() error {
	if handle.fd < 0 {
		return nil
	}
	err := unix.Close(handle.fd)
	handle.fd = -1
	return err
}
