package app

import (
	"testing"
	"time"
)

func TestRecoveryBackoffDoublesAndCaps(t *testing.T) {
	for _, test := range []struct {
		initial  time.Duration
		attempts int
		want     time.Duration
	}{
		{5 * time.Second, 1, 5 * time.Second},
		{5 * time.Second, 2, 10 * time.Second},
		{5 * time.Second, 3, 20 * time.Second},
		{5 * time.Second, 5, time.Minute},
		{5 * time.Second, 1000000, time.Minute},
		{2 * time.Minute, 1, time.Minute},
	} {
		if got := recoveryBackoff(test.initial, test.attempts); got != test.want {
			t.Errorf("backoff(%s,%d)=%s, want %s", test.initial, test.attempts, got, test.want)
		}
	}
}
