package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestT199LegacyHumanEditIsPreservedOnRefresh(t *testing.T) {
	for _, forge := range []string{"github", "gitlab"} {
		t.Run(forge, func(t *testing.T) {
			fixture, legacyBody := t201LegacyFixture(t, forge)
			legacyBody = strings.Replace(legacyBody, "## Verification", "## Maintainer Review\n\nHuman release note: deployment needs approval.\n\n## Verification", 1)
			t201SetLegacyBody(t, fixture, forge, legacyBody)
			if err := os.WriteFile(fixture.ghLog, nil, 0o600); err != nil {
				t.Fatal(err)
			}

			code, output, stderr := fixture.run("publish", "--refresh", "Second summary", "--verify", "second tests -> pass", "--proof", "second proof", "--risk", "Risk: second\nRollback: second")
			if code != 0 {
				t.Fatalf("publish --refresh: code=%d output=%s stderr=%s", code, output, stderr)
			}
			calls := readFileForPublishTest(t, fixture.ghLog)
			if strings.Contains(calls, "pr create") || strings.Contains(calls, "mr create") {
				t.Fatalf("refresh created a duplicate request: %s", calls)
			}
			updatedPath := os.Getenv("POSSE_TEST_GH_EDIT_BODY")
			if forge == "gitlab" {
				updatedPath = os.Getenv("POSSE_TEST_GLAB_DESCRIPTION")
			}
			updated := readFileForPublishTest(t, updatedPath)
			if !strings.HasPrefix(updated, legacyBody) || !strings.Contains(updated, "Human release note: deployment needs approval.") {
				t.Fatalf("refresh did not preserve the entire legacy description: %s", updated)
			}
			if !strings.Contains(updated, publishBodyStart+"\n## Summary\n\nSecond summary") || !strings.HasSuffix(updated, publishBodyEnd) {
				t.Fatalf("refresh did not append the current managed block: %s", updated)
			}
			task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
			if err != nil || task.State != store.StateWorking {
				t.Fatalf("publish changed Task state: %#v %v", task, err)
			}
		})
	}
}
