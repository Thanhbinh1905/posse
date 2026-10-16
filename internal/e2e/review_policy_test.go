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

func signalReviewPolicyTask(t *testing.T, f *prLifecycleFixture, task store.Task, args ...string) string {
	t.Helper()
	env := setEnv(f.env, "HERDR_PANE_ID", task.PaneID)
	env = setEnv(env, "POSSE_WORKER_HOME", f.home)
	output := runPosse(t, f.binary, task.WorktreePath, env, append([]string{"holler", "done", "Checked"}, args...)...)
	// The identity fixture reports working after its launch prompt and then waits
	// for input. Complete that fake turn too, so reconciliation does not correctly
	// interpret its permanently-working pane as resumed work.
	if _, err := herdr.NewWithEnv("herdr", f.env).Run(context.Background(), "pane", "report-agent", task.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	return output
}

func TestSelectedReviewPolicyLocalLand(t *testing.T) {
	f := reviewIdentityFixture(t)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", "local")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "autonomy.yolo", "true", "--project", "shop")
	if code, out := rideReviewIdentityTask(t, f, "Selected policy", "author", ""); code != 0 {
		t.Fatalf("ride: %d %s", code, out)
	}
	ship := f.mustTask(t, "t1")
	gitTest(t, f.env, ship.WorktreePath, "commit", "--allow-empty", "-m", "candidate")
	signalReviewPolicyTask(t, f, ship)
	before := gitTest(t, f.env, f.repo, "rev-parse", "main")
	for _, args := range [][]string{{"land", "t1"}, {"land", "t1", "--merge"}} {
		code, out := runReviewIdentityCLI(t, f, args...)
		if code != 1 || !strings.Contains(out, "review_required") {
			t.Fatalf("missing pass admitted: %d %s", code, out)
		}
	}
	if code, out := rideReviewIdentityTask(t, f, "Independent verdict", "reviewer", "t1"); code != 0 {
		t.Fatalf("review: %d %s", code, out)
	}
	review := f.mustTask(t, "t2")
	report := filepath.Join(f.root, "posse", "scratch", "shop", "t2", "report.md")
	if err := os.WriteFile(report, []byte("No findings.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--verdict", "pass")
	if code, out := runReviewIdentityCLI(t, f, "land", "t1"); code != 0 {
		t.Fatalf("current pass refused: %d %s", code, out)
	}
	gitTest(t, f.env, ship.WorktreePath, "commit", "--allow-empty", "-m", "unreviewed follow-up")
	code, out := runReviewIdentityCLI(t, f, "land", "t1", "--merge")
	if code != 1 || !strings.Contains(out, "review_required") {
		t.Fatalf("stale pass admitted: %d %s", code, out)
	}
	if got := gitTest(t, f.env, f.repo, "rev-parse", "main"); got != before {
		t.Fatal("refused Land changed main")
	}
	if code, out := rideReviewIdentityTask(t, f, "Current verdict", "reviewer", "t1"); code != 0 {
		t.Fatalf("second review: %d %s", code, out)
	}
	review = f.mustTask(t, "t3")
	report = filepath.Join(f.home, "scratch", "shop", "t3", "report.md")
	if err := os.WriteFile(report, []byte("Current candidate passes.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--verdict", "pass")
	// The new pass is current, but the Gate must run again for the changed branch.
	code, out = runReviewIdentityCLI(t, f, "land", "t1", "--merge")
	if code != 1 || !strings.Contains(out, "branch_moved") {
		t.Fatalf("missing re-gate: %d %s", code, out)
	}
	if code, out = runReviewIdentityCLI(t, f, "land", "t1", "--merge"); code != 0 {
		t.Fatalf("current pass merge: %d %s", code, out)
	}
	if got := strings.TrimSpace(gitTest(t, f.env, f.repo, "rev-parse", "main")); got != strings.TrimSpace(gitTest(t, f.env, f.repo, "rev-parse", "refs/heads/"+ship.Branch)) {
		t.Fatal("merge did not land the reviewed candidate")
	}
}

func TestSelectedReviewPolicyNoneKeepsGateAndMergeAuthority(t *testing.T) {
	f := reviewIdentityFixture(t)
	// No explicit review setting: the built-in policy is none.
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.gate", `["exit 1"]`)
	if code, out := rideReviewIdentityTask(t, f, "No independent review", "author", ""); code != 0 {
		t.Fatalf("ride: %d %s", code, out)
	}
	ship := f.mustTask(t, "t1")
	gitTest(t, f.env, ship.WorktreePath, "commit", "--allow-empty", "-m", "candidate")
	signalReviewPolicyTask(t, f, ship)
	before := gitTest(t, f.env, f.repo, "rev-parse", "main")
	code, out := runReviewIdentityCLI(t, f, "land", "t1", "--merge")
	if code != 1 || !strings.Contains(out, "land_approval_required") {
		t.Fatalf("authority bypassed: %d %s", code, out)
	}
	code, out = runReviewIdentityCLI(t, f, "land", "t1")
	if code != 1 || !strings.Contains(out, "gate_failed") {
		t.Fatalf("Gate bypassed: %d %s", code, out)
	}
	if got := gitTest(t, f.env, f.repo, "rev-parse", "main"); got != before {
		t.Fatal("refusal changed target")
	}
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.gate", `["true"]`)
	if code, out = runReviewIdentityCLI(t, f, "land", "t1", "--merge", "--user-approved", "Land this checked Task"); code != 0 {
		t.Fatalf("none refused: %d %s", code, out)
	}
}

func TestSelectedReviewPolicyNoMistakesFailsClosed(t *testing.T) {
	f := reviewIdentityFixture(t)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", "no-mistakes")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "autonomy.yolo", "true", "--project", "shop")
	if code, out := rideReviewIdentityTask(t, f, "Pipeline receipt", "author", ""); code != 0 {
		t.Fatalf("ride: %d %s", code, out)
	}
	ship := f.mustTask(t, "t1")
	gitTest(t, f.env, ship.WorktreePath, "commit", "--allow-empty", "-m", "candidate")
	signalReviewPolicyTask(t, f, ship)
	before := gitTest(t, f.env, f.repo, "rev-parse", "main")
	for _, args := range [][]string{{"land", "t1"}, {"land", "t1", "--merge"}} {
		code, out := runReviewIdentityCLI(t, f, args...)
		if code != 1 || !strings.Contains(out, "review_receipt_missing") {
			t.Fatalf("missing receipt admitted: %d %s", code, out)
		}
	}
	if got := gitTest(t, f.env, f.repo, "rev-parse", "main"); got != before {
		t.Fatal("receipt refusal changed target")
	}
}

func TestSelectedReviewPolicyConfigNeedsUserConsent(t *testing.T) {
	f := reviewIdentityFixture(t)
	for _, key := range []string{"defaults.review", "repositories.api.review"} {
		for _, args := range [][]string{{"config", "set", key, "local", "--project", "shop"}, {"config", "unset", key, "--project", "shop"}} {
			code, out := runReviewIdentityCLI(t, f, args...)
			if code != 1 || !strings.Contains(out, "user_only") {
				t.Fatalf("unapproved policy write: %d %s", code, out)
			}
		}
		const quote = "  Use local independent review.  "
		code, out := runReviewIdentityCLI(t, f, "config", "set", key, "local", "--project", "shop", "--user-approved", quote)
		if code != 0 {
			t.Fatalf("consented policy: %d %s", code, out)
		}
		var recorded string
		if err := f.db.QueryRowContext(context.Background(), "SELECT user_quote FROM config_approvals WHERE key=? ORDER BY id DESC LIMIT 1", key).Scan(&recorded); err != nil || recorded != quote {
			t.Fatalf("consent lost: %q %v", recorded, err)
		}
	}
}

func TestSelectedReviewPolicyWorkspaceAdmission(t *testing.T) {
	f := reviewIdentityFixture(t)
	root := filepath.Join(f.root, "policy-stack")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, root, "clone", filepath.Join(f.root, "origin.git"), filepath.Join(root, "api"))
	project, err := f.db.CreateWorkspaceProject(context.Background(), "policy-stack", root, []store.ProjectRepo{{Name: "api", Path: "api", DefaultBranch: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := createWorkspace(herdr.NewWithEnv("herdr", f.env), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetProjectLead(context.Background(), project.ID, workspace.Workspace.WorkspaceID, workspace.RootPane.PaneID, "posse:policy-stack:lead"); err != nil {
		t.Fatal(err)
	}
	leadEnv := setEnv(f.env, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	for _, setting := range [][2]string{{"defaults.landing_mode", "local"}, {"defaults.review", "no-mistakes"}, {"repositories.api.review", "none"}} {
		runPosse(t, f.binary, root, f.env, "config", "set", setting[0], setting[1], "--project", "policy-stack")
	}
	brief := filepath.Join(f.root, "member-policy.md")
	writeBrief := func(fields string) {
		t.Helper()
		title := "Member policy"
		if fields != "" {
			title = "Lighter member"
		}
		if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: "+title+"\ndone_when: checked\nrepos: [api]\n"+fields+"---\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeBrief("")
	runPosse(t, f.binary, root, leadEnv, "ride", "--brief", brief, "--name", "member-policy", "--profile", "author")
	tasks, err := f.db.Tasks(context.Background(), project.ID, true)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("workspace Task: %#v %v", tasks, err)
	}
	plan, err := tasks[0].AdmittedPlan()
	if err != nil || plan == nil || len(plan.Execution.Members) != 1 || plan.Execution.Members[0].Review != "none" || plan.Execution.Members[0].ReviewSource != "repositories.api.review" {
		t.Fatalf("member override not admitted: %#v %v", plan, err)
	}
	// A stricter live override does not rewrite the already admitted policy.
	runPosse(t, f.binary, root, f.env, "config", "set", "repositories.api.review", "local", "--project", "policy-stack")
	writeBrief("review: none\n")
	cmd := exec.Command(f.binary, "ride", "--brief", brief, "--name", "lighter-member", "--profile", "author")
	cmd.Dir, cmd.Env = root, leadEnv
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "scoped User choice") {
		t.Fatalf("unapproved Member override: %v %s", err, out)
	}
	const quote = "  Use no independent review for this Member Task.  "
	runPosse(t, f.binary, root, leadEnv, "ride", "--brief", brief, "--name", "lighter-member", "--profile", "author", "--user-approved", quote)
	tasks, err = f.db.Tasks(context.Background(), project.ID, true)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("scoped workspace Task: %#v %v", tasks, err)
	}
	for _, task := range tasks {
		plan, err := task.AdmittedPlan()
		if err != nil || plan.Execution.Members[0].Review != "none" {
			t.Fatalf("Member policy changed: %#v %v", plan, err)
		}
		if task.ShortName == "lighter-member" && (plan.UserChoice == nil || plan.UserChoice.Quote != quote || task.AutonomyLand != "ask") {
			t.Fatalf("Member consent lost or escalated: %#v", plan)
		}
	}
}

func TestReviewPolicyLegacyConfigMigration(t *testing.T) {
	f := reviewIdentityFixture(t)
	for _, value := range []string{"always", "on_risk", "never"} {
		code, out := runReviewIdentityCLI(t, f, "config", "set", "defaults.review", value, "--user-approved", "Select review")
		if code != 1 || !strings.Contains(out, "legacy review policy") || !strings.Contains(out, "none, local or no-mistakes") {
			t.Errorf("migration %s: %d %s", value, code, out)
		}
	}
}
