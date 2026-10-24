package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func acquireHandoffWatchLock(home, destination string, pid int) (*os.File, int, error) {
	directory := filepath.Join(home, "watch-handoffs")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, 0, err
	}
	digest := sha256.Sum256([]byte(destination))
	path := filepath.Join(directory, hex.EncodeToString(digest[:])+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, 0, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			contents, readErr := os.ReadFile(path)
			activePID, parseErr := strconv.Atoi(strings.TrimSpace(string(contents)))
			if readErr == nil && parseErr == nil && activePID > 0 {
				return nil, activePID, nil
			}
			return nil, 0, fmt.Errorf("handoff Watch lock is held but its active PID could not be read: %w", err)
		}
		return nil, 0, err
	}
	if err := file.Truncate(0); err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return nil, 0, err
	}
	if _, err := file.WriteAt([]byte(strconv.Itoa(pid)+"\n"), 0); err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return nil, 0, err
	}
	return file, 0, nil
}
