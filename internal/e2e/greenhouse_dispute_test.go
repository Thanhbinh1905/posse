//go:build e2e && linux

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestGreenhouseSpentCapDisputeWithdrawsAndPublishes(t *testing.T) {
	runGreenhouseSpentCapDispute(t, false)
}

func TestGreenhouseDisputeApplyRefusesChangedHead(t *testing.T) {
	runGreenhouseSpentCapDispute(t, true)
}

func runGreenhouseSpentCapDispute(t *testing.T, moveHead bool) {
	t.Helper()
	t.Run(map[bool]string{false: "withdraw proceeds to publication", true: "apply refuses a changed head"}[moveHead], func(t *testing.T) {
		f := newWorkerTabsFixture(t)
		c := newGreenhouseContract(t, f)
		c.env = setEnv(c.env, "GREENHOUSE_HOME", c.home)
		t.Cleanup(func() { c.stopDaemon(t) })
		projectDB, err := store.OpenReadOnly(f.home)
		if err != nil {
			t.Fatal(err)
		}
		project, err := projectDB.ProjectByID(context.Background(), f.projectID)
		projectDB.Close()
		if err != nil {
			t.Fatal(err)
		}
		projectDir := filepath.Join(f.home, "projects", project.Name)
		config := "review_provider = \"greenhouse\"\n[review_loop]\nmax_rounds = 1\n[defaults]\nreview = \"full-pipeline\"\nmerge_mode = \"local\"\n"
		if err := os.WriteFile(filepath.Join(projectDir, "config.toml"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		c.change = f.start(t, "t1", "Greenhouse dispute change", "greenhouse-dispute")
		head := c.authorCommit(t)
		c.setMode(t, "findings")
		workerEnv := setEnv(c.env, "POSSE_TASK_ID", c.change.ContainmentMarker)
		workerEnv = setEnv(workerEnv, "HERDR_PANE_ID", c.change.PaneID)
		if output, err := c.posse(workerEnv, c.change.WorktreePath, "signal", "done", "Greenhouse dispute change", "--head", head); err != nil {
			t.Fatalf("Worker done Signal: %v\n%s", err, output)
		}
		if output, err := c.posse(c.env, f.repo, "watch", "--poll-only", "--timeout", "3000"); err != nil {
			t.Fatalf("Lead Watch: %v\n%s", err, output)
		}
		if !waitForCondition(60*time.Second, func() bool { return len(c.verdicts(t)) > 0 }) {
			t.Fatal("Watch did not record Greenhouse findings")
		}
		db, err := store.Open(f.home)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		change, err := db.TaskByID(context.Background(), c.change.ProjectID, c.change.ID)
		if err != nil {
			t.Fatal(err)
		}
		usage, err := db.ReviewCapUsage(context.Background(), change.ID)
		plan, err := change.AdmittedPlan()
		if err != nil || usage.ReviewRounds != 1 || plan == nil || plan.ValidationLoops.MaxReviewRounds() != 1 {
			t.Fatalf("Greenhouse review cap usage = %+v, max rounds = %v, plan error = %v; want one of one rounds used", usage, plan, err)
		}
		cap, err := db.RaiseDecision(context.Background(), store.DecisionRequest{ProjectID: change.ProjectID, TaskID: change.ID, Kind: "review_cap", Origin: "review_cap:" + fmt.Sprint("t", change.Seq) + ":" + head, Question: "Review cap reached at this head?", Options: []string{"accept-head", "rethink", "discard"}})
		if err != nil {
			t.Fatal(err)
		}
		evidencePath := filepath.Join(f.root, "dispute-evidence.md")
		evidence := fmt.Sprintf("Test: go test ./internal/app -run TestFalseGreenhouseFinding\nHead: %s\nResult: the reported behavior is rejected by validation at the called boundary.\n", head)
		if err := os.WriteFile(evidencePath, []byte(evidence), 0o600); err != nil {
			t.Fatal(err)
		}
		if output, err := c.posse(c.env, f.repo, "review", "dispute", "t1", "F-1", "--evidence", evidencePath); err != nil {
			t.Fatalf("create dispute Decision: %v\n%s", err, output)
		}
		decisions, err := db.Decisions(context.Background(), change.ProjectID, false)
		if err != nil {
			t.Fatal(err)
		}
		var dispute store.Decision
		for _, decision := range decisions {
			if decision.Kind == "review_dispute" {
				dispute = decision
			}
		}
		if dispute.ID == 0 || string(dispute.Evidence) != evidence || dispute.EvidenceDigest == "" {
			t.Fatalf("dispute Decision did not retain evidence: %+v", dispute)
		}
		if output, err := c.posse(c.env, f.repo, "decisions"); err != nil || !strings.Contains(output, "Test: go test") || !strings.Contains(output, dispute.EvidenceDigest) {
			t.Fatalf("posse decisions omitted stored evidence: %v\n%s", err, output)
		}
		if output, err := c.posse(c.env, f.repo, "show", "t1", "--full"); err != nil || !strings.Contains(output, "Test: go test") || !strings.Contains(output, dispute.EvidenceDigest) {
			t.Fatalf("posse show omitted stored evidence: %v\n%s", err, output)
		}
		if output, err := c.posse(c.env, f.repo, "decide", strconv.FormatInt(dispute.ID, 10), "withdraw", "--user-approved", "I reviewed the pinned evidence and withdraw this finding."); err != nil {
			t.Fatalf("User withdraw: %v\n%s", err, output)
		}
		if moveHead {
			moved := c.authorCommit(t)
			if output, err := c.posse(c.env, f.repo, "apply", strconv.FormatInt(dispute.ID, 10)); err == nil || !strings.Contains(output, "head changed") {
				t.Fatalf("apply after head moved (%s) = %v\n%s", moved, err, output)
			}
			return
		}
		if output, err := c.posse(c.env, f.repo, "apply", strconv.FormatInt(dispute.ID, 10)); err != nil {
			t.Fatalf("apply User withdraw: %v\n%s", err, output)
		}
		passed, err := db.GreenhouseGatePass(context.Background(), change.ID, project.Name, head)
		if err != nil || !passed {
			t.Fatalf("withdrawn finding did not qualify exact head: passed=%v err=%v", passed, err)
		}
		cap, err = db.Decision(context.Background(), change.ProjectID, cap.ID)
		if err != nil || cap.ObsoleteAt == 0 || !strings.Contains(cap.ObsoleteReason, fmt.Sprint(dispute.ID)) {
			t.Fatalf("open cap Decision was not superseded by dispute: %+v, %v", cap, err)
		}
		var queued int
		if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE task_id=? AND body LIKE '%Publish this exact head%' AND body LIKE '%Fixes #1169%' AND body LIKE '%Fixes #1135%'`, change.ID).Scan(&queued); err != nil || queued == 0 {
			t.Fatalf("publication instruction count = %d, %v", queued, err)
		}
		views, err := db.ProviderFindingsForTask(context.Background(), change.ID)
		if err != nil || len(views) != 1 || views[0].State != "withdrawn" {
			t.Fatalf("withdrawn finding not visible in review state: %+v, %v", views, err)
		}
	})
}
