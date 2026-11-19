package app

import "testing"

func TestDeliveredByPullRequestIncludesGreenhouseDelivery(t *testing.T) {
	for _, mode := range []string{"greenhouse", "pr"} {
		if !deliveredByPullRequest(mode) {
			t.Errorf("deliveredByPullRequest(%q) = false", mode)
		}
	}
	for _, mode := range []string{"local", "unknown"} {
		if deliveredByPullRequest(mode) {
			t.Errorf("deliveredByPullRequest(%q) = true", mode)
		}
	}
}
