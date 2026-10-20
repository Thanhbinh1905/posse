//go:build linux

package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/thanhbinh1905/posse/internal/store"
)

type sandboxProcessHandle struct {
	pid                int
	fd                 int
	bootID             string
	startTime          string
	executableIdentity string
	signalGuard        func(syscall.Signal) error
}

func processIdentityCapabilityError() string { return "" }

func sandboxProcessExecutableIdentity(pid int) (string, error) {
	info, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("process executable for PID %d has no Linux file identity", pid)
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), nil
}

func sandboxProcessGroupID(pid int) (int, error) {
	fields, err := sandboxProcessStatFields(pid)
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

func sandboxProcessParentID(pid int) (int, error) {
	fields, err := sandboxProcessStatFields(pid)
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

func sandboxProcessStatFields(pid int) ([]string, error) {
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

func openSandboxProcessHandle(pid int) (*sandboxProcessHandle, error) {
	bootID, startTime, err := store.ProcessIdentityForPID(pid)
	if err != nil {
		return nil, err
	}
	executableIdentity, err := sandboxProcessExecutableIdentity(pid)
	if err != nil {
		return nil, fmt.Errorf("inspect executable image for PID %d: %w", pid, err)
	}
	fd, err := openPIDFD(pid)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
			return nil, fmt.Errorf("pidfd is unavailable for PID %d, refusing to signal it: %w", pid, err)
		}
		return nil, fmt.Errorf("open pidfd for PID %d: %w", pid, err)
	}
	handle := &sandboxProcessHandle{pid: pid, fd: fd, bootID: bootID, startTime: startTime, executableIdentity: executableIdentity}
	currentBootID, currentStartTime, err := store.ProcessIdentityForPID(pid)
	if err != nil || currentBootID != bootID || currentStartTime != startTime {
		_ = handle.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("PID %d changed process identity while opening pidfd", pid)
	}
	currentExecutableIdentity, err := sandboxProcessExecutableIdentity(pid)
	if err != nil || currentExecutableIdentity != executableIdentity {
		_ = handle.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("PID %d changed executable image while opening pidfd", pid)
	}
	return handle, nil
}

func (handle *sandboxProcessHandle) PID() int { return handle.pid }

func (handle *sandboxProcessHandle) Identity() string { return handle.bootID + "/" + handle.startTime }

func (handle *sandboxProcessHandle) ExecutableIdentity() string { return handle.executableIdentity }

func openPIDFD(pid int) (int, error) {
	for {
		fd, err := unix.PidfdOpen(pid, 0)
		if !errors.Is(err, unix.EINTR) {
			return fd, err
		}
	}
}

func (handle *sandboxProcessHandle) Signal(signal syscall.Signal) error {
	if handle.signalGuard != nil {
		if err := handle.signalGuard(signal); err != nil {
			return fmt.Errorf("refuse to signal Sandbox process instance %d after ownership changed: %w", handle.pid, err)
		}
	}
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

func (handle *sandboxProcessHandle) Alive() (bool, error) {
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

func (handle *sandboxProcessHandle) Close() error {
	if handle.fd < 0 {
		return nil
	}
	err := unix.Close(handle.fd)
	handle.fd = -1
	return err
}
