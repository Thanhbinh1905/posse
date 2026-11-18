package greenhouse

import "testing"

func TestNextGrantNumberIncludesSettledReviewUsage(t *testing.T) {
	got := nextGrantNumber(&run{HostGrants: []string{"shop/t1/shop/r1"}, ReviewUsed: 2})
	if got != 3 {
		t.Fatalf("next grant number = %d, want 3 after two settled reviews", got)
	}
}

func TestTerminalReviewStatus(t *testing.T) {
	for _, status := range []string{"cancelled", "failed", "unconfigured", "refused"} {
		if !terminalReviewStatus(status) {
			t.Errorf("terminalReviewStatus(%q) = false, want true", status)
		}
	}
	for _, status := range []string{"running", "cancelling", "paused"} {
		if terminalReviewStatus(status) {
			t.Errorf("terminalReviewStatus(%q) = true, want false", status)
		}
	}
}
