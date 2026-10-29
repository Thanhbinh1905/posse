//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// workspaceCapChange is a full-pipeline workspace Change Task t1 over the api
// and web Members that has spent the default cap of three rounds.
type workspaceCapChange struct {
	f         *prLifecycleFixture
	ws        fullPipelineWorkspace
	change    store.Task
	workerEnv []string
}

func (h *workspaceCapChange) lead(t *testing.T, args ...string) (int, string) {
	t.Helper()
	return runReleaseCLI(t, h.f.binary, h.ws.root, h.ws.leadEnv, args...)
}

func (h *workspaceCapChange) candidates(t *testing.T) map[string]store.ValidationCandidate {
	t.Helper()
	candidates := map[string]store.ValidationCandidate{}
	for _, member := range []string{"api", "web"} {
		candidate, err := h.f.db.LatestValidationCandidate(context.Background(), h.change.ID, member)
		if err != nil {
			t.Fatal(err)
		}
		candidates[member] = candidate
	}
	return candidates
}

// recordRound starts the next Review Round of t1 and completes it with
// verdict, findings patterns and resolutions.
func (h *workspaceCapChange) recordRound(t *testing.T, round, verdict, patterns, resolutions string) {
	t.Helper()
	if code, output := h.lead(t, "review", "start", "t1", "--profile", "reviewer"); code != 0 {
		t.Fatalf("start Review Round %s: code=%d %s", round, code, output)
	}
	review := mustProjectTask(t, h.f, h.ws.project.ID, round)
	waitForReviewIdentityAgent(t, h.f, review.PaneID)
	scratch := filepath.Join(h.f.home, "scratch", "stack", round)
	report, findings := filepath.Join(scratch, "report.md"), filepath.Join(scratch, "findings.json")
	if err := os.WriteFile(report, []byte("Review Round "+round+" recorded "+verdict+".\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := `{"schema":"posse.review-findings.v1","patterns":[` + patterns + `],"resolutions":[` + resolutions + `]}`
	if err := os.WriteFile(findings, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, h.f, review, "--report", report, "--findings", findings, "--verdict", verdict)
}

// applyFix delivers the queued whole-pattern fix, commits a fix in each named
// Member's private candidate and answers every pattern as fixed.
func (h *workspaceCapChange) applyFix(t *testing.T, name string, members []string, patterns ...string) {
	t.Helper()
	if _, err := herdr.NewWithEnv("herdr", h.f.env).Run(context.Background(), "pane", "report-agent", h.change.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	if code, output := h.lead(t, "show", "t1"); code != 0 {
		t.Fatalf("deliver queued fix instruction: code=%d %s", code, output)
	}
	candidates := h.candidates(t)
	for _, member := range members {
		path := candidates[member].WorktreePath
		if err := os.WriteFile(filepath.Join(path, "change.txt"), []byte(name+" "+member+" operation\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, h.f.env, path, "add", "change.txt")
		gitTest(t, h.f.env, path, "commit", "-m", name+" "+member)
	}
	var answers []string
	for _, pattern := range patterns {
		answers = append(answers, `{"pattern":"`+pattern+`","status":"fixed"}`)
	}
	resolutions := filepath.Join(h.f.home, "scratch", "stack", "t1", name+"-resolutions.json")
	if err := os.WriteFile(resolutions, []byte(`{"schema":"posse.review-resolutions.v1","patterns":[`+strings.Join(answers, ",")+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	turn, err := h.f.db.TaskTurnToken(context.Background(), h.change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, h.f.binary, h.change.WorktreePath, h.workerEnv, "signal", "done", "Applied "+name, "--turn", turn, "--resolutions", resolutions); code != 0 || strings.Contains(output, "reconciliation deferred") {
		t.Fatalf("record %s resolutions: code=%d %s", name, code, output)
	}
}

// openCapDecision reconciles Decisions and returns the one open review_cap
// Decision of t1 with its origin.
func (h *workspaceCapChange) openCapDecision(t *testing.T) (int64, string) {
	t.Helper()
	if code, output := h.lead(t, "decisions"); code != 0 {
		t.Fatalf("reconcile Decisions: code=%d %s", code, output)
	}
	var id int64
	var origin string
	if err := h.f.db.QueryRowContext(context.Background(), `SELECT id,origin FROM decisions WHERE task_id=? AND kind='review_cap' AND answered_at=0 AND obsolete_at=0`, h.change.ID).Scan(&id, &origin); err != nil {
		t.Fatalf("open review_cap Decision: %v", err)
	}
	return id, origin
}

func (h *workspaceCapChange) answer(t *testing.T, decisionID int64, option, quote, want string) string {
	t.Helper()
	if code, output := h.lead(t, "decide", fmt.Sprint(decisionID), option, "--user-approved", quote); code != 0 {
		t.Fatalf("answer %s: code=%d %s", option, code, output)
	}
	code, output := h.lead(t, "apply", fmt.Sprint(decisionID))
	if code != 0 || !strings.Contains(output, want) {
		t.Fatalf("apply %s: code=%d %s", option, code, output)
	}
	return output
}

func unsafeMemberPattern(key, member, excerpt string) string {
	return `{"key":"` + key + `","title":"Unsafe ` + member + ` operation","rule":"Operations must be validated","root_cause":"The value is used without validation","fix_guidance":"Validate every operation","severity":"P1","origin":"introduced","instances":[{"repository":"` + member + `","path":"change.txt","line":1,"symbol":"operation","excerpt":"` + excerpt + `","description":"Unvalidated operation"}]}`
}

// startWorkspaceCapChange replays the clouddbv1 t27 sequence: Review Round 1
// requests changes in both Members, repair round 1 fixes them in the private
// candidates, and Review Round 2 requests changes for a new pattern P-3, which
// spends 3 of the default 3 shared Review/repair rounds.
func startWorkspaceCapChange(t *testing.T) *workspaceCapChange {
	t.Helper()
	f := reviewIdentityFixture(t)
	ws := newFullPipelineWorkspace(t, f)
	brief := filepath.Join(f.root, "workspace-cap.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Workspace cap\ndone_when: checked\nrepos: [api, web]\n---\nChange both Members safely.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, ws.root, ws.leadEnv, "start", "--brief", brief, "--name", "workspace-cap", "--profile", "author"); code != 0 {
		t.Fatalf("start workspace Change Task: code=%d %s", code, output)
	}
	change := mustProjectTask(t, f, ws.project.ID, "t1")
	h := &workspaceCapChange{f: f, ws: ws, change: change}
	h.workerEnv = setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	h.workerEnv = setEnv(h.workerEnv, "POSSE_WORKER_HOME", f.home)
	repos, err := f.db.TaskRepos(context.Background(), change.ID)
	if err != nil || len(repos) != 2 {
		t.Fatalf("workspace Members: %+v %v", repos, err)
	}
	for _, repo := range repos {
		if err := os.WriteFile(filepath.Join(repo.WorktreePath, "change.txt"), []byte("unsafe "+repo.Repo+" operation\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, f.env, repo.WorktreePath, "add", "change.txt")
		gitTest(t, f.env, repo.WorktreePath, "commit", "-m", "change "+repo.Repo)
		if code, output := runReleaseCLI(t, f.binary, repo.WorktreePath, h.workerEnv,
			"publish", "--repo", repo.Repo, repo.Repo+" candidate summary", "--verify", "go test ./... -> passed",
			"--proof", "Intent behavior verified", "--risk", "Risk: low\nRollback: revert the "+repo.Repo+" change"); code != 0 {
			t.Fatalf("submit %s private candidate: code=%d %s", repo.Repo, code, output)
		}
	}
	if code, output := h.lead(t, "review", "t1"); code != 0 || !strings.Contains(output, "max_rounds: 3") || !strings.Contains(output, "review and repair rounds") {
		t.Fatalf("review status does not explain the shared cap before round 1: code=%d %s", code, output)
	}
	h.recordRound(t, "t2", "changes_requested", unsafeMemberPattern("unsafe-api", "api", "unsafe api operation")+","+unsafeMemberPattern("unsafe-web", "web", "unsafe web operation"), "")
	if code, output := h.lead(t, "review", "fix", "t1"); code != 0 || !strings.Contains(output, "queued") {
		t.Fatalf("queue repair round 1: code=%d %s", code, output)
	}
	h.applyFix(t, "repair", []string{"api", "web"}, "P-1", "P-2")
	h.recordRound(t, "t3", "changes_requested", unsafeMemberPattern("api-retry", "api", "repair api operation"),
		`{"finding":"F-1","status":"resolved","note":"api validates"},{"finding":"F-2","status":"resolved","note":"web validates"}`)
	return h
}

// TestWorkspaceReviewCapRaisesDecisionBoundToEveryMemberCandidate reproduces
// the clouddbv1 t27 report: at the shared cap a workspace Task gets the same
// User Decision as a single repository instead of review_cap_unsupported, and
// accept-head is exact-head evidence for every changed Member's candidate.
func TestWorkspaceReviewCapRaisesDecisionBoundToEveryMemberCandidate(t *testing.T) {
	h := startWorkspaceCapChange(t)
	code, output := h.lead(t, "review", "dispose", "t1", "P-3", "fix", "--user-approved", "Fix P-3.")
	if code == 0 || !strings.Contains(output, "review_cap_reached") || strings.Contains(output, "review_cap_unsupported") {
		t.Fatalf("dispose fix at the cap: code=%d %s", code, output)
	}
	candidates := h.candidates(t)
	for _, member := range []string{"api", "web"} {
		if !strings.Contains(output, member+"@"+candidates[member].CandidateSHA[:7]) {
			t.Fatalf("cap refusal does not name %s candidate %s: %s", member, candidates[member].CandidateSHA, output)
		}
	}
	decisionID, _ := h.openCapDecision(t)
	code, output = h.lead(t, "decisions")
	if code != 0 || !strings.Contains(output, "accept-head") || !strings.Contains(output, "final-fix") || !strings.Contains(output, "rethink") || !strings.Contains(output, "discard") || !strings.Contains(output, "api@"+candidates["api"].CandidateSHA[:7]) {
		t.Fatalf("workspace cap Decision options or binding: code=%d %s", code, output)
	}
	const quote = "Accept both Member candidates at the cap."
	h.answer(t, decisionID, "accept-head", quote, "accepted-head")
	rows, err := h.f.db.QueryContext(context.Background(), `SELECT head_sha FROM review_dispositions WHERE ship_task_id=? AND user_quote=?`, h.change.ID, quote)
	if err != nil {
		t.Fatal(err)
	}
	var heads []string
	for rows.Next() {
		var head string
		if err := rows.Scan(&head); err != nil {
			t.Fatal(err)
		}
		heads = append(heads, head)
	}
	rows.Close()
	slices.Sort(heads)
	want := []string{candidates["api"].CandidateSHA, candidates["web"].CandidateSHA}
	slices.Sort(want)
	if !slices.Equal(heads, want) {
		t.Fatalf("accept-head dispositions=%v, want every Member candidate %v", heads, want)
	}
	if code, output := runReleaseCLI(t, h.f.binary, h.change.WorktreePath, h.workerEnv, "signal", "working", "Validate the accepted candidates"); code != 0 {
		t.Fatalf("reopen for local stages: code=%d %s", code, output)
	}
	for _, member := range []string{"web", "api"} {
		candidate := candidates[member]
		recordCandidateStages(t, h.f, h.workerEnv, candidate, func(string) {}, "--repo", member)
		if code, output := runReleaseCLI(t, h.f.binary, candidate.WorktreePath, h.workerEnv, "publish", "--repo", member, "Promote the accepted candidate"); code != 0 {
			t.Fatalf("promote accepted %s candidate: code=%d %s", member, code, output)
		}
	}
	if published := gitRef(t, h.f, h.f.remote, "refs/heads/"+h.change.Branch); published != candidates["api"].CandidateSHA {
		t.Fatalf("api origin Task branch=%s, want accepted candidate %s", published, candidates["api"].CandidateSHA)
	}
}

// TestWorkspaceReviewCapFinalFixRethinkAndDiscardApply covers the other cap
// options on a workspace Task.
func TestWorkspaceReviewCapFinalFixRethinkAndDiscardApply(t *testing.T) {
	t.Run("final-fix", func(t *testing.T) {
		h := startWorkspaceCapChange(t)
		decisionID, origin := h.openCapDecision(t)
		h.answer(t, decisionID, "final-fix", "Allow one final fix of P-3.", "final-fix-queued")
		before := h.candidates(t)
		h.applyFix(t, "final-fix", []string{"api"}, "P-3")
		after := h.candidates(t)
		if after["api"].CandidateSHA == before["api"].CandidateSHA || after["web"].CandidateSHA != before["web"].CandidateSHA {
			t.Fatalf("final fix candidates before=%v after=%v", before, after)
		}
		nextID, nextOrigin := h.openCapDecision(t)
		if nextID == decisionID || nextOrigin == origin {
			t.Fatalf("final fix did not raise a new cap Decision for its resulting heads: id=%d origin=%s", nextID, nextOrigin)
		}
	})
	t.Run("rethink", func(t *testing.T) {
		h := startWorkspaceCapChange(t)
		decisionID, _ := h.openCapDecision(t)
		h.answer(t, decisionID, "rethink", "Rethink the approach across both Members.", "supersedes: t1")
		brief := filepath.Join(h.f.root, "workspace-rethink.md")
		if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Workspace rethink\ndone_when: revised approach is checked\nrepos: [api, web]\nsupersedes: t1\n---\nRework the approach.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, output := h.lead(t, "start", "--brief", brief, "--name", "workspace-rethink", "--profile", "author"); code != 0 {
			t.Fatalf("start superseding workspace Change Task: code=%d %s", code, output)
		}
		if code, output := h.lead(t, "review", "t4", "--full"); code != 0 || !strings.Contains(output, "inherited") {
			t.Fatalf("successor does not inherit the Finding Ledger: code=%d %s", code, output)
		}
		if task := mustProjectTask(t, h.f, h.ws.project.ID, "t1"); task.State == store.StateTornDown {
			t.Fatal("rethink tore down the capped Task")
		}
	})
	t.Run("discard", func(t *testing.T) {
		h := startWorkspaceCapChange(t)
		decisionID, _ := h.openCapDecision(t)
		h.answer(t, decisionID, "discard", "Discard the capped workspace Task.", "torn-down")
		if task := mustProjectTask(t, h.f, h.ws.project.ID, "t1"); task.State != store.StateTornDown {
			t.Fatalf("discarded Task state=%s", task.State)
		}
	})
}
