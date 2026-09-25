//go:build linux

package herdr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// serverStartedAt identifies the Herdr server generation as "pid:start-ticks"
// of the process listening on socketPath. The listener's pid comes from
// SO_PEERCRED on a fresh connection; scanning /proc/net/unix instead can skip
// the socket's line while other sockets open and close during the read.
func serverStartedAt(ctx context.Context, socketPath string) string {
	connection, err := dialBusySocket(ctx, socketPath)
	if err != nil {
		return ""
	}
	defer connection.Close()
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return ""
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return ""
	}
	var credentials *unix.Ucred
	var credentialsErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, credentialsErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || credentialsErr != nil || credentials == nil || credentials.Pid <= 0 {
		return ""
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(int(credentials.Pid)), "stat"))
	if err != nil {
		return ""
	}
	started, err := procStartTicks(string(stat))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%s", credentials.Pid, started)
}

// dialBusySocket retries a Unix socket connect that fails with EAGAIN, which
// Linux returns at once when the listener's backlog is full.
func dialBusySocket(ctx context.Context, socketPath string) (net.Conn, error) {
	deadline := time.Now().Add(time.Second)
	delay := 5 * time.Millisecond
	for {
		connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		if err == nil || !errors.Is(err, syscall.EAGAIN) || time.Now().After(deadline) {
			return connection, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 50*time.Millisecond {
			delay *= 2
		}
	}
}

func procStartTicks(stat string) (string, error) {
	close := strings.LastIndex(stat, ")")
	if close < 0 || close+1 >= len(stat) {
		return "", fmt.Errorf("invalid proc stat")
	}
	fields := strings.Fields(stat[close+1:])
	if len(fields) <= 19 {
		return "", fmt.Errorf("proc stat has no start time")
	}
	return fields[19], nil
}
