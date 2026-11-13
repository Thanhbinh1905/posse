package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/thanhbinh1905/posse/internal/atomicfile"
)

type parallelReviewAction string

const (
	parallelReviewSkip   parallelReviewAction = "skip"
	parallelReviewStart  parallelReviewAction = "start"
	parallelReviewResume parallelReviewAction = "resume"
)

type parallelReviewOwner struct {
	PID         int    `json:"pid"`
	Head        string `json:"head"`
	RequestedAt int64  `json:"requested_at"`
}

func nextParallelReviewAction(hasVerdict, hasPending bool) parallelReviewAction {
	if hasPending {
		return parallelReviewResume
	}
	if hasVerdict {
		return parallelReviewSkip
	}
	return parallelReviewStart
}

func parallelReviewLockPath(roundDir, repository string) string {
	return filepath.Join(roundDir, repository+".parallel.lock")
}

func parallelReviewOwnerPath(roundDir, repository string) string {
	return filepath.Join(roundDir, repository+".parallel-owner.json")
}

func parallelReviewPending(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func shouldRecordParallelRequestFailure(hasPending bool) bool {
	return !hasPending
}

func parallelRequestMetadata(head string, requestedAt int64, hasPending bool, owner *parallelReviewOwner) (string, int64) {
	if hasPending && owner != nil {
		if owner.Head != "" {
			head = owner.Head
		}
		if owner.RequestedAt > 0 {
			requestedAt = owner.RequestedAt
		}
	}
	return head, requestedAt
}

func acquireParallelReviewLock(path string) (*os.File, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return file, false, nil
}

func inheritedParallelReviewLock(fd int, path string) (*os.File, error) {
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		return nil, errors.New("invalid inherited parallel review lock")
	}
	fileInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	pathInfo, err := os.Stat(path)
	if err != nil || !os.SameFile(fileInfo, pathInfo) {
		_ = file.Close()
		return nil, errors.New("inherited parallel review lock does not match its path")
	}
	return file, nil
}

func parallelReviewInFlight(path string) (bool, error) {
	file, busy, err := acquireParallelReviewLock(path)
	if err != nil || busy {
		return busy, err
	}
	return false, releaseParallelReviewLock(file)
}

func releaseParallelReviewLock(file *os.File) error {
	if file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}

func readParallelReviewOwner(path string) (*parallelReviewOwner, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var owner parallelReviewOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return nil, err
	}
	if owner.PID < 2 || owner.Head == "" {
		return nil, errors.New("invalid parallel review owner")
	}
	return &owner, nil
}

func writeParallelReviewOwner(path string, owner parallelReviewOwner) error {
	data, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, data, 0o600)
}

func clearParallelReviewOwner(path, pending string, pid int) error {
	owner, err := readParallelReviewOwner(path)
	if err != nil || owner == nil || owner.PID != pid {
		return err
	}
	if _, err := os.Stat(pending); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
