package app

import (
	"context"
	"errors"
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
		if !parallel || errors.As(err, &protocolErr) || retries >= 3 {
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
