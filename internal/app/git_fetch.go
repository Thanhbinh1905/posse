package app

import (
	"context"
	"strings"
	"time"
)

const gitFetchAttempts = 4

func gitFetch(ctx context.Context, root string, args ...string) (string, error) {
	commandArgs := append([]string{"fetch"}, args...)
	for attempt := 0; ; attempt++ {
		output, err := gitOutput(ctx, root, commandArgs...)
		if err == nil || attempt+1 == gitFetchAttempts || !isTransientGitFetchLock(err) {
			return output, err
		}
		delay := 25 * time.Millisecond * time.Duration(1<<attempt)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		}
	}
}

func isTransientGitFetchLock(err error) bool {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "cannot lock ref") || strings.Contains(message, "another git process seems to be running") {
		return true
	}
	return strings.Contains(message, ".lock") &&
		(strings.Contains(message, "file exists") || strings.Contains(message, "unable to create") || strings.Contains(message, "could not create"))
}
