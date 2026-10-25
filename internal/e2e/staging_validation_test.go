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

// The tests in this file prove the built-in validation loop against a frozen
// `staging` target. Origin staging is one commit ahead of main, so a candidate
// bound to the default branch instead of the Task's target is detectable, and
// every step audits the full ref set of each bare origin.

// stagingTarget creates origin staging one commit ahead of main and makes the
// fake forge report staging as every PR base. It returns the staging head.
func stagingTarget(t *testing.T, f *prLifecycleFixture) string {
	t.Helper()
	main := gitRef(t, f, f.remote, "refs/heads/main")
	staging := advanceBranch(t, f, f.remote, "staging", main, "staging-only commit")
	if err := os.WriteFile(filepath.Join(f.root, "gh-base-branch"), []byte("staging\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return staging
}

// advanceBranch commits on top of parent directly in gitDir and points branch
// at the commit, as an outside push to a bare origin would. An empty parent
// uses the current branch head.
func advanceBranch(t *testing.T, f *prLifecycleFixture, gitDir, branch, parent, message string) string {
	t.Helper()
	if parent == "" {
		parent = gitRef(t, f, gitDir, "refs/heads/"+branch)
	}
	tree := gitRef(t, f, gitDir, parent+"^{tree}")
	head := strings.TrimSpace(gitTest(t, f.env, gitDir, "commit-tree", tree, "-p", parent, "-m", message))
	gitTest(t, f.env, gitDir, "update-ref", "refs/heads/"+branch, head)
	return head
}

// originRefs lists every ref of the bare repository at gitDir.
func originRefs(t *testing.T, f *prLifecycleFixture, gitDir string) map[string]string {
	t.Helper()
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(gitTest(t, f.env, gitDir, "for-each-ref", "--format=%(refname) %(objectname)")), "\n") {
		if name, sha, found := strings.Cut(line, " "); found {
			refs[name] = sha
		}
	}
	return refs
}

// assertOriginRefs requires the bare repository at gitDir to hold exactly want.
func assertOriginRefs(t *testing.T, f *prLifecycleFixture, gitDir string, want map[string]string, stage string) {
	t.Helper()
	got := originRefs(t, f, gitDir)
	for name, sha := range want {
		if got[name] != sha {
			t.Fatalf("%s: origin %s=%q, want %s (refs %v)", stage, name, got[name], sha, got)
		}
	}
	for name, sha := range got {
		if _, expected := want[name]; !expected {
			t.Fatalf("%s: unexpected origin ref %s=%s was published", stage, name, sha)
		}
	}
}

func withRef(refs map[string]string, name, sha string) map[string]string {
	copied := map[string]string{}
	for key, value := range refs {
		copied[key] = value
	}
	if sha != "" {
		copied[name] = sha
	}
	return copied
}

// startStagingTask starts Change Task title with staging as its base and
// frozen target.
func startStagingTask(t *testing.T, f *prLifecycleFixture, title string) (int, string) {
	t.Helper()
	name := strings.ReplaceAll(strings.ToLower(title), " ", "-")
	brief := filepath.Join(f.root, name+".md")
	text := "---\ntype: change\ntitle: " + title + "\ndone_when: checked\nbase: staging\ntarget: staging\n---\nChange the staging candidate.\n"
	if err := os.WriteFile(brief, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return runReviewIdentityCLI(t, f, "start", "--brief", brief, "--name", name, "--profile", "author")
}

func commitCandidateFile(t *testing.T, f *prLifecycleFixture, worktree, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(worktree, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, worktree, "add", name)
	gitTest(t, f.env, worktree, "commit", "-m", "update "+name)
	return gitRef(t, f, worktree, "HEAD")
}

// submitStagingCandidate submits the author head as a private candidate and
// requires it to bind the expected staging head.
func submitStagingCandidate(t *testing.T, f *prLifecycleFixture, change store.Task, workerEnv []string, repo, target string) store.ValidationCandidate {
	t.Helper()
	dir, args := change.WorktreePath, []string{"publish"}
	if repo != "" {
		repos, err := f.db.TaskRepos(context.Background(), change.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range repos {
			if member.Repo == repo {
				dir = member.WorktreePath
			}
		}
		args = append(args, "--repo", repo)
	}
	args = append(args, "Staging candidate", "--verify", "go test ./... -> passed", "--proof", "Intent/E2E behavior verified", "--risk", "Risk: low\nRollback: revert")
	if code, output := runReleaseCLI(t, f.binary, dir, workerEnv, args...); code != 0 || !strings.Contains(output, "validation required before publication") {
		t.Fatalf("submit staging candidate: code=%d %s", code, output)
	}
	candidate, err := f.db.LatestValidationCandidate(context.Background(), change.ID, repo)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.State != store.ValidationCandidateActive || candidate.TargetSHA != target {
		t.Fatalf("candidate %+v did not freeze the staging target %s", candidate, target)
	}
	return candidate
}

func ghCreateCalls(t *testing.T, f *prLifecycleFixture) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, "gh.log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var calls []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "pr create ") {
			calls = append(calls, line)
		}
	}
	return calls
}

func requireOneStagingPR(t *testing.T, f *prLifecycleFixture) {
	t.Helper()
	calls := ghCreateCalls(t, f)
	if len(calls) != 1 || !strings.Contains(calls[0], "--base staging") {
		t.Fatalf("PR creations=%q, want exactly one with --base staging", calls)
	}
}

// TestStagingSelectedReviewModesPublishOnlyToStaging proves none and quick PR
// delivery on a staging target: publication pushes only the Task branch, the
// PR targets staging, quick still requires an exact-head pass and Merge
// readiness never moves staging or main.
func TestStagingSelectedReviewModesPublishOnlyToStaging(t *testing.T) {
	for _, mode := range []string{"none", "quick"} {
		t.Run(mode, func(t *testing.T) {
			f := reviewIdentityFixture(t)
			stopBackgroundWatch(t, f)
			stagingTarget(t, f)
			runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.merge_mode", "pr")
			runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", mode)
			if code, output := startStagingTask(t, f, "Staging "+mode); code != 0 {
				t.Fatalf("start staging Change Task: %d %s", code, output)
			}
			change := f.mustTask(t, "t1")
			if change.TargetBranch != "staging" || change.BaseRef != gitRef(t, f, f.remote, "refs/heads/staging") {
				t.Fatalf("Task froze target=%q base=%s, want staging", change.TargetBranch, change.BaseRef)
			}
			refs := originRefs(t, f, f.remote)
			head := commitCandidateFile(t, f, change.WorktreePath, "change.txt", mode+" candidate\n")
			workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
			workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
			code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "publish", "Staging "+mode, "--verify", "go test ./... -> passed", "--proof", "Intent verified", "--risk", "Risk: low\nRollback: revert")
			if code != 0 || !strings.Contains(output, "pull/17") {
				t.Fatalf("publish %s candidate: %d %s", mode, code, output)
			}
			published := withRef(refs, "refs/heads/"+change.Branch, head)
			assertOriginRefs(t, f, f.remote, published, "publication")
			requireOneStagingPR(t, f)
			signalReviewPolicyTask(t, f, change, "--pr", "https://github.com/acme/shop/pull/17")
			if mode == "quick" {
				if code, output := runReviewIdentityCLI(t, f, "merge", "t1"); code != 1 || !strings.Contains(output, "review_required") {
					t.Fatalf("quick Merge without an exact-head pass: %d %s", code, output)
				}
				passCandidateReview(t, f, f.repo, change, "t2")
			}
			if code, output := runReviewIdentityCLI(t, f, "merge", "t1"); code != 0 {
				t.Fatalf("%s Merge readiness on staging: %d %s", mode, code, output)
			}
			if gated := f.mustTask(t, "t1").GatedSHA; gated != head {
				t.Fatalf("Merge pinned %s, want published head %s", gated, head)
			}
			assertOriginRefs(t, f, f.remote, published, "Merge")
		})
	}
}

// TestStagingFullPipelineCIRepairRepublishesAcrossRestart proves the
// full-pipeline loop on staging with a bare-origin ref audit: nothing reaches
// origin before local validation, a CI failure returns to a fresh private
// candidate bound to staging, and a promotion interrupted after its push
// completes on rerun without a second PR or any target movement.
func TestStagingFullPipelineCIRepairRepublishesAcrossRestart(t *testing.T) {
	f := reviewIdentityFixture(t)
	staging := stagingTarget(t, f)
	h := startCIRepairChangeWith(t, f, func() (int, string) {
		return startStagingTask(t, f, "Staging CI repair")
	}, [2]string{"ci_loop.max_fix_rounds", "2"})
	if h.change.TargetBranch != "staging" {
		t.Fatalf("Task target=%q, want staging", h.change.TargetBranch)
	}
	refs := originRefs(t, f, f.remote)
	h.auditOrigin = func(t *testing.T) {
		t.Helper()
		assertOriginRefs(t, f, f.remote, withRef(refs, "refs/heads/"+h.change.Branch, h.originHead), "CI repair loop")
	}
	firstHead := h.publishThenFailCI(t)
	first, err := f.db.LatestValidationCandidate(context.Background(), h.change.ID, "")
	if err != nil || first.CandidateSHA != firstHead || first.TargetSHA != staging {
		t.Fatalf("first published candidate=%+v err=%v, want head %s bound to staging %s", first, err, firstHead, staging)
	}
	requireOneStagingPR(t, f)

	candidate := h.submitCandidate(t, "Repaired staging candidate")
	if candidate.CandidateSHA == firstHead || candidate.TargetSHA != staging {
		t.Fatalf("CI repair candidate=%+v, want a fresh head bound to staging %s", candidate, staging)
	}
	h.validateCandidate(t, candidate, "t3")

	// The promotion process dies after pushing the Task branch and before it
	// updates the PR; rerunning publish finishes from the recorded intent.
	crashEnv := setEnv(h.workerEnv, "POSSE_INTENT_CRASH_AT", "publish:after:candidate.push")
	if code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, crashEnv, "publish", "Repaired staging candidate"); code != 86 {
		t.Fatalf("promotion did not stop at the injected crash: %d %s", code, output)
	}
	h.originHead = candidate.CandidateSHA
	h.assertOriginHead(t, candidate.CandidateSHA)
	if interrupted, err := f.db.LatestValidationCandidate(context.Background(), h.change.ID, ""); err != nil || interrupted.State != store.ValidationCandidatePromoting {
		t.Fatalf("interrupted promotion=%+v err=%v, want promoting", interrupted, err)
	}
	if code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, h.workerEnv, "publish", "Repaired staging candidate"); code != 0 || !strings.Contains(output, "promoted: candidate") {
		t.Fatalf("rerun after the interrupted promotion: %d %s", code, output)
	}
	h.assertOriginHead(t, candidate.CandidateSHA)
	requireOneStagingPR(t, f)
	if promoted, err := f.db.LatestValidationCandidate(context.Background(), h.change.ID, ""); err != nil || promoted.State != store.ValidationCandidatePromoted || promoted.PromotedSHA != candidate.CandidateSHA {
		t.Fatalf("recovered promotion=%+v err=%v", promoted, err)
	}

	if err := os.WriteFile(filepath.Join(f.root, "ci-worker-allow-done"), []byte("complete\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(10*time.Second, func() bool {
		signalLog, err := os.ReadFile(filepath.Join(f.root, "ci-worker-signal.log"))
		return err == nil && strings.Contains(string(signalLog), "signal-ok")
	}) {
		signalLog, _ := os.ReadFile(filepath.Join(f.root, "ci-worker-signal.log"))
		t.Fatalf("fake Worker did not complete the repaired Task: %s", signalLog)
	}
	f.waitTaskState(t, "t1", store.StateDone)
	f.writeGraphQL(t, "pr1", "OPEN", "SUCCESS", "REVIEW_REQUIRED", "MERGEABLE", "", candidate.CandidateSHA)
	runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t1")
	observation, err := f.db.LatestPRObservation(context.Background(), h.change.ID)
	if err != nil || observation.HeadSHA != candidate.CandidateSHA || !strings.Contains(observation.Checks, "SUCCESS") {
		t.Fatalf("republished staging head lacks a green CI observation: %#v err=%v", observation, err)
	}
	h.assertOriginHead(t, candidate.CandidateSHA)
}

// TestStagingFullPipelineTargetAndAuthorRacesRefusePublication proves that a
// staging candidate ignores movement of main, is invalidated by movement of
// staging and by an author commit after submission, and publishes only a
// fresh candidate validated against the current staging head.
func TestStagingFullPipelineTargetAndAuthorRacesRefusePublication(t *testing.T) {
	f := reviewIdentityFixture(t)
	stopBackgroundWatch(t, f)
	staging := stagingTarget(t, f)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.merge_mode", "pr")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", "full-pipeline")
	if code, output := startStagingTask(t, f, "Staging races"); code != 0 {
		t.Fatalf("start staging Change Task: %d %s", code, output)
	}
	change := f.mustTask(t, "t1")
	commitCandidateFile(t, f, change.WorktreePath, "change.txt", "race candidate\n")
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	refs := originRefs(t, f, f.remote)
	audit := func(stage string) {
		t.Helper()
		assertOriginRefs(t, f, f.remote, refs, stage)
	}
	status := func(candidate store.ValidationCandidate) string {
		t.Helper()
		code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, workerEnv, "validate", "status")
		if code != 0 {
			t.Fatalf("validate status: %d %s", code, output)
		}
		return output
	}

	candidate := submitStagingCandidate(t, f, change, workerEnv, "", staging)
	audit("submission")
	passCandidateReview(t, f, f.repo, change, "t2")
	recordCandidateStages(t, f, workerEnv, candidate, audit)

	// main is not the frozen target, so its movement keeps the candidate ready.
	refs["refs/heads/main"] = advanceBranch(t, f, f.remote, "main", "", "unrelated main commit")
	if output := status(candidate); !strings.Contains(output, "target_current: true") || !strings.Contains(output, "publication_ready: true") {
		t.Fatalf("main movement invalidated a staging candidate:\n%s", output)
	}

	// Target race: staging moves after every local stage passed.
	staging = advanceBranch(t, f, f.remote, "staging", "", "concurrent staging commit")
	refs["refs/heads/staging"] = staging
	if output := status(candidate); !strings.Contains(output, "target_current: false") || !strings.Contains(output, "publication_ready: false") {
		t.Fatalf("staging movement kept the candidate ready:\n%s", output)
	}
	if code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, workerEnv, "publish", "Staging candidate"); code == 0 || !strings.Contains(output, "frozen target moved") {
		t.Fatalf("promotion after staging moved: %d %s", code, output)
	}
	audit("refused target race")
	refreshed := submitStagingCandidate(t, f, change, workerEnv, "", staging)
	if refreshed.ID == candidate.ID {
		t.Fatal("staging movement did not require a fresh candidate")
	}
	audit("refreshed submission")
	passCandidateReview(t, f, f.repo, change, "t3")
	recordCandidateStages(t, f, workerEnv, refreshed, audit)

	// Author race: the Worker commits after submission and validation.
	author := commitCandidateFile(t, f, change.WorktreePath, "author-followup.txt", "late author commit\n")
	if code, output := runReleaseCLI(t, f.binary, refreshed.WorktreePath, workerEnv, "publish", "Staging candidate"); code == 0 || !strings.Contains(output, "candidate_author_moved") {
		t.Fatalf("promotion after the author moved: %d %s", code, output)
	}
	audit("refused author race")
	final := submitStagingCandidate(t, f, change, workerEnv, "", staging)
	if final.Generation != refreshed.Generation+1 || final.SubmittedSHA != author {
		t.Fatalf("author resubmission=%+v, want generation %d at %s", final, refreshed.Generation+1, author)
	}
	passCandidateReview(t, f, f.repo, change, "t4")
	recordCandidateStages(t, f, workerEnv, final, audit)
	if code, output := runReleaseCLI(t, f.binary, final.WorktreePath, workerEnv, "publish", "Staging candidate"); code != 0 || !strings.Contains(output, "promoted: candidate") {
		t.Fatalf("promote the fresh staging candidate: %d %s", code, output)
	}
	refs["refs/heads/"+change.Branch] = final.CandidateSHA
	audit("promotion")
	requireOneStagingPR(t, f)
	if !gitAncestorTest(t, f, final.WorktreePath, staging, final.CandidateSHA) {
		t.Fatalf("published candidate %s does not contain the current staging head %s", final.CandidateSHA, staging)
	}
}

func gitAncestorTest(t *testing.T, f *prLifecycleFixture, dir, ancestor, descendant string) bool {
	t.Helper()
	_, err := gitCommand(f.env, dir, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// TestStagingFullPipelineWorkspaceMembersBindStagingTargets proves Member
// coverage on staging: each Member candidate freezes its own staging head, a
// staging race on one Member refuses only that Member, the PR Member publishes
// only its Task branch and Merge fast-forwards only the local Member's staging.
func TestStagingFullPipelineWorkspaceMembersBindStagingTargets(t *testing.T) {
	f := reviewIdentityFixture(t)
	apiStaging := stagingTarget(t, f)
	ws := newFullPipelineWorkspace(t, f)
	webStaging := advanceBranch(t, f, ws.webRemote, "staging", gitRef(t, f, ws.webRemote, "refs/heads/main"), "web staging-only commit")
	gitTest(t, f.env, ws.web, "fetch", "-q", "origin", "staging:staging")
	brief := filepath.Join(f.root, "workspace-staging.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Workspace staging\ndone_when: checked\nrepos: [api, web]\nbase: staging\ntarget: staging\n---\nChange both Members on staging.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, ws.root, ws.leadEnv, "start", "--brief", brief, "--name", "workspace-staging", "--profile", "author"); code != 0 {
		t.Fatalf("start workspace staging Task: code=%d %s", code, output)
	}
	change := mustProjectTask(t, f, ws.project.ID, "t1")
	repos, err := f.db.TaskRepos(context.Background(), change.ID)
	if err != nil || len(repos) != 2 {
		t.Fatalf("workspace Members: %+v %v", repos, err)
	}
	worktrees := map[string]string{}
	for _, repo := range repos {
		if repo.TargetBranch != "staging" {
			t.Fatalf("Member %s froze target %q, want staging", repo.Repo, repo.TargetBranch)
		}
		worktrees[repo.Repo] = repo.WorktreePath
		commitCandidateFile(t, f, repo.WorktreePath, "change.txt", repo.Repo+" staging candidate\n")
	}
	apiRefs := originRefs(t, f, f.remote)
	webRefs := originRefs(t, f, ws.webRemote)
	webMain := gitRef(t, f, ws.web, "refs/heads/main")
	webLocalStaging := webStaging
	audit := func(stage string) {
		t.Helper()
		assertOriginRefs(t, f, f.remote, apiRefs, "api "+stage)
		assertOriginRefs(t, f, ws.webRemote, webRefs, "web "+stage)
		if got := gitRef(t, f, ws.web, "refs/heads/main"); got != webMain {
			t.Fatalf("web local main moved to %s during %s", got, stage)
		}
		if got := gitRef(t, f, ws.web, "refs/heads/staging"); got != webLocalStaging {
			t.Fatalf("web local staging=%s during %s, want %s", got, stage, webLocalStaging)
		}
	}
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	candidates := map[string]store.ValidationCandidate{
		"api": submitStagingCandidate(t, f, change, workerEnv, "api", apiStaging),
		"web": submitStagingCandidate(t, f, change, workerEnv, "web", webStaging),
	}
	audit("submission")
	passCandidateReviewAs(t, f, ws.leadEnv, ws.root, change, "t2")
	audit("review")

	// Member target race: api's staging moves after its stages pass.
	recordCandidateStages(t, f, workerEnv, candidates["api"], func(stage string) { audit("api " + stage) }, "--repo", "api")
	apiStaging = advanceBranch(t, f, f.remote, "staging", "", "concurrent api staging commit")
	apiRefs["refs/heads/staging"] = apiStaging
	if code, output := runReleaseCLI(t, f.binary, candidates["api"].WorktreePath, workerEnv, "publish", "--repo", "api", "Promote"); code == 0 || !strings.Contains(output, "frozen target moved") {
		t.Fatalf("api promotion after its staging moved: code=%d %s", code, output)
	}
	audit("refused api target race")
	candidates["api"] = submitStagingCandidate(t, f, change, workerEnv, "api", apiStaging)
	if web, err := f.db.LatestValidationCandidate(context.Background(), change.ID, "web"); err != nil || web.ID != candidates["web"].ID || web.State != store.ValidationCandidateActive || web.TargetSHA != webStaging {
		t.Fatalf("api staging race disturbed the web candidate: %+v err=%v", web, err)
	}
	passCandidateReviewAs(t, f, ws.leadEnv, ws.root, change, "t3")
	for _, member := range []string{"api", "web"} {
		recordCandidateStages(t, f, workerEnv, candidates[member], func(stage string) { audit(member + " " + stage) }, "--repo", member)
	}
	if code, output := runReleaseCLI(t, f.binary, candidates["web"].WorktreePath, workerEnv, "publish", "--repo", "web", "Promote"); code != 0 || !strings.Contains(output, "for local delivery") {
		t.Fatalf("promote web staging candidate: code=%d %s", code, output)
	}
	audit("web promotion")
	if code, output := runReleaseCLI(t, f.binary, candidates["api"].WorktreePath, workerEnv, "publish", "--repo", "api", "Promote"); code != 0 || !strings.Contains(output, "promoted: candidate") {
		t.Fatalf("promote api staging candidate: code=%d %s", code, output)
	}
	apiRefs["refs/heads/"+change.Branch] = candidates["api"].CandidateSHA
	audit("api promotion")
	requireOneStagingPR(t, f)
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "signal", "done", "Both staging Members validated"); code != 0 {
		t.Fatalf("done Signal: code=%d %s", code, output)
	}
	if code, output := runReleaseCLI(t, f.binary, ws.root, ws.leadEnv, "merge", "t1", "--execute", "--user-approved", "Merge both staging Members"); code != 0 {
		t.Fatalf("Merge staging Members: code=%d %s", code, output)
	}
	webLocalStaging = candidates["web"].CandidateSHA
	audit("Merge")
}

// TestStagingFullPipelineReviewCarryAndCapDecision proves three Review Rounds
// on staging: a pass carries across a tree-preserving staging advance without
// carrying local stages, and the third changes_requested round raises one
// User-only cap Decision bound to the current candidate. Nothing is published.
func TestStagingFullPipelineReviewCarryAndCapDecision(t *testing.T) {
	f := reviewIdentityFixture(t)
	stopBackgroundWatch(t, f)
	staging := stagingTarget(t, f)
	for _, setting := range [][2]string{{"defaults.merge_mode", "pr"}, {"defaults.review", "full-pipeline"}, {"review_loop.enabled", "true"}} {
		runPosse(t, f.binary, f.repo, f.env, "config", "set", setting[0], setting[1])
	}
	if code, output := startStagingTask(t, f, "Staging review rounds"); code != 0 {
		t.Fatalf("start staging Change Task: %d %s", code, output)
	}
	change := f.mustTask(t, "t1")
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	refs := originRefs(t, f, f.remote)
	audit := func(stage string) {
		t.Helper()
		assertOriginRefs(t, f, f.remote, refs, stage)
	}
	commitCandidateFile(t, f, change.WorktreePath, "round-1.txt", "validated operation\n")
	passed := submitStagingCandidate(t, f, change, workerEnv, "", staging)
	turn, err := f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, passed.WorktreePath, workerEnv, "signal", "done", "Candidate submitted", "--turn", turn); code != 0 {
		t.Fatalf("complete submission turn: %d %s", code, output)
	}
	recordReviewRound(t, f, "t1", "pass", `{"schema":"posse.review-findings.v1","patterns":[],"resolutions":[]}`)
	audit("Review Round 1")

	// staging advances without changing its tree: the refreshed candidate
	// carries Review Round 1 but none of its local stages.
	staging = advanceBranch(t, f, f.remote, "staging", "", "tree-preserving staging commit")
	refs["refs/heads/staging"] = staging
	if code, output := runReleaseCLI(t, f.binary, passed.WorktreePath, workerEnv, "signal", "working", "Refresh the candidate for staging"); code != 0 {
		t.Fatalf("reopen Change Task: %d %s", code, output)
	}
	refreshed := submitStagingCandidate(t, f, change, workerEnv, "", staging)
	if refreshed.CandidateSHA == passed.CandidateSHA {
		t.Fatal("staging advance did not refresh the candidate")
	}
	turn, err = f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, refreshed.WorktreePath, workerEnv, "signal", "done", "Candidate refreshed for staging", "--turn", turn); code != 0 {
		t.Fatalf("record Review carry: %d %s", code, output)
	}
	var carries int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM review_carries WHERE ship_task_id=? AND from_head=? AND to_head=?`, change.ID, passed.CandidateSHA, refreshed.CandidateSHA).Scan(&carries); err != nil || carries != 1 {
		t.Fatalf("staging Review carry rows=%d err=%v", carries, err)
	}
	if code, output := runReleaseCLI(t, f.binary, refreshed.WorktreePath, workerEnv, "signal", "working", "Continue staging validation"); code != 0 {
		t.Fatalf("reopen for local stages: %d %s", code, output)
	}
	if code, output := runReleaseCLI(t, f.binary, refreshed.WorktreePath, workerEnv, "validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent behavior verified"); code != 0 {
		t.Fatalf("carried Review did not admit the test stage: %d %s", code, output)
	}
	if code, output := runReleaseCLI(t, f.binary, refreshed.WorktreePath, workerEnv, "publish", "Staging candidate"); code == 0 || !strings.Contains(output, "validation_stage_required") {
		t.Fatalf("carry skipped docs/lint stages: %d %s", code, output)
	}
	audit("carry")

	// Rounds 2 and 3 request changes on new author heads; the third spends
	// the default cap.
	for round := 2; round <= 3; round++ {
		if round == 3 {
			if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "signal", "working", "Address round 2"); code != 0 {
				t.Fatalf("reopen Change Task: %d %s", code, output)
			}
		}
		name := fmt.Sprintf("round-%d.txt", round)
		commitCandidateFile(t, f, change.WorktreePath, name, "unsafe operation\n")
		// The first publish reports the author movement and preserves the stale
		// candidate; the next submits a fresh one.
		if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "publish", "Staging candidate"); code == 0 || !strings.Contains(output, "candidate_author_moved") {
			t.Fatalf("round %d author movement was not reported: %d %s", round, code, output)
		}
		candidate := submitStagingCandidate(t, f, change, workerEnv, "", staging)
		turn, err = f.db.TaskTurnToken(context.Background(), change.ID)
		if err != nil {
			t.Fatal(err)
		}
		if code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, workerEnv, "signal", "done", fmt.Sprintf("Candidate for round %d", round), "--turn", turn); code != 0 {
			t.Fatalf("complete round %d submission: %d %s", round, code, output)
		}
		recordReviewRound(t, f, "t1", "changes_requested", blockingFindings(fmt.Sprintf("unsafe-operation-%d", round), name, "unsafe operation"))
		audit(fmt.Sprintf("Review Round %d", round))
		decisions := openReviewCapDecisions(t, f, change)
		if round == 2 && len(decisions) != 0 {
			t.Fatalf("round 2 of 3 raised a cap Decision: %v", decisions)
		}
		if round == 3 {
			if len(decisions) != 1 {
				t.Fatalf("round 3 raised %d cap Decisions, want one", len(decisions))
			}
			var origin string
			if err := f.db.QueryRowContext(context.Background(), `SELECT origin FROM decisions WHERE id=?`, decisions[0]).Scan(&origin); err != nil || !strings.HasSuffix(origin, candidate.CandidateSHA) {
				t.Fatalf("cap Decision origin=%q err=%v, want candidate %s", origin, err, candidate.CandidateSHA)
			}
			for _, option := range []string{"accept-head", "final-fix", "rethink", "discard"} {
				if code, output := runReviewIdentityCLI(t, f, "decide", fmt.Sprint(decisions[0]), option); code != 1 || !strings.Contains(output, "user_only") {
					t.Fatalf("Lead answered the cap Decision with %s: %d %s", option, code, output)
				}
			}
		}
	}
	if rounds := reviewRoundCount(t, f, change); rounds != 3 {
		t.Fatalf("Review Rounds=%d, want 3", rounds)
	}
	audit("cap Decision")
}
