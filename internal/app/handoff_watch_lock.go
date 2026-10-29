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
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"golang.org/x/sys/unix"
)

// handoffWatchHolder identifies the live handoff Watch that holds a
// destination's lock.
type handoffWatchHolder struct {
	PID       int
	StartedAt int64
}

func handoffWatchLockPath(home, destination string) string {
	digest := sha256.Sum256([]byte(destination))
	return filepath.Join(home, "watch-handoffs", hex.EncodeToString(digest[:])+".lock")
}

// acquireHandoffWatchLock returns the held lock, or the live holder when
// another handoff Watch already owns the destination.
func acquireHandoffWatchLock(home, destination string, pid int) (*os.File, handoffWatchHolder, error) {
	path := handoffWatchLockPath(home, destination)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, handoffWatchHolder{}, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, handoffWatchHolder{}, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			holder, active, readErr := inspectHandoffWatchLock(path)
			if readErr != nil {
				return nil, handoffWatchHolder{}, readErr
			}
			if active {
				return nil, holder, nil
			}
			// The holder exited between the two lock attempts.
			return acquireHandoffWatchLock(home, destination, pid)
		}
		return nil, handoffWatchHolder{}, err
	}
	if err := file.Truncate(0); err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return nil, handoffWatchHolder{}, err
	}
	if _, err := file.WriteAt([]byte(fmt.Sprintf("%d\n%d\n", pid, currentTime())), 0); err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return nil, handoffWatchHolder{}, err
	}
	return file, handoffWatchHolder{}, nil
}

// handoffWatchStatus reports the live handoff Watch for destination, if any.
func handoffWatchStatus(home, destination string) (handoffWatchHolder, bool, error) {
	return inspectHandoffWatchLock(handoffWatchLockPath(home, destination))
}

// inspectHandoffWatchLock probes the lock without holding it. A holder that
// has just taken the lock may not have written its identity yet, so a held
// lock with unreadable contents is read again briefly before failing.
func inspectHandoffWatchLock(path string) (handoffWatchHolder, bool, error) {
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		if attempt > 0 {
			time.Sleep(10 * time.Millisecond)
		}
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			return handoffWatchHolder{}, false, nil
		}
		if err != nil {
			return handoffWatchHolder{}, false, err
		}
		err = unix.Flock(int(file.Fd()), unix.LOCK_SH|unix.LOCK_NB)
		if err == nil {
			_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
			_ = file.Close()
			return handoffWatchHolder{}, false, nil
		}
		_ = file.Close()
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return handoffWatchHolder{}, false, err
		}
		holder, parseErr := readHandoffWatchHolder(path)
		if parseErr == nil {
			return holder, true, nil
		}
		lastErr = parseErr
	}
	return handoffWatchHolder{}, false, fmt.Errorf("handoff Watch lock is held but its active PID could not be read: %w", lastErr)
}

func readHandoffWatchHolder(path string) (handoffWatchHolder, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return handoffWatchHolder{}, err
	}
	fields := strings.Fields(string(contents))
	if len(fields) == 0 {
		return handoffWatchHolder{}, errors.New("lock file is empty")
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid < 1 {
		return handoffWatchHolder{}, fmt.Errorf("invalid PID %q", fields[0])
	}
	holder := handoffWatchHolder{PID: pid}
	// Locks written before start times were recorded hold only the PID.
	if len(fields) > 1 {
		if startedAt, err := strconv.ParseInt(fields[1], 10, 64); err == nil && startedAt > 0 {
			holder.StartedAt = startedAt
		}
	}
	return holder, nil
}

// handoffWatchResult describes a destination's handoff Watch. It never carries
// notices or a delivery, so a receiver cannot mistake it for a Notice batch.
func handoffWatchResult(state, destination string, holder handoffWatchHolder) axi.Object {
	result := axi.Object{{Key: "state", Value: state}, {Key: "destination", Value: destination}}
	if holder.PID == 0 {
		return result
	}
	result = append(result, axi.Field{Key: "pid", Value: holder.PID})
	if holder.StartedAt > 0 {
		result = append(result, axi.Field{Key: "started_at", Value: time.UnixMilli(holder.StartedAt).UTC().Format(time.RFC3339)})
	}
	return result
}
