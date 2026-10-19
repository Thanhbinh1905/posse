//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestTaskPreferencesRequireScopedUserChoice(t *testing.T) {
	f := reviewIdentityFixture(t)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", "local")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.merge_mode", "pr")
	brief := filepath.Join(f.root, "choice.md")
	text := "---\ntype: change\ntitle: Scoped choice\ndone_when: checked\nmerge_mode: local\nreview: none\n---\nKeep the scope small.\n"
	if err := os.WriteFile(brief, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output := runReviewIdentityCLI(t, f, "start", "--brief", brief, "--name", "scoped-choice", "--profile", "author")
	if code != 1 || !strings.Contains(output, "brief_invalid") {
		t.Fatalf("unapproved preference: %d %s", code, output)
	}
	tasks, err := f.db.Tasks(context.Background(), f.project.ID, true)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("refusal created Tasks: %#v %v", tasks, err)
	}
	const quote = "  Use local delivery and no review for this Task only.  "
	code, output = runReviewIdentityCLI(t, f, "start", "--brief", brief, "--name", "scoped-choice", "--profile", "author", "--user-approved", quote)
	if code != 0 {
		t.Fatalf("scoped User choice refused: %d %s", code, output)
	}
	task := f.mustTask(t, "t1")
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil || plan.Delivery != "local" || plan.Review != "none" || !strings.Contains(task.PlanJSON, quote) {
		t.Fatalf("choice/provenance lost: %s %v", task.PlanJSON, err)
	}
	if task.AutonomyMerge != "ask" || task.AutonomyReview != "ask" {
		t.Fatalf("preference escalated grants: %#v", task)
	}
}

func TestTaskPreferenceRefusalsHaveNoTaskEffects(t *testing.T) {
	f := reviewIdentityFixture(t)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", "local")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.merge_mode", "pr")
	client := herdr.NewWithEnv("herdr", f.env)
	before, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	branches := gitTest(t, f.env, f.repo, "for-each-ref", "--format=%(refname) %(objectname)")
	for _, tc := range []struct {
		name, fields string
		approval     []string
	}{
		{"lighter delivery", "merge_mode: local\n", nil},
		{"lighter review", "review: none\n", nil},
		{"blank quote", "merge_mode: local\n", []string{"--user-approved", " "}},
		{"unscoped quote", "", []string{"--user-approved", "Unscoped consent"}},
		{"merge escalation", "merge_mode: local\nautonomy: { merge: auto }\n", []string{"--user-approved", "Use local delivery"}},
		{"decision escalation", "review: none\nautonomy: { review: lead }\n", []string{"--user-approved", "Skip review"}},
		{"executable escalation", "gate: [evil]\n", []string{"--user-approved", "Skip review"}},
		{"discard escalation", "discard: true\n", []string{"--user-approved", "Skip review"}},
		{"repo Members", "repos: [api]\n", nil},
		{"repo branch map", "target: {api: main}\n", nil},
		{"missing branch", "base: main\ntarget: missing\n", nil},
		{"unsupported execution", "execution: in-place\n", nil},
		{"uninitialized no-mistakes", "merge_mode: no-mistakes\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brief := filepath.Join(f.root, "refused.md")
			text := "---\ntype: change\ntitle: Refused choice\ndone_when: checked\n" + tc.fields + "---\nIntent stays unchanged.\n"
			if err := os.WriteFile(brief, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"start", "--brief", brief, "--name", "refused-choice", "--profile", "author"}, tc.approval...)
			if code, output := runReviewIdentityCLI(t, f, args...); code == 0 {
				t.Fatalf("incompatible choice admitted: %s", output)
			}
			tasks, err := f.db.Tasks(context.Background(), f.project.ID, true)
			if err != nil || len(tasks) != 0 {
				t.Fatalf("refusal created Tasks: %#v %v", tasks, err)
			}
			intents, err := f.db.Intents(context.Background(), f.project.ID)
			if err != nil || len(intents) != 0 {
				t.Fatalf("refusal created intents: %#v %v", intents, err)
			}
			sandboxes, err := f.db.Sandboxes(context.Background(), f.project.ID)
			if err != nil || len(sandboxes) != 0 {
				t.Fatalf("refusal allocated Sandboxes: %#v %v", sandboxes, err)
			}
			if got := gitTest(t, f.env, f.repo, "for-each-ref", "--format=%(refname) %(objectname)"); got != branches {
				t.Fatalf("refusal changed Git refs: %s", got)
			}
			if _, err := os.Stat(filepath.Join(f.home, "scratch", "shop", "t1")); !os.IsNotExist(err) {
				t.Fatalf("refusal created scratch: %v", err)
			}
			after, err := client.Snapshot(context.Background())
			if err != nil || len(after.Panes) != len(before.Panes) || len(after.Workspaces) != len(before.Workspaces) {
				t.Fatalf("refusal allocated panes/workspaces: %#v %v", after, err)
			}
		})
	}
	gitTest(t, f.env, f.repo, "remote", "remove", "origin")
	brief := filepath.Join(f.root, "no-origin.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: No origin\ndone_when: checked\nmerge_mode: pr\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runReviewIdentityCLI(t, f, "start", "--brief", brief, "--name", "no-origin", "--profile", "author"); code != 1 || !strings.Contains(output, "without origin") {
		t.Fatalf("PR without Git remote: %d %s", code, output)
	}
}

func TestAuthorizedFeatureBranchChoiceKeepsDefaultAndSurvivesRestart(t *testing.T) {
	f := reviewIdentityFixture(t)
	gitTest(t, f.env, f.repo, "checkout", "-b", "release/next")
	if err := os.WriteFile(filepath.Join(f.repo, "release.txt"), []byte("release base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, f.repo, "add", "release.txt")
	gitTest(t, f.env, f.repo, "commit", "-m", "release base")
	gitTest(t, f.env, f.repo, "push", "origin", "release/next")
	base := strings.TrimSpace(gitTest(t, f.env, f.repo, "rev-parse", "HEAD"))
	gitTest(t, f.env, f.repo, "checkout", "main")
	main := gitTest(t, f.env, f.repo, "rev-parse", "main")
	brief := filepath.Join(f.root, "feature.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Release choice\ndone_when: checked\nbase: release/next\ntarget: release/next\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runReviewIdentityCLI(t, f, "start", "--brief", brief, "--name", "release-choice", "--profile", "author", "--user-approved", "Use the release branch for this Task"); code != 0 {
		t.Fatalf("feature branch choice: %d %s", code, output)
	}
	task := f.mustTask(t, "t1")
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil || plan.Execution.BaseCommit != base || plan.Execution.Target != "release/next" {
		t.Fatalf("branch not pinned: %#v %v", plan, err)
	}
	if got := strings.TrimSpace(gitTest(t, f.env, task.WorktreePath, "rev-parse", "HEAD")); got != base {
		t.Fatalf("Task base=%s want %s", got, base)
	}
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.target_branch", "main", "--project", "shop")
	if code, output := runReviewIdentityCLI(t, f, "relaunch", "t1"); code != 0 {
		t.Fatalf("relaunch: %d %s", code, output)
	}
	restarted := f.mustTask(t, "t1")
	if restarted.PlanJSON != task.PlanJSON || restarted.TargetBranch != "release/next" || gitTest(t, f.env, f.repo, "rev-parse", "main") != main || strings.TrimSpace(gitTest(t, f.env, f.repo, "branch", "--show-current")) != "main" {
		t.Fatal("restart changed selected plan or Project default branch")
	}
	preview := runPosse(t, f.binary, f.repo, f.env, "config", "show", "--project", "shop", "--effective", "--json")
	var view struct {
		Sources map[string][]string `json:"sources"`
	}
	if err := json.Unmarshal([]byte(preview), &view); err != nil || !reflect.DeepEqual(view.Sources["defaults.target_branch"], []string{filepath.Join(f.home, "projects", "shop", "config.toml")}) {
		t.Fatalf("effective preview lacks sources: %s %v", preview, err)
	}
}

func TestWorkspacePreferenceCompatibilityPrecedesBranchEffects(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	root := filepath.Join(f.root, "stack")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	withOrigin := filepath.Join(root, "api")
	gitTest(t, f.env, root, "clone", filepath.Join(f.root, "origin.git"), withOrigin)
	local := filepath.Join(root, "tool")
	if err := os.MkdirAll(local, 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, local, "init", "-b", "main")
	gitTest(t, f.env, local, "commit", "--allow-empty", "-m", "initial")
	project, err := f.db.CreateWorkspaceProject(context.Background(), "stack", root, []store.ProjectRepo{{Name: "api", Path: "api", DefaultBranch: "main"}, {Name: "tool", Path: "tool", DefaultBranch: "main"}})
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
	env := setEnv(f.env, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	for _, fields := range []string{
		"repos: [api, tool]\nmerge_mode: pr\nbase: main\n",
		"repos: [api]\nmerge_mode: no-mistakes\n",
		"repos: [api]\ntarget: {tool: main}\n",
		"repos: [missing]\n",
		"merge_mode: local\n",
	} {
		brief := filepath.Join(f.root, "workspace-refused.md")
		if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Workspace refusal\ndone_when: checked\n"+fields+"---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(f.binary, "start", "--brief", brief, "--name", "workspace-refusal", "--user-approved", "Use this workflow for this Task")
		cmd.Env, cmd.Dir = env, root
		if output, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("incompatible Member choice admitted: %s", output)
		} else {
			t.Logf("refusal: %s", output)
		}
		tasks, err := f.db.Tasks(context.Background(), project.ID, true)
		if err != nil || len(tasks) != 0 {
			t.Fatalf("refusal created Tasks: %#v %v", tasks, err)
		}
		if _, err := os.Stat(filepath.Join(withOrigin, ".git", "FETCH_HEAD")); !os.IsNotExist(err) {
			t.Fatalf("refusal fetched before Member compatibility checks: %v", err)
		}
	}
}

func TestPermissionTemplatesRequireConsentThroughCLI(t *testing.T) {
	f := reviewIdentityFixture(t)
	configPath := filepath.Join(f.home, "projects", "shop", "config.toml")
	beforeConfig, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, setting := range [][2]string{
		{"kinds.claude.auto_approve_args", `["--dangerously-skip-permissions"]`},
		{"kinds.claude.prepare", "claude-trust"},
		{"profiles.deep.args", `["--dangerously-skip-permissions"]`},
	} {
		code, output := runReviewIdentityCLI(t, f, "config", "set", setting[0], setting[1], "--project", "shop")
		if code != 1 || !strings.Contains(output, "user_only") {
			t.Errorf("permission template accepted without consent: %s: code=%d %s", setting[0], code, output)
		}
	}
	afterConfig, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) || string(beforeConfig) != string(afterConfig) {
		t.Fatalf("refused config writes changed bytes: %v", err)
	}
	// A native Lead child retains its registered actor through its parent,
	// even when the immediate command omits the pane variable.
	quote := "  Prepare Claude trust for this Project.  "
	child := exec.Command("sh", "-c", "env -u HERDR_PANE_ID \"$@\"; result=$?; exit \"$result\"", "lead-child", f.binary, "config", "set", "kinds.claude.prepare", "claude-trust", "--project", "shop", "--user-approved", quote)
	child.Env, child.Dir = f.leadEnv, f.repo
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("registered Lead child: %v %s", err, output)
	}
	var recorded string
	if err := f.db.QueryRowContext(context.Background(), "SELECT user_quote FROM config_approvals ORDER BY id DESC LIMIT 1").Scan(&recorded); err != nil || recorded != quote {
		t.Fatalf("child consent record=%q err=%v", recorded, err)
	}
	unbound := exec.Command(f.binary, "config", "set", "kinds.claude.prepare", "", "--project", "shop", "--user-approved", "claimed consent")
	unbound.Env, unbound.Dir = setEnv(f.env, "PI_CODING_AGENT", "1"), f.repo
	if output, err := unbound.CombinedOutput(); err == nil || !strings.Contains(string(output), "user_only") {
		t.Fatalf("unbound agent treated as human: %v %s", err, output)
	}
	client := herdr.NewWithEnv("herdr", f.env)
	shell, err := createWorkspace(client, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	shellEnv := setEnv(f.env, "HERDR_PANE_ID", shell.RootPane.PaneID)
	runPosse(t, f.binary, f.repo, shellEnv, "config", "set", "kinds.claude.prepare", "claude-trust", "--project", "shop")
	if _, err := client.Run(context.Background(), "pane", "report-agent", shell.RootPane.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	native := exec.Command(f.binary, "config", "set", "kinds.claude.prepare", "claude-trust", "--project", "shop", "--user-approved", "claimed consent")
	native.Env, native.Dir = shellEnv, f.repo
	if output, err := native.CombinedOutput(); err == nil || !strings.Contains(string(output), "user_only") {
		t.Fatalf("unbound native agent treated as human: %v %s", err, output)
	}
	if code, output := rideReviewIdentityTask(t, f, "Native actor", "author", ""); code != 0 {
		t.Fatalf("native Worker: %d %s", code, output)
	}
	task := f.mustTask(t, "t1")
	worker := exec.Command("sh", "-c", "env -u HERDR_PANE_ID \"$@\"; result=$?; exit \"$result\"", "worker-child", f.binary, "config", "set", "kinds.claude.prepare", "", "--project", "shop", "--user-approved", "claimed consent")
	worker.Env, worker.Dir = setEnv(f.env, "HERDR_PANE_ID", task.PaneID), f.repo
	if output, err := worker.CombinedOutput(); err == nil || !strings.Contains(string(output), "worker_forbidden") {
		t.Fatalf("registered Worker child escaped identity: %v %s", err, output)
	}
}
