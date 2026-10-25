//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

// ciRepairWorkerScript is a fake Change Worker that answers the first queued
// CI fix instruction with one repair commit, then signals done only after the
// test creates ci-worker-allow-done. Review Tasks idle on their launch prompt.
const ciRepairWorkerScript = `#!/bin/sh
set -eu
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
IFS= read -r prompt || exit 0
launch=${prompt#Read }
launch=${launch% and follow it.}
case "$launch" in */tasks/t1/launch.md)
  herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
  : > "$POSSE_TEST_ROOT/ci-worker-ready"
  herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
  turn_token=
  while IFS= read -r line; do
    printf '%s\n' "$line" >> "$POSSE_TEST_ROOT/ci-worker-input.log"
    case "$line" in *'| turn '*) turn_token=$(printf '%s\n' "$line" | sed -n 's/.*| turn \([^]]*\)].*/\1/p') ;; esac
    case "$line" in *'Required CI checks failed'*)
      printf 'repaired candidate content\n' > change.txt
      git add change.txt
      git commit -m 'repair failing CI check'
      : > "$POSSE_TEST_ROOT/ci-worker-fixed"
      herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
      while [ ! -e "$POSSE_TEST_ROOT/ci-worker-allow-done" ]; do sleep 0.02; done
      if "$POSSE_E2E_POSSE_BIN" signal done 'Repaired CI candidate validated' --pr 'https://github.com/acme/shop/pull/17' --turn "$turn_token" >> "$POSSE_TEST_ROOT/ci-worker-signal.log" 2>&1; then
        printf 'signal-ok\n' >> "$POSSE_TEST_ROOT/ci-worker-signal.log"
      else
        printf 'signal-failed\n' >> "$POSSE_TEST_ROOT/ci-worker-signal.log"
      fi
      herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
      ;;
    esac
  done
  ;;
*)
  herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
  printf '%s\t%s\n' "$HERDR_PANE_ID" "$PWD" >> "$POSSE_TEST_ROOT/review-identity-ready.log"
  : > "$POSSE_TEST_ROOT/review-working-$HERDR_PANE_ID"
  while IFS= read -r line; do herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1; done
  ;;
esac`

// ciRepairChange drives one full-pipeline PR Change Task through private
// candidates, Review Rounds and forge CI observations.
type ciRepairChange struct {
	f         *prLifecycleFixture
	change    store.Task
	workerEnv []string
	// originHead is the published head the bare origin must hold; empty means
	// the Task branch must still be absent.
	originHead string
	// auditOrigin, when set, also runs on every origin head check.
	auditOrigin func(t *testing.T)
}

func startCIRepairChange(t *testing.T, f *prLifecycleFixture, settings ...[2]string) *ciRepairChange {
	t.Helper()
	return startCIRepairChangeWith(t, f, func() (int, string) {
		return startReviewIdentityTask(t, f, "CI repair", "author", "")
	}, settings...)
}

// startCIRepairChangeWith installs the CI repair Worker, applies settings and
// starts Change Task t1 with start.
func startCIRepairChangeWith(t *testing.T, f *prLifecycleFixture, start func() (int, string), settings ...[2]string) *ciRepairChange {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, "bin", "claude"), []byte(ciRepairWorkerScript), 0o700); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.merge_mode", "pr")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", "full-pipeline")
	for _, setting := range settings {
		runPosse(t, f.binary, f.repo, f.env, "config", "set", setting[0], setting[1])
	}
	if code, output := start(); code != 0 {
		t.Fatalf("start Change Task: %d %s", code, output)
	}
	change := f.mustTask(t, "t1")
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "change.txt"), []byte("initial candidate content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, change.WorktreePath, "add", "change.txt")
	gitTest(t, f.env, change.WorktreePath, "commit", "-m", "update candidate content")
	return &ciRepairChange{f: f, change: change, workerEnv: workerEnv}
}

// submitCandidate hands the author head to a private candidate without
// publishing it.
func (h *ciRepairChange) submitCandidate(t *testing.T, summary string) store.ValidationCandidate {
	t.Helper()
	code, output := runReleaseCLI(t, h.f.binary, h.change.WorktreePath, h.workerEnv, "publish", summary, "--verify", "go test ./... -> passed", "--proof", "Intent/E2E behavior verified", "--risk", "Risk: low\nRollback: revert PR")
	if code != 0 || !strings.Contains(output, "validation required before publication") {
		t.Fatalf("submit private candidate: %d %s", code, output)
	}
	candidate, err := h.f.db.LatestValidationCandidate(context.Background(), h.change.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	h.assertOriginHead(t, h.originHead)
	return candidate
}

func (h *ciRepairChange) assertOriginHead(t *testing.T, expected string) {
	t.Helper()
	if h.auditOrigin != nil {
		h.auditOrigin(t)
	}
	if expected == "" {
		assertTaskBranchAbsentOnBareOrigin(t, h.f, h.change.Branch)
		return
	}
	got := strings.TrimSpace(gitTest(t, h.f.env, h.f.remote, "show-ref", "--hash", "refs/heads/"+h.change.Branch))
	if got != expected {
		t.Fatalf("bare origin head is %s; want %s", got, expected)
	}
}

// validateCandidate records a passing Review Round and the test, docs and lint
// stages for candidate, checking after each stage that nothing was published.
func (h *ciRepairChange) validateCandidate(t *testing.T, candidate store.ValidationCandidate, round string) {
	t.Helper()
	f := h.f
	if code, output := runReviewIdentityCLI(t, f, "review", "start", "t1", "--profile", "reviewer"); code != 0 {
		t.Fatalf("start candidate Review: %d %s", code, output)
	}
	review := f.mustTask(t, round)
	waitForReviewIdentityAgent(t, f, review.PaneID)
	report := filepath.Join(f.home, "scratch", "shop", round, "report.md")
	findings := filepath.Join(f.home, "scratch", "shop", round, "findings.json")
	if err := os.WriteFile(report, []byte("The private candidate passed review.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(findings, []byte(`{"schema":"posse.review-findings.v1","patterns":[],"resolutions":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--findings", findings, "--verdict", "pass")
	for _, args := range [][]string{
		{"validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent/E2E behavior verified"},
		{"validate", "docs", "--no-comments", "passed", "--no-owner-doc-change", "No owner documentation changes are warranted", "--proof", "No unnecessary comments; complete PR description prepared"},
		{"validate", "lint", "--proof", "Configured Gates passed", "--coverage", "No local workflow jobs are present"},
	} {
		if code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, h.workerEnv, args...); code != 0 {
			t.Fatalf("candidate stage %v failed: %d %s", args, code, output)
		}
		h.assertOriginHead(t, h.originHead)
	}
}

// publishThenFailCI validates and publishes the first candidate with Review
// Round t2, opens its PR, reports a failed required check on that head and
// waits until the fake Worker has committed the queued CI repair. It returns
// the published head.
func (h *ciRepairChange) publishThenFailCI(t *testing.T) string {
	t.Helper()
	f := h.f
	candidate := h.submitCandidate(t, "CI repair candidate")
	h.validateCandidate(t, candidate, "t2")
	if code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, h.workerEnv, "publish", "CI repair candidate"); code != 0 || !strings.Contains(output, "promoted: candidate") {
		t.Fatalf("promote initial candidate: %d %s", code, output)
	}
	h.originHead = candidate.CandidateSHA
	turn, err := f.db.TaskTurnToken(context.Background(), h.change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, h.change.WorktreePath, h.workerEnv, "signal", "done", "Initial candidate validated", "--pr", "https://github.com/acme/shop/pull/17", "--turn", turn); code != 0 {
		t.Fatalf("complete initial Task: %d %s", code, output)
	}
	if output := runPosse(t, f.binary, f.repo, f.leadEnv, "merge", "t1"); !strings.Contains(output, "merging") {
		t.Fatalf("open initial PR: %s", output)
	}
	h.change = f.mustTask(t, "t1")
	head := h.change.GatedSHA
	if head == "" {
		t.Fatal("initial publication did not bind a gated PR head")
	}
	f.writeGraphQL(t, "pr1", "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", head)
	runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t1")
	if !waitForCondition(20*time.Second, func() bool {
		observation, err := f.db.LatestPRObservation(context.Background(), h.change.ID)
		if err != nil || observation.HeadSHA != head || !strings.Contains(observation.Checks, "FAILURE") {
			return false
		}
		return h.hasNotice("ci_fix_queued")
	}) {
		observation, observationErr := f.db.LatestPRObservation(context.Background(), h.change.ID)
		notices, noticesErr := f.db.Notices(context.Background(), f.project.ID, false)
		t.Fatalf("failed CI observation did not queue a repair: observation=%#v err=%v notices=%#v err=%v", observation, observationErr, notices, noticesErr)
	}
	if !waitForCondition(15*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(f.root, "ci-worker-fixed"))
		return err == nil
	}) {
		input, _ := os.ReadFile(filepath.Join(f.root, "ci-worker-input.log"))
		t.Fatalf("fake Worker did not consume the queued CI instruction: %s", input)
	}
	return head
}

func (h *ciRepairChange) hasNotice(kind string) bool {
	notices, err := h.f.db.Notices(context.Background(), h.f.project.ID, false)
	if err != nil {
		return false
	}
	for _, notice := range notices {
		if notice.TaskID == h.change.ID && notice.Kind == kind {
			return true
		}
	}
	return false
}

func TestFullPipelineCIRepairSharesReviewCapAndBlocksRepublishAtExhaustion(t *testing.T) {
	f := reviewIdentityFixture(t)
	// A two-round cap is spent by Review Round 1 and one CI repair, while the
	// CI loop alone would still allow two more repairs.
	h := startCIRepairChange(t, f, [2]string{"ci_loop.max_fix_rounds", "3"}, [2]string{"review_loop.max_rounds", "2"})
	firstHead := h.publishThenFailCI(t)
	usage, err := f.db.ReviewCapUsage(context.Background(), h.change.ID)
	if err != nil || usage.ReviewRounds != 1 || usage.RepairRounds != 1 {
		t.Fatalf("shared cap usage=%+v err=%v, want one Review Round and one CI repair", usage, err)
	}
	candidate := h.submitCandidate(t, "Repaired CI candidate")
	if candidate.CandidateSHA == firstHead {
		t.Fatal("CI repair did not produce a fresh private candidate")
	}
	code, output := runReviewIdentityCLI(t, f, "review", "start", "t1", "--profile", "reviewer")
	if code == 0 || !strings.Contains(output, "review_round_cap") {
		t.Fatalf("Review Round admitted after the shared cap was spent: %d %s", code, output)
	}
	code, output = runReleaseCLI(t, f.binary, candidate.WorktreePath, h.workerEnv, "publish", "Repaired CI candidate")
	if code == 0 || !strings.Contains(output, "review_cap_reached") || strings.Contains(output, "posse review start") {
		t.Fatalf("republish after cap exhaustion was not refused with the cap Decision: %d %s", code, output)
	}
	h.assertOriginHead(t, firstHead)
	decisions := openReviewCapDecisions(t, f, h.change)
	if len(decisions) != 1 {
		t.Fatalf("cap exhaustion raised %d Review cap Decisions, want one", len(decisions))
	}
	var origin string
	if err := f.db.QueryRowContext(context.Background(), `SELECT origin FROM decisions WHERE id=?`, decisions[0]).Scan(&origin); err != nil || !strings.HasSuffix(origin, candidate.CandidateSHA) {
		t.Fatalf("cap Decision origin=%q err=%v, want the repaired candidate head %s", origin, err, candidate.CandidateSHA)
	}
	for _, option := range []string{"accept-head", "final-fix", "rethink", "discard"} {
		if code, output := runReviewIdentityCLI(t, f, "decide", fmt.Sprint(decisions[0]), option); code != 1 || !strings.Contains(output, "user_only") {
			t.Fatalf("Lead answered the cap Decision with %s: %d %s", option, code, output)
		}
	}
	h.assertOriginHead(t, firstHead)
}
