//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// passCandidateReview starts the next Review Round of change and records a
// passing verdict from its independent Reviewer.
func passCandidateReview(t *testing.T, f *prLifecycleFixture, dir string, change store.Task, reviewSeq string) {
	t.Helper()
	command := []string{"review", "start", taskIDOf(change), "--profile", "reviewer"}
	if code, output := runFixtureCLI(t, f, dir, command...); code != 0 {
		t.Fatalf("start candidate Review Round: code=%d %s", code, output)
	}
	review := mustProjectTask(t, f, change.ProjectID, reviewSeq)
	waitForReviewIdentityAgent(t, f, review.PaneID)
	scratch := filepath.Join(f.home, "scratch", projectNameOf(t, f, change.ProjectID), reviewSeq)
	report := filepath.Join(scratch, "report.md")
	findings := filepath.Join(scratch, "findings.json")
	if err := os.WriteFile(report, []byte("The private candidate passed review.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(findings, []byte(`{"schema":"posse.review-findings.v1","patterns":[],"resolutions":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--findings", findings, "--verdict", "pass")
}

// recordCandidateStages records test -> docs -> lint from the private
// candidate checkout and calls audit after every stage.
func recordCandidateStages(t *testing.T, f *prLifecycleFixture, workerEnv []string, candidate store.ValidationCandidate, audit func(stage string), repoArgs ...string) {
	t.Helper()
	for _, args := range [][]string{
		{"validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent/E2E behavior verified"},
		{"validate", "docs", "--no-comments", "passed", "--no-owner-doc-change", "No owner documentation changes are warranted", "--proof", "No unnecessary comments; complete description prepared"},
		{"validate", "lint", "--proof", "Configured Gates passed", "--coverage", "No workflow jobs in this fixture"},
	} {
		code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, workerEnv, append(args, repoArgs...)...)
		if code != 0 {
			t.Fatalf("candidate stage %v failed: %d %s", args, code, output)
		}
		audit(args[1])
	}
}

func runFixtureCLI(t *testing.T, f *prLifecycleFixture, dir string, args ...string) (int, string) {
	t.Helper()
	return runReleaseCLI(t, f.binary, dir, f.leadEnv, args...)
}

func mustProjectTask(t *testing.T, f *prLifecycleFixture, projectID int64, id string) store.Task {
	t.Helper()
	task, err := f.db.Task(context.Background(), projectID, id)
	if err != nil {
		t.Fatalf("Task %s: %v", id, err)
	}
	return task
}

func projectNameOf(t *testing.T, f *prLifecycleFixture, projectID int64) string {
	t.Helper()
	project, err := f.db.ProjectByID(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	return project.Name
}

func gitRef(t *testing.T, f *prLifecycleFixture, gitDir, ref string) string {
	t.Helper()
	return strings.TrimSpace(gitTest(t, f.env, gitDir, "rev-parse", "--verify", ref))
}

// TestFullPipelineLocalDeliveryValidatesBeforeTargetFastForward proves that a
// local-delivery full-pipeline Task never moves its target ref before every
// local stage passes on one exact private candidate, and that the legacy
// Merge path cannot integrate an unvalidated head.
func TestFullPipelineLocalDeliveryValidatesBeforeTargetFastForward(t *testing.T) {
	f := reviewIdentityFixture(t)
	stopBackgroundWatch(t, f)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", "full-pipeline")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "autonomy.yolo", "true", "--project", "shop")
	if code, output := startReviewIdentityTask(t, f, "Local candidate", "author", ""); code != 0 {
		t.Fatalf("start Change Task: code=%d %s", code, output)
	}
	change := f.mustTask(t, "t1")
	if change.MergeMode != "local" {
		t.Fatalf("fixture Task merge mode=%q, want local", change.MergeMode)
	}
	target := gitRef(t, f, f.repo, "refs/heads/main")
	originTarget := gitRef(t, f, f.remote, "refs/heads/main")
	auditTarget := func(stage string) {
		t.Helper()
		if current := gitRef(t, f, f.repo, "refs/heads/main"); current != target {
			t.Fatalf("local target moved to %s during %s, before validated integration", current, stage)
		}
		if current := gitRef(t, f, f.remote, "refs/heads/main"); current != originTarget {
			t.Fatalf("origin target moved to %s during %s", current, stage)
		}
		assertTaskBranchAbsentOnBareOrigin(t, f, change.Branch)
	}
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "change.txt"), []byte("local candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, change.WorktreePath, "add", "change.txt")
	gitTest(t, f.env, change.WorktreePath, "commit", "-m", "add local candidate")
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)

	// Without a candidate, neither the done Signal nor the legacy Merge path
	// can integrate the author head.
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "signal", "done", "Unvalidated"); code == 0 || !strings.Contains(output, "candidate_missing") {
		t.Fatalf("done Signal without a private candidate: code=%d %s", code, output)
	}
	auditTarget("unvalidated done Signal")

	code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv,
		"publish", "Local candidate summary", "--verify", "go test ./... -> passed",
		"--proof", "Intent behavior verified", "--risk", "Risk: low\nRollback: revert the local fast-forward")
	if code != 0 || !strings.Contains(output, "validation required before publication") {
		t.Fatalf("submit local private candidate: code=%d %s", code, output)
	}
	auditTarget("submission")
	candidate, err := f.db.LatestValidationCandidate(context.Background(), change.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.TargetSHA != target {
		t.Fatalf("candidate froze target %s, want local target %s", candidate.TargetSHA, target)
	}
	if code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, workerEnv, "publish", "Local candidate summary"); code == 0 || !strings.Contains(output, "validation_review_required") {
		t.Fatalf("unvalidated local promotion: code=%d %s", code, output)
	}
	auditTarget("refused promotion")

	passCandidateReview(t, f, f.repo, change, "t2")
	auditTarget("review")
	recordCandidateStages(t, f, workerEnv, candidate, auditTarget)

	code, output = runReleaseCLI(t, f.binary, candidate.WorktreePath, workerEnv, "publish", "Local candidate summary")
	if code != 0 || !strings.Contains(output, "promoted: candidate") {
		t.Fatalf("promote validated local candidate: code=%d %s", code, output)
	}
	// Local promotion fast-forwards only the author Task branch; integration
	// into the target stays with Merge.
	auditTarget("local promotion")
	if head := gitRef(t, f, change.WorktreePath, "refs/heads/"+change.Branch); head != candidate.CandidateSHA {
		t.Fatalf("Task branch=%s after local promotion, want candidate %s", head, candidate.CandidateSHA)
	}
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "signal", "done", "Validated local candidate"); code != 0 {
		t.Fatalf("done Signal after local promotion: code=%d %s", code, output)
	}
	// The legacy local Merge path cannot integrate a head edited after
	// validation; restoring the validated head lets it proceed.
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "late.txt"), []byte("late edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, change.WorktreePath, "add", "late.txt")
	gitTest(t, f.env, change.WorktreePath, "commit", "-m", "unvalidated late edit")
	if code, output := runFixtureCLI(t, f, f.repo, "merge", "t1", "--execute", "--user-approved", "Merge the validated candidate"); code == 0 || !strings.Contains(output, "validation_stale") {
		t.Fatalf("Merge of an unvalidated local head: code=%d %s", code, output)
	}
	auditTarget("refused Merge")
	gitTest(t, f.env, change.WorktreePath, "reset", "-q", "--hard", candidate.CandidateSHA)
	if code, output := runFixtureCLI(t, f, f.repo, "merge", "t1", "--execute", "--user-approved", "Merge the validated candidate"); code != 0 {
		t.Fatalf("Merge validated local candidate: code=%d %s", code, output)
	}
	if merged := gitRef(t, f, f.repo, "refs/heads/main"); merged != candidate.CandidateSHA {
		t.Fatalf("local target=%s after Merge, want validated candidate %s", merged, candidate.CandidateSHA)
	}
	assertTaskBranchAbsentOnBareOrigin(t, f, change.Branch)
}

// fullPipelineWorkspace is a "stack" workspace Project whose api Member
// delivers by PR through the fake forge, backed by the bare origin f.remote,
// and whose web Member delivers locally into its own target ref. Both Members
// use full-pipeline review.
type fullPipelineWorkspace struct {
	root, api, web, webRemote string
	project                   store.Project
	leadEnv                   []string
}

func newFullPipelineWorkspace(t *testing.T, f *prLifecycleFixture) fullPipelineWorkspace {
	t.Helper()
	stopBackgroundWatch(t, f)
	root := filepath.Join(f.root, "stack")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	api := filepath.Join(root, "api")
	gitTest(t, f.env, root, "clone", "-q", f.remote, api)
	gitTest(t, f.env, api, "remote", "set-url", "origin", "https://github.com/acme/shop.git")
	gitTest(t, f.env, api, "config", "url.file://"+f.remote+".insteadOf", "https://github.com/acme/shop.git")
	web := filepath.Join(root, "web")
	webRemote := filepath.Join(f.root, "web.git")
	initRepository(t, web, webRemote, f.env)
	project, err := f.db.CreateWorkspaceProject(context.Background(), "stack", root, []store.ProjectRepo{
		{Name: "api", Path: "api", DefaultBranch: "main", Status: store.RepoActive, OriginHost: "github.com"},
		{Name: "web", Path: "web", DefaultBranch: "main", Status: store.RepoActive},
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := createWorkspace(herdr.NewWithEnv("herdr", f.env), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetProjectLead(context.Background(), project.ID, workspace.Workspace.WorkspaceID, workspace.RootPane.PaneID, "posse:stack:lead"); err != nil {
		t.Fatal(err)
	}
	leadEnv := setEnv(f.leadEnv, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", workspace.Workspace.WorkspaceID)
	for _, setting := range [][2]string{
		{"defaults.review", "full-pipeline"}, {"autonomy.yolo", "true"},
		{"repositories.api.merge_mode", "pr"}, {"repositories.web.merge_mode", "local"},
	} {
		runPosse(t, f.binary, root, f.env, "config", "set", setting[0], setting[1], "--project", "stack")
	}
	return fullPipelineWorkspace{root: root, api: api, web: web, webRemote: webRemote, project: project, leadEnv: leadEnv}
}

// TestFullPipelineWorkspaceMembersPublishOnlyValidatedCandidates proves the
// per-Member barrier: a PR-delivery Member reaches its bare origin and a
// local-delivery Member reaches its target ref only after that Member's own
// private candidate passes review -> test -> docs -> lint, and a Member edit
// after validation cannot be integrated.
func TestFullPipelineWorkspaceMembersPublishOnlyValidatedCandidates(t *testing.T) {
	f := reviewIdentityFixture(t)
	ws := newFullPipelineWorkspace(t, f)
	root, web, webRemote, project, leadEnv := ws.root, ws.web, ws.webRemote, ws.project, ws.leadEnv
	brief := filepath.Join(f.root, "workspace-candidates.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Workspace candidates\ndone_when: checked\nrepos: [api, web]\n---\nChange both Members.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, root, leadEnv, "start", "--brief", brief, "--name", "workspace-candidates", "--profile", "author"); code != 0 {
		t.Fatalf("start workspace Change Task: code=%d %s", code, output)
	}
	change := mustProjectTask(t, f, project.ID, "t1")
	repos, err := f.db.TaskRepos(context.Background(), change.ID)
	if err != nil || len(repos) != 2 {
		t.Fatalf("workspace Members: %+v %v", repos, err)
	}
	worktrees := map[string]string{}
	for _, repo := range repos {
		worktrees[repo.Repo] = repo.WorktreePath
		if err := os.WriteFile(filepath.Join(repo.WorktreePath, "change.txt"), []byte(repo.Repo+" candidate\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, f.env, repo.WorktreePath, "add", "change.txt")
		gitTest(t, f.env, repo.WorktreePath, "commit", "-m", "change "+repo.Repo)
	}
	apiOriginTarget := gitRef(t, f, f.remote, "refs/heads/main")
	webTarget := gitRef(t, f, web, "refs/heads/main")
	webOriginTarget := gitRef(t, f, webRemote, "refs/heads/main")
	promotedAPI := ""
	audit := func(stage string) {
		t.Helper()
		if promotedAPI != "" {
			if published := gitRef(t, f, f.remote, "refs/heads/"+change.Branch); published != promotedAPI {
				t.Fatalf("api origin Task branch=%s during %s, want promoted candidate %s", published, stage, promotedAPI)
			}
		} else {
			assertTaskBranchAbsentOnBareOrigin(t, f, change.Branch)
		}
		if current := gitRef(t, f, f.remote, "refs/heads/main"); current != apiOriginTarget {
			t.Fatalf("api origin target moved during %s", stage)
		}
		if current := gitRef(t, f, web, "refs/heads/main"); current != webTarget {
			t.Fatalf("web local target moved to %s during %s, before validated integration", current, stage)
		}
		if current := gitRef(t, f, webRemote, "refs/heads/main"); current != webOriginTarget {
			t.Fatalf("web origin target moved during %s", stage)
		}
		command := exec.Command("git", "--git-dir", webRemote, "show-ref", "--verify", "--quiet", "refs/heads/"+change.Branch)
		command.Env = f.env
		if output, err := command.CombinedOutput(); err == nil {
			t.Fatalf("local-delivery Member branch was pushed during %s: %s", stage, output)
		}
	}
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)

	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "signal", "done", "Unvalidated"); code == 0 || !strings.Contains(output, "candidate_missing") {
		t.Fatalf("workspace done Signal without private candidates: code=%d %s", code, output)
	}
	audit("unvalidated done Signal")
	candidates := map[string]store.ValidationCandidate{}
	for _, member := range []string{"api", "web"} {
		code, output := runReleaseCLI(t, f.binary, worktrees[member], workerEnv,
			"publish", "--repo", member, member+" candidate summary", "--verify", "go test ./... -> passed",
			"--proof", "Intent behavior verified", "--risk", "Risk: low\nRollback: revert the "+member+" change")
		if code != 0 || !strings.Contains(output, "validation required before publication") {
			t.Fatalf("submit %s private candidate: code=%d %s", member, code, output)
		}
		audit(member + " submission")
		candidate, err := f.db.LatestValidationCandidate(context.Background(), change.ID, member)
		if err != nil {
			t.Fatal(err)
		}
		candidates[member] = candidate
	}
	if candidates["web"].TargetSHA != webTarget || candidates["web"].ExpectedRemoteSHA != "" {
		t.Fatalf("local Member froze target=%s remote=%q, want local target %s and no remote", candidates["web"].TargetSHA, candidates["web"].ExpectedRemoteSHA, webTarget)
	}
	if candidates["api"].TargetSHA != apiOriginTarget {
		t.Fatalf("PR Member froze target=%s, want origin target %s", candidates["api"].TargetSHA, apiOriginTarget)
	}
	for _, member := range []string{"api", "web"} {
		if code, output := runReleaseCLI(t, f.binary, candidates[member].WorktreePath, workerEnv, "publish", "--repo", member, "Promote"); code == 0 || !strings.Contains(output, "validation_review_required") {
			t.Fatalf("unvalidated %s promotion: code=%d %s", member, code, output)
		}
	}
	audit("refused promotion")

	if code, output := runReleaseCLI(t, f.binary, root, leadEnv, "review", "start", "t1", "--profile", "reviewer"); code != 0 {
		t.Fatalf("start workspace candidate Review Round: code=%d %s", code, output)
	}
	review := mustProjectTask(t, f, project.ID, "t2")
	waitForReviewIdentityAgent(t, f, review.PaneID)
	scratch := filepath.Join(f.home, "scratch", "stack", "t2")
	report := filepath.Join(scratch, "report.md")
	findings := filepath.Join(scratch, "findings.json")
	if err := os.WriteFile(report, []byte("Both private candidates passed review.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(findings, []byte(`{"schema":"posse.review-findings.v1","patterns":[],"resolutions":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--findings", findings, "--verdict", "pass")
	audit("review")

	// web passes its stages first; api still cannot publish until its own
	// stages pass, and web's local promotion leaves its target alone.
	recordCandidateStages(t, f, workerEnv, candidates["web"], func(stage string) { audit("web " + stage) }, "--repo", "web")
	if code, output := runReleaseCLI(t, f.binary, candidates["api"].WorktreePath, workerEnv, "publish", "--repo", "api", "Promote"); code == 0 || !strings.Contains(output, "validation_stage_required") {
		t.Fatalf("api promotion before its own stages: code=%d %s", code, output)
	}
	audit("api refused promotion")
	if code, output := runReleaseCLI(t, f.binary, candidates["web"].WorktreePath, workerEnv, "publish", "--repo", "web", "Promote"); code != 0 || !strings.Contains(output, "for local delivery") {
		t.Fatalf("promote validated web candidate: code=%d %s", code, output)
	}
	audit("web local promotion")
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "signal", "done", "Only web validated"); code == 0 || !strings.Contains(output, "api:") {
		t.Fatalf("done Signal with an unpromoted Member: code=%d %s", code, output)
	}
	recordCandidateStages(t, f, workerEnv, candidates["api"], func(stage string) { audit("api " + stage) }, "--repo", "api")
	code, output := runReleaseCLI(t, f.binary, candidates["api"].WorktreePath, workerEnv, "publish", "--repo", "api", "Promote")
	if code != 0 || !strings.Contains(output, "promoted: candidate") || !strings.Contains(output, "pull/17") {
		t.Fatalf("promote validated api candidate: code=%d %s", code, output)
	}
	promotedAPI = candidates["api"].CandidateSHA
	audit("api promotion")
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "signal", "done", "Both Members validated"); code != 0 {
		t.Fatalf("done Signal after per-Member promotion: code=%d %s", code, output)
	}

	// An unvalidated web edit after done is refused by Merge before any
	// Member integrates; restoring the validated head lets Merge proceed.
	if err := os.WriteFile(filepath.Join(worktrees["web"], "late.txt"), []byte("late edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, worktrees["web"], "add", "late.txt")
	gitTest(t, f.env, worktrees["web"], "commit", "-m", "unvalidated late edit")
	if code, output := runReleaseCLI(t, f.binary, root, leadEnv, "merge", "t1", "--execute", "--user-approved", "Merge both Members"); code == 0 || !strings.Contains(output, "validation_stale") {
		t.Fatalf("Merge of an unvalidated Member head: code=%d %s", code, output)
	}
	audit("refused Merge")
	gitTest(t, f.env, worktrees["web"], "reset", "-q", "--hard", candidates["web"].CandidateSHA)
	if code, output := runReleaseCLI(t, f.binary, root, leadEnv, "merge", "t1", "--execute", "--user-approved", "Merge both Members"); code != 0 {
		t.Fatalf("Merge validated workspace Members: code=%d %s", code, output)
	}
	if merged := gitRef(t, f, web, "refs/heads/main"); merged != candidates["web"].CandidateSHA {
		t.Fatalf("web target=%s after Merge, want validated candidate %s", merged, candidates["web"].CandidateSHA)
	}
	if published := gitRef(t, f, f.remote, "refs/heads/"+change.Branch); published != candidates["api"].CandidateSHA {
		t.Fatalf("api origin Task branch=%s after Merge, want validated candidate %s", published, candidates["api"].CandidateSHA)
	}
}

// workspaceForgeScript fakes GitHub for workspace Members acme/<repo>, each
// backed by the bare origin $POSSE_TEST_ROOT/ws-<repo>.git. A merge moves the
// Member's origin main to the PR head and records the PR as merged.
const workspaceForgeScript = `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$POSSE_TEST_ROOT/ws-gh.log"
state="$POSSE_TEST_ROOT/ws-gh"
mkdir -p "$state"
remote() { printf '%s/ws-%s.git' "$POSSE_TEST_ROOT" "$1"; }
number() { case "$1" in api) echo 31 ;; web) echo 32 ;; *) exit 91 ;; esac; }
head_of() { git --git-dir="$(remote "$1")" rev-parse refs/heads/posse/t1; }
pull_json() {
  status=OPEN
  if [ -e "$state/$1-merged" ]; then status=MERGED; fi
  printf '{"url":"https://github.com/acme/%s/pull/%s","state":"%s","headRefOid":"%s","headRefName":"posse/t1","baseRefName":"main","headRepository":{"nameWithOwner":"acme/%s"}}' "$1" "$(number "$1")" "$status" "$(head_of "$1")" "$1"
}
case "$1 $2" in
  "pr list")
    repo=$(git config --get remote.origin.url | sed -E 's#.*/acme/([^/.]+)(\.git)?$#\1#')
    if [ -e "$state/$repo-open" ]; then printf '[%s]\n' "$(pull_json "$repo")"; else printf '[]\n'; fi ;;
  "pr create")
    repo=$(git config --get remote.origin.url | sed -E 's#.*/acme/([^/.]+)(\.git)?$#\1#')
    : > "$state/$repo-open"
    printf 'https://github.com/acme/%s/pull/%s\n' "$repo" "$(number "$repo")" ;;
  "pr view")
    repo=$(printf '%s' "$3" | sed -E 's#https://github.com/acme/([^/]+)/pull/.*#\1#')
    case "$*" in
      *'--json title,body'*) printf '{"title":"t","body":"b"}\n' ;;
      *) pull_json "$repo"; printf '\n' ;;
    esac ;;
  "pr edit") printf 'Edited\n' ;;
  "pr merge")
    repo=$(printf '%s' "$3" | sed -E 's#https://github.com/acme/([^/]+)/pull/.*#\1#')
    sha=$(printf '%s\n' "$*" | sed -E 's#.*--match-head-commit ([0-9a-f]+).*#\1#')
    if [ "$sha" != "$(head_of "$repo")" ]; then printf 'head moved\n' >&2; exit 1; fi
    git --git-dir="$(remote "$repo")" update-ref refs/heads/main "$sha"
    : > "$state/$repo-merged"
    printf 'Merged\n' ;;
  "api graphql") printf '{"data":{}}\n' ;;
  "auth status") exit 0 ;;
  *) printf 'unexpected fake gh command: %s\n' "$*" >&2; exit 90 ;;
esac
`

// TestFullPipelineWorkspaceMergesEveryPRMemberInOneRun proves that merging one
// PR Member does not make the barrier refuse the next one, and that a mixed
// workspace still requires its quick Member's own exact-head review.
func TestFullPipelineWorkspaceMergesEveryPRMemberInOneRun(t *testing.T) {
	f := reviewIdentityFixture(t)
	stopBackgroundWatch(t, f)
	if err := os.WriteFile(filepath.Join(f.root, "bin", "gh"), []byte(workspaceForgeScript), 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(f.root, "stack")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	remotes := map[string]string{}
	for _, member := range []string{"api", "web"} {
		remotes[member] = filepath.Join(f.root, "ws-"+member+".git")
		path := filepath.Join(root, member)
		initRepository(t, path, remotes[member], f.env)
		url := "https://github.com/acme/" + member + ".git"
		gitTest(t, f.env, path, "remote", "set-url", "origin", url)
		gitTest(t, f.env, path, "config", "url.file://"+remotes[member]+".insteadOf", url)
	}
	zeta := filepath.Join(root, "zeta")
	initRepository(t, zeta, filepath.Join(f.root, "ws-zeta.git"), f.env)
	project, err := f.db.CreateWorkspaceProject(context.Background(), "stack", root, []store.ProjectRepo{
		{Name: "api", Path: "api", DefaultBranch: "main", Status: store.RepoActive, OriginHost: "github.com"},
		{Name: "zeta", Path: "zeta", DefaultBranch: "main", Status: store.RepoActive},
		{Name: "web", Path: "web", DefaultBranch: "main", Status: store.RepoActive, OriginHost: "github.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := createWorkspace(herdr.NewWithEnv("herdr", f.env), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetProjectLead(context.Background(), project.ID, workspace.Workspace.WorkspaceID, workspace.RootPane.PaneID, "posse:stack:lead"); err != nil {
		t.Fatal(err)
	}
	leadEnv := setEnv(f.leadEnv, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", workspace.Workspace.WorkspaceID)
	for _, setting := range [][2]string{
		{"defaults.review", "full-pipeline"}, {"autonomy.yolo", "true"},
		{"repositories.api.merge_mode", "pr"}, {"repositories.web.merge_mode", "pr"},
		{"repositories.zeta.merge_mode", "local"}, {"repositories.zeta.review", "quick"},
	} {
		runPosse(t, f.binary, root, f.env, "config", "set", setting[0], setting[1], "--project", "stack")
	}
	brief := filepath.Join(f.root, "two-pr-members.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Two PR Members\ndone_when: checked\nrepos: [api, web, zeta]\n---\nChange every Member.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, root, leadEnv, "start", "--brief", brief, "--name", "two-pr-members", "--profile", "author"); code != 0 {
		t.Fatalf("start workspace Change Task: code=%d %s", code, output)
	}
	change := mustProjectTask(t, f, project.ID, "t1")
	repos, err := f.db.TaskRepos(context.Background(), change.ID)
	if err != nil || len(repos) != 3 {
		t.Fatalf("workspace Members: %+v %v", repos, err)
	}
	worktrees := map[string]string{}
	for _, repo := range repos {
		worktrees[repo.Repo] = repo.WorktreePath
		if err := os.WriteFile(filepath.Join(repo.WorktreePath, "change.txt"), []byte(repo.Repo+" candidate\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, f.env, repo.WorktreePath, "add", "change.txt")
		gitTest(t, f.env, repo.WorktreePath, "commit", "-m", "change "+repo.Repo)
	}
	originMain := map[string]string{}
	for _, member := range []string{"api", "web"} {
		originMain[member] = gitRef(t, f, remotes[member], "refs/heads/main")
	}
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	candidates := map[string]store.ValidationCandidate{}
	for _, member := range []string{"api", "web"} {
		code, output := runReleaseCLI(t, f.binary, worktrees[member], workerEnv,
			"publish", "--repo", member, member+" candidate summary", "--verify", "go test ./... -> passed",
			"--proof", "Intent behavior verified", "--risk", "Risk: low\nRollback: revert the "+member+" change")
		if code != 0 || !strings.Contains(output, "validation required before publication") {
			t.Fatalf("submit %s private candidate: code=%d %s", member, code, output)
		}
		candidate, err := f.db.LatestValidationCandidate(context.Background(), change.ID, member)
		if err != nil {
			t.Fatal(err)
		}
		candidates[member] = candidate
	}
	if code, output := runReleaseCLI(t, f.binary, root, leadEnv, "review", "start", "t1", "--profile", "reviewer"); code != 0 {
		t.Fatalf("start workspace Review Round: code=%d %s", code, output)
	}
	review := mustProjectTask(t, f, project.ID, "t2")
	waitForReviewIdentityAgent(t, f, review.PaneID)
	scratch := filepath.Join(f.home, "scratch", "stack", "t2")
	report := filepath.Join(scratch, "report.md")
	findings := filepath.Join(scratch, "findings.json")
	if err := os.WriteFile(report, []byte("Every Member passed review.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(findings, []byte(`{"schema":"posse.review-findings.v1","patterns":[],"resolutions":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--findings", findings, "--verdict", "pass")
	for _, member := range []string{"api", "web"} {
		recordCandidateStages(t, f, workerEnv, candidates[member], func(string) {}, "--repo", member)
		if code, output := runReleaseCLI(t, f.binary, candidates[member].WorktreePath, workerEnv, "publish", "--repo", member, "Promote"); code != 0 || !strings.Contains(output, "promoted: candidate") {
			t.Fatalf("promote validated %s candidate: code=%d %s", member, code, output)
		}
		if main := gitRef(t, f, remotes[member], "refs/heads/main"); main != originMain[member] {
			t.Fatalf("%s origin main moved on promotion", member)
		}
	}
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "signal", "done", "Every Member validated"); code != 0 {
		t.Fatalf("done Signal: code=%d %s", code, output)
	}

	// One run merges both PR Members: the first merge moves api's origin
	// main, and the barrier still admits web. The local quick Member zeta
	// merges last and fails on a busy target checkout, leaving both PR
	// Members merged on the forge before the PR poll records them.
	reviewedZeta := gitRef(t, f, worktrees["zeta"], "HEAD")
	zetaTarget := gitRef(t, f, zeta, "refs/heads/main")
	busy := filepath.Join(zeta, "busy.txt")
	if err := os.WriteFile(busy, []byte("uncommitted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, root, leadEnv, "merge", "t1", "--execute", "--user-approved", "Merge every Member"); code == 0 || !strings.Contains(output, "merge_target_busy") {
		t.Fatalf("first Merge run: code=%d %s", code, output)
	}
	for _, member := range []string{"api", "web"} {
		if main := gitRef(t, f, remotes[member], "refs/heads/main"); main != candidates[member].CandidateSHA {
			t.Fatalf("%s origin main=%s after one Merge run, want validated candidate %s", member, main, candidates[member].CandidateSHA)
		}
	}
	if err := os.Remove(busy); err != nil {
		t.Fatal(err)
	}

	// With every full-pipeline Member integrated, the quick Member still
	// needs its own exact-head review.
	if err := os.WriteFile(filepath.Join(worktrees["zeta"], "late.txt"), []byte("unreviewed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, worktrees["zeta"], "add", "late.txt")
	gitTest(t, f.env, worktrees["zeta"], "commit", "-m", "unreviewed quick edit")
	if code, output := runReleaseCLI(t, f.binary, root, leadEnv, "merge", "t1", "--execute", "--user-approved", "Merge every Member"); code == 0 || !strings.Contains(output, "review_required") {
		t.Fatalf("Merge with an unreviewed quick Member: code=%d %s", code, output)
	}
	if main := gitRef(t, f, zeta, "refs/heads/main"); main != zetaTarget {
		t.Fatalf("unreviewed zeta head merged: target=%s", main)
	}
	gitTest(t, f.env, worktrees["zeta"], "reset", "-q", "--hard", reviewedZeta)
	if code, output := runReleaseCLI(t, f.binary, root, leadEnv, "merge", "t1", "--execute", "--user-approved", "Merge every Member"); code != 0 {
		t.Fatalf("Merge the reviewed quick Member: code=%d %s", code, output)
	}
	if main := gitRef(t, f, zeta, "refs/heads/main"); main != reviewedZeta {
		t.Fatalf("zeta target=%s after Merge, want reviewed head %s", main, reviewedZeta)
	}
}
