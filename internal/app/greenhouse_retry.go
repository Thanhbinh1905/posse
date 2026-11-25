package app

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"strings"
	"syscall"
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
		if !parallel || errors.As(err, &protocolErr) || errors.As(err, &terminalErr) || reviewerSizeFailure(err) || retries >= 3 {
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

func transientGreenhouseFailure(err error) bool {
	if err == nil {
		return false
	}
	var terminalErr *greenhouse.TerminalReviewError
	if errors.As(err, &terminalErr) {
		return false
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && strings.Contains(strings.ToLower(err.Error()), "greenhouse axi:") {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ETIMEDOUT) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	reason := strings.ToLower(err.Error())
	for _, marker := range []string{"quota", "daemon unavailable", "rate limit", "rate-limit", "too many requests", "http 429", "status 429", "status code 429", "http 502", "bad gateway", "http 503", "http 504", "service unavailable", "gateway timeout", "network error", "network failure", "connection reset", "connection refused", "connection timed out", "network is unreachable", "temporary failure", "temporarily unavailable", "i/o timeout", "unexpected eof", "transport reset"} {
		if strings.Contains(reason, marker) {
			return true
		}
	}
	return false
}

func reviewerSizeFailure(err error) bool {
	if err == nil {
		return false
	}
	reason := strings.ToLower(err.Error())
	for _, marker := range []string{"intent_too_long", "prompt_exceeds_context", "context_overflow"} {
		if strings.Contains(reason, marker) {
			return true
		}
	}
	return reviewerContextOverflow(err)
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
