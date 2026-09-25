//go:build linux

package herdr

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestServerStartedAtSurvivesSocketChurn covers the restart flake where
// generation detection returned "" while other Unix sockets, including
// connections to the server itself, opened and closed.
func TestServerStartedAtSurvivesSocketChurn(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go acceptAndClose(listener)
	want := selfGeneration(t)

	var extra []net.Listener
	for index := 0; index < 200; index++ {
		other, err := net.Listen("unix", filepath.Join(dir, "extra-"+strconv.Itoa(index)+".sock"))
		if err != nil {
			t.Fatal(err)
		}
		extra = append(extra, other)
	}
	defer func() {
		for _, other := range extra {
			other.Close()
		}
	}()
	stop := make(chan struct{})
	var churn sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		churn.Add(1)
		go func(worker int) {
			defer churn.Done()
			path := filepath.Join(dir, "churn-"+strconv.Itoa(worker)+".sock")
			for {
				select {
				case <-stop:
					return
				case <-time.After(100 * time.Microsecond):
				}
				if other, err := net.Listen("unix", path); err == nil {
					other.Close()
				}
				if connection, err := net.Dial("unix", socketPath); err == nil {
					connection.Close()
				}
			}
		}(worker)
	}
	defer func() {
		close(stop)
		churn.Wait()
	}()

	for attempt := 0; attempt < 300; attempt++ {
		if got := serverStartedAt(context.Background(), socketPath); got != want {
			t.Fatalf("attempt %d: serverStartedAt = %q, want %q", attempt, got, want)
		}
	}
}

// TestServerStartedAtWaitsForFullBacklog covers a busy server: Linux fails a
// connect to a Unix listener with a full backlog at once with EAGAIN, and the
// generation must still be read once the server accepts again.
func TestServerStartedAtWaitsForFullBacklog(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener := listenUnixWithBacklog(t, socketPath, 1)
	defer listener.Close()
	want := selfGeneration(t)

	var pending []net.Conn
	defer func() {
		for _, connection := range pending {
			connection.Close()
		}
	}()
	for {
		connection, err := net.Dial("unix", socketPath)
		if errors.Is(err, syscall.EAGAIN) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if pending = append(pending, connection); len(pending) > 64 {
			t.Fatal("the listener backlog never filled")
		}
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		acceptAndClose(listener)
	}()

	if got := serverStartedAt(context.Background(), socketPath); got != want {
		t.Fatalf("serverStartedAt with a full backlog = %q, want %q", got, want)
	}
}

func acceptAndClose(listener net.Listener) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		connection.Close()
	}
}

func selfGeneration(t *testing.T) string {
	t.Helper()
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	ticks, err := procStartTicks(string(stat))
	if err != nil {
		t.Fatal(err)
	}
	return strconv.Itoa(os.Getpid()) + ":" + ticks
}

func listenUnixWithBacklog(t *testing.T, path string, backlog int) net.Listener {
	t.Helper()
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: path}); err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	if err := unix.Listen(fd, backlog); err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	listener, err := net.FileListener(file)
	if err != nil {
		t.Fatal(err)
	}
	return listener
}
