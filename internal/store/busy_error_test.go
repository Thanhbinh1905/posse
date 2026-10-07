package store

import (
	"errors"
	"fmt"
	"testing"
)

func TestWithoutBusyKeepsOnlyPersistentFailures(t *testing.T) {
	busy := fmt.Errorf("update Task t2 observation: %w", ErrBusy)
	persistent := fmt.Errorf("update Task t8 observation: %w", errors.New("constraint failed"))

	got := WithoutBusy(fmt.Errorf("maintenance: %w", errors.Join(busy, persistent)))
	if got == nil || got.Error() != "maintenance: update Task t8 observation: constraint failed" {
		t.Fatalf("WithoutBusy(mixed) = %v, want only the contextual persistent failure", got)
	}
	if got := WithoutBusy(errors.Join(busy, ErrBusy)); got != nil {
		t.Fatalf("WithoutBusy(busy) = %v, want nil", got)
	}
	if got := WithoutBusy(persistent); got == nil || got.Error() != persistent.Error() {
		t.Fatalf("WithoutBusy(persistent) changed the error: %v", got)
	}
}

func TestIsOnlyBusyClassifiesEveryJoinedFailure(t *testing.T) {
	busy := fmt.Errorf("Task observation: %w", ErrBusy)
	other := errors.New("non-transient database constraint")

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "busy", err: ErrBusy, want: true},
		{name: "wrapped busy", err: fmt.Errorf("maintenance: %w", busy), want: true},
		{name: "joined busy errors", err: errors.Join(busy, fmt.Errorf("Lead observation: %w", ErrBusy)), want: true},
		{name: "busy and persistent", err: errors.Join(busy, other), want: false},
		{name: "wrapped mixed errors", err: fmt.Errorf("maintenance: %w", errors.Join(busy, other)), want: false},
		{name: "persistent", err: other, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsOnlyBusy(test.err); got != test.want {
				t.Fatalf("IsOnlyBusy(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}
