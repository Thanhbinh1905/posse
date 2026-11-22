package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/greenhouse"
)

func reviewGreenhouseWithRetries(ctx context.Context, parallel bool, wait time.Duration, review func(time.Duration) (greenhouse.RoundResult, error)) (greenhouse.RoundResult, int, error) {
	for retries := 0; ; retries++ {
		result, err := review(wait)
		if err == nil {
			return result, retries + 1, nil
		}
		var protocolErr *greenhouse.ProtocolError
		var terminalErr *greenhouse.TerminalReviewError
		if !parallel || errors.As(err, &protocolErr) || errors.As(err, &terminalErr) || reviewerContextOverflow(err) || retries >= 3 {
			return result, retries + 1, err
		}
		timer := time.NewTimer(time.Duration(1<<retries) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, retries + 1, ctx.Err()
		case <-timer.C:
		}
	}
}

func reviewerContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	reason := strings.ToLower(err.Error())
	for _, marker := range []string{"context overflow", "context length", "maximum context", "context window", "prompt is too long", "input is too long", "prompt exceeds"} {
		if strings.Contains(reason, marker) {
			return true
		}
	}
	return false
}
