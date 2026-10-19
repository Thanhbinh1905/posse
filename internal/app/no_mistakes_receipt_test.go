package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func stubNoMistakesReceipt(t *testing.T, query func(commit string) (string, error)) *[]string {
	t.Helper()
	var calls []string
	previous := queryNoMistakesReceipt
	queryNoMistakesReceipt = func(_ context.Context, _ string, commit string) (string, error) {
		calls = append(calls, commit)
		return query(commit)
	}
	t.Cleanup(func() { queryNoMistakesReceipt = previous })
	return &calls
}

func noMistakesReceiptJSON(t *testing.T, head string, edit func(map[string]any)) string {
	t.Helper()
	receipt := map[string]any{
		"schema": noMistakesReceiptSchema, "repository": "github.com/acme/shop", "run_id": "01RUN", "head_sha": head, "run_status": "completed",
		"review": map[string]any{
			"stage": "review", "status": "passed", "report_sha256": strings.Repeat("ab", 32),
			"reviewer": map[string]any{"profile": "nm-reviewer", "model": "model-r"},
		},
	}
	if edit != nil {
		edit(receipt)
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func requireFailureCode(t *testing.T, err error, code string, contains ...string) {
	t.Helper()
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	text := failure.Message + " " + strings.Join(failure.Help, " ")
	for _, part := range contains {
		if !strings.Contains(text, part) {
			t.Fatalf("%s does not mention %q: %s", code, part, text)
		}
	}
}

func TestNoMistakesReceiptFailsClosedWithSpecificErrors(t *testing.T) {
	review := func(edit func(map[string]any)) func(map[string]any) {
		return func(receipt map[string]any) { edit(receipt["review"].(map[string]any)) }
	}
	for _, tc := range []struct {
		name     string
		output   func(t *testing.T, head string) (string, error)
		code     string
		contains []string
	}{
		{"not installed", func(*testing.T, string) (string, error) { return "", errNoMistakesMissing }, "no_mistakes_unavailable", nil},
		{"unsupported upstream", func(*testing.T, string) (string, error) {
			return "", fmt.Errorf("exit status 1: unknown flag: --head")
		}, "review_receipt_unsupported", []string{noMistakesReceiptSchema}},
		{"no run for head", func(*testing.T, string) (string, error) { return "", errors.New("exit status 1: no run for head") }, "review_receipt_unavailable", []string{"no run for head"}},
		{"not JSON", func(*testing.T, string) (string, error) { return "outcome: passed\n", nil }, "review_receipt_invalid", nil},
		{"wrong schema", func(t *testing.T, head string) (string, error) {
			return noMistakesReceiptJSON(t, head, func(r map[string]any) { r["schema"] = "other" }), nil
		}, "review_receipt_invalid", []string{`"other"`}},
		{"missing fields", func(t *testing.T, head string) (string, error) {
			return noMistakesReceiptJSON(t, head, func(r map[string]any) {
				delete(r, "repository")
				delete(r["review"].(map[string]any), "reviewer")
				delete(r["review"].(map[string]any), "report_sha256")
			}), nil
		}, "review_receipt_incomplete", []string{"repository", "review.reviewer.profile", "review.reviewer.model", "review.report_sha256"}},
		{"pipeline failed", func(t *testing.T, head string) (string, error) {
			return noMistakesReceiptJSON(t, head, func(r map[string]any) {
				r["run_status"] = "failed"
				delete(r["review"].(map[string]any), "reviewer")
			}), nil
		}, "review_receipt_failed", []string{`run_status="failed"`}},
		{"review stage requested changes", func(t *testing.T, head string) (string, error) {
			return noMistakesReceiptJSON(t, head, review(func(r map[string]any) { r["status"] = "changes_requested" })), nil
		}, "review_receipt_failed", []string{`status="changes_requested"`}},
		{"other stage", func(t *testing.T, head string) (string, error) {
			return noMistakesReceiptJSON(t, head, review(func(r map[string]any) { r["stage"] = "test" })), nil
		}, "review_receipt_failed", []string{`stage "test"`}},
		{"other repository", func(t *testing.T, head string) (string, error) {
			return noMistakesReceiptJSON(t, head, func(r map[string]any) { r["repository"] = "github.com/acme/fork" }), nil
		}, "review_receipt_mismatch", []string{"github.com/acme/fork", "github.com/acme/shop"}},
		{"stale head", func(t *testing.T, _ string) (string, error) {
			return noMistakesReceiptJSON(t, strings.Repeat("c", 40), func(r map[string]any) { r["run_status"] = "failed" }), nil
		}, "review_receipt_stale", []string{"cccccccccccc"}},
		{"invalid digest", func(t *testing.T, head string) (string, error) {
			return noMistakesReceiptJSON(t, head, review(func(r map[string]any) { r["report_sha256"] = "sha256:abc" })), nil
		}, "review_receipt_invalid", nil},
		{"reviewer model matches author", func(t *testing.T, head string) (string, error) {
			return noMistakesReceiptJSON(t, head, review(func(r map[string]any) { r["reviewer"] = map[string]any{"profile": "nm-reviewer", "model": "model-a"} })), nil
		}, "review_receipt_identity_ineligible", []string{"configured model matches author launch 1"}},
		{"reviewer Profile matches author", func(t *testing.T, head string) (string, error) {
			return noMistakesReceiptJSON(t, head, review(func(r map[string]any) { r["reviewer"] = map[string]any{"profile": "author", "model": "model-r"} })), nil
		}, "review_receipt_identity_ineligible", []string{"Profile matches author launch 1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewVerdictFixture(t)
			freezeReviewPolicy(t, f, "no-mistakes", taskplan.Version)
			stubNoMistakesReceipt(t, func(commit string) (string, error) { return tc.output(t, commit) })
			err := requireSelectedReviewAt(context.Background(), f.db, f.project, config.Config{}, f.task, f.headSHA)
			requireFailureCode(t, err, tc.code, tc.contains...)
			if receipts, err := f.db.ReviewReceipts(context.Background(), f.task.ID); err != nil || len(receipts) != 0 {
				t.Fatalf("refused receipt was recorded: %#v %v", receipts, err)
			}
		})
	}
}

func TestNoMistakesReceiptQualifiesExactHeadAndIsRecordedOnce(t *testing.T) {
	f := newReviewVerdictFixture(t)
	freezeReviewPolicy(t, f, "no-mistakes", taskplan.Version)
	var output string
	calls := stubNoMistakesReceipt(t, func(commit string) (string, error) {
		output = noMistakesReceiptJSON(t, commit, nil)
		return output, nil
	})
	ctx := context.Background()
	for range 2 {
		if err := requireSelectedReviewAt(ctx, f.db, f.project, config.Config{}, f.task, f.headSHA); err != nil {
			t.Fatal(err)
		}
	}
	if len(*calls) != 2 || (*calls)[0] != f.headSHA {
		t.Fatalf("receipt was not queried for the exact candidate each time: %v", *calls)
	}
	receipts, err := f.db.ReviewReceipts(ctx, f.task.ID)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt records: %#v %v", receipts, err)
	}
	sum := sha256.Sum256([]byte(output))
	got := receipts[0]
	if got.HeadSHA != f.headSHA || got.RunID != "01RUN" || got.Repository != "github.com/acme/shop" || got.ReviewerModel != "model-r" || got.ReviewerProfile != "nm-reviewer" || got.ReceiptSHA256 != hex.EncodeToString(sum[:]) || got.ReportSHA256 != strings.Repeat("ab", 32) {
		t.Fatalf("receipt record: %#v", got)
	}
	// A local pass is not a pipeline receipt.
	stubNoMistakesReceipt(t, func(string) (string, error) { return "", errors.New("exit status 1: unknown flag: --head") })
	if code, out := f.signal(t, f.review.PaneID, "done", "Checked", "--report", "report.md", "--verdict", "pass"); code != 0 {
		t.Fatalf("verdict: %d %s", code, out)
	}
	requireFailureCode(t, requireSelectedReviewAt(ctx, f.db, f.project, config.Config{}, f.task, f.headSHA), "review_receipt_unsupported")
}

func TestLocalReviewBindsToExplicitPullRequestHead(t *testing.T) {
	f := newReviewVerdictFixture(t)
	freezeReviewPolicy(t, f, "local", taskplan.Version)
	if code, out := f.signal(t, f.review.PaneID, "done", "Checked", "--report", "report.md", "--verdict", "pass"); code != 0 {
		t.Fatalf("verdict: %d %s", code, out)
	}
	ctx := context.Background()
	if err := requireSelectedReviewAt(ctx, f.db, f.project, config.Config{}, f.task, f.headSHA); err != nil {
		t.Fatalf("pass for the reviewed head refused: %v", err)
	}
	other := strings.Repeat("d", 40)
	requireFailureCode(t, requireSelectedReviewAt(ctx, f.db, f.project, config.Config{}, f.task, other), "review_required", "candidate dddddddddddd")
}

func TestMergedPullRequestWithoutEvidenceIsPolicyViolation(t *testing.T) {
	f := newReviewVerdictFixture(t)
	freezeReviewPolicy(t, f, "local", taskplan.Version)
	merged := store.PRObservation{PRURL: f.task.PRURL, HeadSHA: f.headSHA, State: "MERGED", MergeCommit: strings.Repeat("e", 40), ObservedAt: 7}
	notice, violated := reviewPolicyViolation(context.Background(), f.db, f.project, config.Config{}, f.task, merged)
	if !violated || notice.Kind != "review_policy_violation" || !strings.Contains(notice.DataJSON, `"evidence_error":"review_required"`) || !strings.Contains(notice.Summary, "without the selected review evidence") {
		t.Fatalf("external merge without a pass: %v %#v", violated, notice)
	}
	if verdicts, err := f.db.ReviewVerdictsFor(context.Background(), f.task.ID); err != nil || len(verdicts) != 0 {
		t.Fatalf("violation fabricated a verdict: %#v %v", verdicts, err)
	}
	if code, out := f.signal(t, f.review.PaneID, "done", "Checked", "--report", "report.md", "--verdict", "pass"); code != 0 {
		t.Fatalf("verdict: %d %s", code, out)
	}
	if _, violated := reviewPolicyViolation(context.Background(), f.db, f.project, config.Config{}, f.task, merged); violated {
		t.Fatal("merge of the reviewed head reported a violation")
	}
}
