package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestReplaceValidationBlockTouchesOnlyOwnedBlock(t *testing.T) {
	forged := managedPublishBody("Human note\n"+validationBlockStart+"\nkeep\n"+validationBlockEnd, "forged-token")
	oldBlock := validationSummary{Head: "aaaaaaaaaaaa", Review: "full-pipeline", MaxRounds: 3, RoundsUsed: 1, Remaining: 2, Findings: map[string]int{"open": 2}, CI: "pending"}.markdown()
	newBlock := validationSummary{Head: "bbbbbbbbbbbb", Review: "full-pipeline", MaxRounds: 3, RoundsUsed: 2, Remaining: 1, LatestVerdict: "pass", Findings: map[string]int{"fixed": 2}, Carries: 1, CI: "success"}.markdown()
	original := "Maintainer intro\n\n" + forged + "\n\n" + managedPublishBody("## Summary\n\nWorker text\n\n"+oldBlock, testPublishToken) + "\n\nMaintainer footer\n"

	updated, ok := replaceValidationBlock(original, testPublishToken, newBlock)
	if !ok {
		t.Fatalf("owned block was not replaced: %s", updated)
	}
	want := strings.Replace(original, oldBlock, newBlock, 1)
	if updated != want {
		t.Fatalf("refresh changed text outside the Validation block:\n got: %q\nwant: %q", updated, want)
	}
	if again, ok := replaceValidationBlock(updated, testPublishToken, newBlock); !ok || again != updated {
		t.Fatalf("refresh is not idempotent: %q", again)
	}
}

func TestReplaceValidationBlockInsertsIntoSpanWithoutBlock(t *testing.T) {
	block := validationSummary{Head: "cccccccccccc", Review: "quick", Findings: map[string]int{}, CI: "unknown"}.markdown()
	original := "Intro\n\n" + managedPublishBody("## Summary\n\nWorker text", testPublishToken) + "\n\nFooter\n"
	updated, ok := replaceValidationBlock(original, testPublishToken, block)
	if !ok || !strings.HasPrefix(updated, "Intro\n\n") || !strings.HasSuffix(updated, "\n\nFooter\n") || !strings.Contains(updated, "Worker text\n\n"+block+"\n"+publishBodyMarker("end", testPublishToken)) {
		t.Fatalf("block was not inserted before the end marker: %q", updated)
	}
}

func TestReplaceValidationBlockRefusesUnownedBodies(t *testing.T) {
	block := validationSummary{Findings: map[string]int{}}.markdown()
	for name, body := range map[string]string{
		"no markers":      "Human text only",
		"foreign token":   managedPublishBody("x", "other-token"),
		"duplicate start": managedPublishBody("x", testPublishToken) + "\n" + publishBodyMarker("start", testPublishToken),
		"half block":      managedPublishBody(validationBlockStart+"\nx", testPublishToken),
	} {
		if updated, ok := replaceValidationBlock(body, testPublishToken, block); ok {
			t.Errorf("%s: body was rewritten: %q", name, updated)
		}
	}
}

func TestWatchRefreshesValidationBlockWithoutTouchingHumanText(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateWorking)
	defer f.db.Close()
	ctx := context.Background()
	f.setGHHead(t, f.headSHA)
	if code, output, stderr := f.run("publish", "Worker completion summary"); code != 0 {
		t.Fatalf("publish: %d %s %s", code, output, stderr)
	}
	if err := f.db.UpdateTaskMerge(ctx, f.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	marker, err := f.db.GetPRBodyMarker(ctx, task.ID, "")
	if err != nil || marker.PRURL == "" {
		t.Fatalf("publish did not bind a marker: %+v %v", marker, err)
	}
	_, body, err := prDetails(ctx, f.db, f.project, task, f.service.homePath, "", "Worker completion summary", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	human := "Maintainer intro\n\n" + managedPublishBody(body, marker.Token) + "\n\nMaintainer footer"
	t.Setenv("POSSE_TEST_GH_BODY", human)
	edited := os.Getenv("POSSE_TEST_GH_EDIT_BODY")
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}

	f.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", f.headSHA)
	if err := f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(edited)
	if err != nil {
		t.Fatalf("new CI evidence did not refresh the PR body: %v", err)
	}
	want := strings.Replace(human, "- CI: pending", "- CI: success", 1)
	if want == human || string(got) != want {
		t.Fatalf("refreshed body changed more than the Validation block:\n got: %q\nwant: %q", got, want)
	}

	if err := os.Remove(edited); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GH_BODY", string(got))
	if err := f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(edited); !os.IsNotExist(err) {
		t.Fatalf("unchanged evidence rewrote the PR body: %v", err)
	}
}

func TestReviewAndShowExposeValidationRoundsAndCounts(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateWorking)
	defer f.db.Close()
	for _, args := range [][]string{{"review", "t1"}, {"show", "t1"}} {
		for _, format := range []string{"toon", "json"} {
			code, output, stderr := f.run(append(args, "--format", format)...)
			if code != 0 {
				t.Fatalf("%v %s: %d %s %s", args, format, code, output, stderr)
			}
			for _, field := range []string{"validation", "rounds_used", "remaining", "findings_total", "carries", "latest_verdict"} {
				if !strings.Contains(output, field) {
					t.Errorf("%v %s omitted %q: %s", args, format, field, output)
				}
			}
			if format == "json" {
				var decoded map[string]any
				if err := json.Unmarshal([]byte(output), &decoded); err != nil {
					t.Fatalf("%v json: %v: %s", args, err, output)
				}
				validation, ok := decoded["validation"].(map[string]any)
				if !ok || validation["rounds_used"] != float64(0) {
					t.Errorf("%v json validation = %#v", args, decoded["validation"])
				}
			}
		}
	}
}
