package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestMergeIntentFailurePreservesRetryableStoreContention(t *testing.T) {
	busy := mergeIntentFailure(fmt.Errorf("reserve merge intent: %w", store.ErrBusy))
	var busyFailure *axi.Error
	if !errors.As(busy, &busyFailure) || busyFailure.Code != "store_busy" || !busyFailure.Retryable {
		t.Fatalf("busy merge intent failure = %v, want retryable store_busy", busy)
	}
	active := mergeIntentFailure(errors.New("task already has an active merge --execute intent"))
	var activeFailure *axi.Error
	if !errors.As(active, &activeFailure) || activeFailure.Code != "intent_active" || !activeFailure.Retryable {
		t.Fatalf("active merge intent failure = %v, want retryable intent_active", active)
	}
}

func TestRetryBusyRepeatsOnlyTransientContention(t *testing.T) {
	calls := 0
	err := retryBusy(context.Background(), 4, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("update Project observation: %w", store.ErrBusy)
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("transient contention: err=%v calls=%d, want success on third call", err, calls)
	}

	calls = 0
	persistent := errors.New("project missing")
	err = retryBusy(context.Background(), 4, time.Millisecond, func() error {
		calls++
		return errors.Join(store.ErrBusy, persistent)
	})
	if !errors.Is(err, persistent) || calls != 1 {
		t.Fatalf("mixed failure: err=%v calls=%d, want one call", err, calls)
	}

	calls = 0
	err = retryBusy(context.Background(), 3, time.Millisecond, func() error {
		calls++
		return store.ErrBusy
	})
	if !store.IsBusy(err) || calls != 3 {
		t.Fatalf("exhausted: err=%v calls=%d, want busy after three calls", err, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = 0
	err = retryBusy(ctx, 4, time.Hour, func() error {
		calls++
		return store.ErrBusy
	})
	if !store.IsBusy(err) || calls != 1 {
		t.Fatalf("cancelled: err=%v calls=%d, want one call", err, calls)
	}
}
