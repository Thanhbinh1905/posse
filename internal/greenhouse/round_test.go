package greenhouse

import "testing"

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
