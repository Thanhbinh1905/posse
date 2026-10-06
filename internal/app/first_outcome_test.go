package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type firstOutcomeFixture struct {
	root, home, repo, brief string
	service                 *Service
	fake                    *herdr.Fake
	db                      *store.DB
	project                 store.Project
}

func newFirstOutcomeFixture(t *testing.T) firstOutcomeFixture {
	t.Helper()
	root := t.TempDir()
	home, repo := filepath.Join(root, "posse"), filepath.Join(root, "repo")
	initRepo(t, repo)
	setupFollowupAgentPath(t, "pi")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	t.Setenv("POSSE_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(root, "pi"))
	if err := os.MkdirAll(filepath.Join(root, "pi"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByID(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{
		Workspaces: []herdr.Workspace{{WorkspaceID: "w1", Label: "Lead:shop"}},
		Panes:      []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: project.LeadLabel, Agent: "pi", AgentStatus: "idle", Focused: true}},
		Agents:     []herdr.Agent{{PaneID: "w1:p1", Kind: "pi"}},
	}
	fake.Results["server.agent_manifests"] = json.RawMessage(`{"agents":[{"kind":"pi","available":true}]}`)
	fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	fake.RunOut["plugin list --json"] = []byte(`{"plugins":[]}`)
	fake.RunOut["integration status"] = []byte("pi: installed\n")
	fake.RunOut["plugin log list --plugin posse.herdr"] = []byte(`{}`)
	brief := filepath.Join(root, "brief.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: First outcome\ndone_when: verified\n---\nMake a change.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	return firstOutcomeFixture{root: root, home: home, repo: repo, brief: brief, service: testService(home, fake), fake: fake, db: db, project: project}
}

func outcomeCLI(t *testing.T, service *Service, wantCode int, args ...string) string {
	t.Helper()
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run(args); code != wantCode {
		t.Fatalf("%v exit=%d want=%d:\n%s", args, code, wantCode, &output)
	}
	return output.String()
}

func TestFirstOutcomeUpKindSelection(t *testing.T) {
	for _, tc := range []struct {
		name, manifests, flag, kind, source string
		code                                int
	}{
		{name: "sole", manifests: `{"agents":[{"kind":"pi","available":true}]}`, kind: "pi", source: "only available kind"},
		{name: "PATH fallback", manifests: `{}`, kind: "pi", source: "only available kind"},
		{name: "several", manifests: `{"agents":[{"kind":"pi"},{"kind":"codex"}]}`, code: 1},
		{name: "flag wins", manifests: `{"agents":[{"kind":"pi"},{"kind":"codex"}]}`, flag: "--pi", kind: "pi", source: "flag"},
		{name: "none", manifests: `{"agents":[]}`, code: 1},
		{name: "configured wins", manifests: `{"agents":[{"kind":"pi"},{"kind":"codex"}]}`, kind: "pi", source: "lead.kind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFirstOutcomeFixture(t)
			if tc.name == "none" {
				setupFollowupAgentPath(t)
			} else if tc.name != "sole" && tc.name != "PATH fallback" {
				setupFollowupAgentPath(t, "pi", "codex")
			}
			if tc.name == "configured wins" {
				outcomeCLI(t, f.service, 0, "config", "set", "lead.kind", "pi")
			}
			before, _ := os.ReadFile(filepath.Join(f.home, "config.toml"))
			f.fake.Results["server.agent_manifests"] = json.RawMessage(tc.manifests)
			f.fake.SnapshotValue.Panes[0].Agent = ""
			f.fake.SnapshotValue.Agents = nil
			t.Setenv("HERDR_ENV", "1")
			t.Setenv("HERDR_PANE_ID", "w1:p1")
			t.Setenv("HERDR_WORKSPACE_ID", "w1")
			args := []string{"up", "--yes", "--json"}
			if tc.flag != "" {
				args = append(args, tc.flag)
			}
			output := outcomeCLI(t, f.service, tc.code, args...)
			if tc.code != 0 {
				if !strings.Contains(output, "lead_kind_required") {
					t.Fatal(output)
				}
				if tc.name == "several" && (!strings.Contains(output, "pi") || !strings.Contains(output, "codex")) {
					t.Fatal(output)
				}
				if f.service.pendingLead != nil {
					t.Fatal("ambiguous up scheduled a Lead")
				}
			} else {
				if !strings.Contains(output, tc.source) || f.service.pendingLead == nil || f.service.pendingLead.Kind != tc.kind {
					t.Fatalf("selection: %s %#v", output, f.service.pendingLead)
				}
				for _, code := range []string{"gate_empty", "autonomy_ask", "machine_setup"} {
					if !strings.Contains(output, code) {
						t.Fatalf("up missing %s: %s", code, output)
					}
				}
				instructions, err := os.ReadFile(filepath.Join(f.home, "projects", "shop", "lead.md"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(instructions), "gate_empty") {
					t.Fatalf("startup readiness absent: %s", instructions)
				}
			}
			after, _ := os.ReadFile(filepath.Join(f.home, "config.toml"))
			if !bytes.Equal(before, after) {
				t.Fatalf("up persisted its selection: %s", after)
			}
		})
	}
}

func TestFirstOutcomeConfiguredCursorUsesMappedCLI(t *testing.T) {
	f := newFirstOutcomeFixture(t)
	setupFollowupAgentPath(t, "cursor-agent")
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("[kinds.cursor]\nnotice_delivery='prompt'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.fake.Results["server.agent_manifests"] = json.RawMessage(`{}`)
	f.fake.SnapshotValue.Panes[0].Agent = ""
	f.fake.SnapshotValue.Agents = nil
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	output := outcomeCLI(t, f.service, 0, "up", "--yes", "--json")
	if f.service.pendingLead == nil || f.service.pendingLead.Kind != "cursor" || !strings.Contains(output, "only available kind") {
		t.Fatalf("selection: %s %#v", output, f.service.pendingLead)
	}
}

func TestFirstOutcomeUpReportsNoMistakesReadinessOnRefusal(t *testing.T) {
	f := newFirstOutcomeFixture(t)
	bin := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	if err := os.WriteFile(filepath.Join(bin, "no-mistakes"), []byte("#!/bin/sh\necho 'error: repo not initialized (run no-mistakes init first)'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	projectConfig := filepath.Join(f.home, "projects", f.project.Name, "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfig, []byte("[defaults]\nlanding_mode='no-mistakes'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	output := outcomeCLI(t, f.service, 1, "up", "--pi", "--json")
	for _, want := range []string{"no_mistakes_uninitialized", "Readiness gap: no_mistakes_uninitialized", "no-mistakes init"} {
		if !strings.Contains(output, want) {
			t.Fatalf("up omitted %q: %s", want, output)
		}
	}
}

func TestFirstOutcomeDispatchPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, config, profile, source, expectedProfile string
		code                                           int
	}{
		{name: "implicit", source: "implicit: lead kind", expectedProfile: "implicit:pi"},
		{name: "named Profiles without rules", config: "[profiles.deep]\nkind='codex'\n", source: "implicit: lead kind", expectedProfile: "implicit:pi"},
		{name: "live Lead overrides default Lead", config: "[lead]\nkind='codex'\n", source: "implicit: lead kind", expectedProfile: "implicit:pi"},
		{name: "default", config: "[profiles.deep]\nkind='codex'\n[dispatch.default]\nuse='deep'\n", source: "dispatch.default", expectedProfile: "deep"},
		{name: "single rule", config: "[profiles.deep]\nkind='codex'\n[[dispatch]]\ntype='ship'\nuse='deep'\n", source: "type=ship", expectedProfile: "deep"},
		{name: "explicit", config: "[profiles.deep]\nkind='codex'\n", profile: "deep", source: "--profile deep", expectedProfile: "deep"},
		{name: "ambiguous", config: "[profiles.deep]\nkind='codex'\n[[dispatch]]\nwhen='hard'\nuse='deep'\n[[dispatch]]\nwhen='easy'\nuse='deep'\n[dispatch.default]\nuse='deep'\n", code: 1},
		{name: "unmatched configured rule", config: "[profiles.deep]\nkind='codex'\n[[dispatch]]\ntype='review'\nuse='deep'\n", code: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFirstOutcomeFixture(t)
			if tc.config != "" {
				if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(tc.config), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"dispatch", "--brief", f.brief, "--json"}
			if tc.profile != "" {
				args = append(args, "--profile", tc.profile)
			}
			output := outcomeCLI(t, f.service, tc.code, args...)
			if tc.code != 0 {
				if !strings.Contains(output, "profile_required") {
					t.Fatal(output)
				}
				return
			}
			if !strings.Contains(output, tc.source) || !strings.Contains(output, `"profile":"`+tc.expectedProfile+`"`) {
				t.Fatal(output)
			}
		})
	}
}

func TestFirstOutcomeExplicitProfilesFollowTypedDispatchRules(t *testing.T) {
	configText := `[profiles.pi-luna]
kind='pi'
[profiles.pi-sol]
kind='pi'
[profiles.claude-sonnet]
kind='claude'
[profiles.claude-opus]
kind='claude'
[profiles.codex-sol]
kind='codex'
[[dispatch]]
type='scout'
use='pi-luna'
[[dispatch]]
type='scout'
when='science-heavy research'
use='pi-sol'
[[dispatch]]
type='scout'
when='exceptionally technical science research or pi-sol already failed'
use='claude-opus'
[[dispatch]]
type='scout'
when='plan or design; pi-sol by default'
use='pi-sol'
[[dispatch]]
type='scout'
when='exceptionally technical plan or design, or pi-sol already failed'
use='claude-opus'
[[dispatch]]
type='ship'
when='plan or design; pi-sol by default'
use='pi-sol'
[[dispatch]]
type='ship'
when='exceptionally technical plan or design, or pi-sol already failed'
use='claude-opus'
[[dispatch]]
type='ship'
when='clear-scope implementation; choose based on load and fit'
use='pi-luna'
[[dispatch]]
type='ship'
when='clear-scope implementation; choose based on load and fit'
use='claude-sonnet'
[[dispatch]]
type='review'
when='validation; choose based on load and fit'
use='pi-luna'
[[dispatch]]
type='review'
when='validation; choose based on load and fit'
use='claude-sonnet'
[[dispatch]]
when='image work'
use='codex-sol'
[[dispatch]]
when='simple, well-known bug fix'
use='pi-luna'
`
	for _, tc := range []struct {
		name, taskType, profile, expected string
	}{
		{name: "type default", taskType: "scout", expected: "pi-luna"},
		{name: "conditional selection", taskType: "scout", profile: "pi-sol", expected: "pi-sol"},
		{name: "alternate explicit selection", taskType: "scout", profile: "claude-opus", expected: "claude-opus"},
		{name: "ship conditional selection", taskType: "ship", profile: "pi-sol", expected: "pi-sol"},
		{name: "ship alternate selection", taskType: "ship", profile: "claude-opus", expected: "claude-opus"},
		{name: "ship Profile choice one", taskType: "ship", profile: "pi-luna", expected: "pi-luna"},
		{name: "ship Profile choice two", taskType: "ship", profile: "claude-sonnet", expected: "claude-sonnet"},
		{name: "review Profile choice one", taskType: "review", profile: "pi-luna", expected: "pi-luna"},
		{name: "review Profile choice two", taskType: "review", profile: "claude-sonnet", expected: "claude-sonnet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFirstOutcomeFixture(t)
			if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(configText), 0o600); err != nil {
				t.Fatal(err)
			}
			brief := "---\ntype: " + tc.taskType + "\ntitle: Typed dispatch test\ndone_when: the selected Profile is returned\n"
			if tc.taskType == "review" {
				brief += "review_of: t1\n"
			}
			brief += "---\nRun this Task.\n"
			if err := os.WriteFile(f.brief, []byte(brief), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"dispatch", "--brief", f.brief, "--json"}
			if tc.profile != "" {
				args = append(args, "--profile", tc.profile)
			}
			output := outcomeCLI(t, f.service, 0, args...)
			if !strings.Contains(output, `"profile":"`+tc.expected+`"`) {
				t.Fatalf("dispatch did not select %s: %s", tc.expected, output)
			}
		})
	}
}

func TestFirstOutcomeDispatchUsesTypedDefaultFromMergedProjectAndGlobalRules(t *testing.T) {
	f := newFirstOutcomeFixture(t)
	globalConfig := `[profiles.pi-luna]
kind='pi'
[profiles.pi-sol]
kind='pi'
[profiles.claude-opus]
kind='claude'
[profiles.codex-sol]
kind='codex'
[[dispatch]]
when='image work'
use='codex-sol'
[[dispatch]]
when='simple, well-known bug fix'
use='pi-luna'
`
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(globalConfig), 0600); err != nil {
		t.Fatal(err)
	}
	projectConfig := `[[dispatch]]
type='scout'
use='pi-luna'
[[dispatch]]
type='scout'
when='science-heavy research'
use='pi-sol'
[[dispatch]]
type='scout'
when='exceptionally technical research'
use='claude-opus'
`
	projectConfigPath := filepath.Join(f.home, "projects", f.project.Name, "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfigPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfigPath, []byte(projectConfig), 0600); err != nil {
		t.Fatal(err)
	}
	brief := "---\ntype: scout\ntitle: Scout resolver merge\ndone_when: the phase default resolves\n---\nInvestigate dispatch behavior.\n"
	if err := os.WriteFile(f.brief, []byte(brief), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")

	output := outcomeCLI(t, f.service, 0, "dispatch", "--brief", f.brief, "--json")
	var result struct {
		TaskType     string `json:"task_type"`
		Profile      string `json:"profile"`
		DispatchRule string `json:"dispatch_rule"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("dispatch output is not JSON: %v\n%s", err, output)
	}
	if result.TaskType != "scout" || result.Profile != "pi-luna" || result.DispatchRule != "type=scout" {
		t.Fatalf("merged dispatch resolution = %#v, want scout phase default pi-luna: %s", result, output)
	}
}

func TestFirstOutcomeTypedDispatchKeepsConditionalChoicesAndIgnoresGenericRules(t *testing.T) {
	f := newFirstOutcomeFixture(t)
	configText := `[profiles.pi-luna]
kind='pi'
[profiles.pi-sol]
kind='pi'
[profiles.claude-sonnet]
kind='claude'
[profiles.claude-opus]
kind='claude'
[profiles.codex-sol]
kind='codex'
[[dispatch]]
type='ship'
when='plan or design; pi-sol by default'
use='pi-sol'
[[dispatch]]
type='ship'
when='exceptionally technical plan or design, or pi-sol already failed'
use='claude-opus'
[[dispatch]]
type='ship'
when='clear-scope implementation; choose based on load and fit'
use='pi-luna'
[[dispatch]]
type='ship'
when='clear-scope implementation; choose based on load and fit'
use='claude-sonnet'
[[dispatch]]
when='image work'
use='codex-sol'
[[dispatch]]
when='simple, well-known bug fix'
use='pi-luna'
`
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	output := outcomeCLI(t, f.service, 1, "dispatch", "--brief", f.brief, "--json")
	for _, candidate := range []string{"pi-sol", "claude-opus", "pi-luna", "claude-sonnet"} {
		if !strings.Contains(output, candidate) {
			t.Fatalf("typed ship choices omitted %s: %s", candidate, output)
		}
	}
	if strings.Contains(output, "codex-sol") || strings.Contains(output, "image work") {
		t.Fatalf("generic rule competed with typed ship choices: %s", output)
	}
}

func TestFirstOutcomeDispatchOmitsProfileArguments(t *testing.T) {
	f := newFirstOutcomeFixture(t)
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("[profiles.deep]\nkind='pi'\nargs=['--api-key','secret']\n[dispatch.default]\nuse='deep'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := outcomeCLI(t, f.service, 0, "dispatch", "--brief", f.brief, "--json")
	if !strings.Contains(output, `"profile":"deep"`) {
		t.Fatal(output)
	}
	for _, leaked := range []string{"resolved_profile", "--api-key", "secret"} {
		if strings.Contains(output, leaked) {
			t.Fatalf("dispatch exposed %q: %s", leaked, output)
		}
	}
}

func TestFirstOutcomeImplicitRideRelaunchAndRecovery(t *testing.T) {
	f := newFirstOutcomeFixture(t)
	f.fake.BeforeCall = func(method string) {
		if method == "agent.start" && len(f.fake.SnapshotValue.Panes) == 1 {
			f.fake.SnapshotValue.Panes = append(f.fake.SnapshotValue.Panes, herdr.Pane{PaneID: "fake:child:p1", WorkspaceID: "fake:child", Label: "posse:shop:t1", Agent: "pi", AgentStatus: "working"})
		}
	}
	output := outcomeCLI(t, f.service, 0, "ride", "--brief", f.brief, "--name", "first-outcome")
	if !strings.Contains(output, "implicit:pi") {
		t.Fatal(output)
	}
	task, err := f.db.Task(context.Background(), f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Profile != "implicit:pi" || task.DispatchRule != "implicit: lead kind" || task.State != store.StateWorking {
		t.Fatalf("Task: %#v", task)
	}
	// Changing the Lead and adding rules must not change the already resolved Rider.
	outcomeCLI(t, f.service, 0, "config", "set", "lead.kind", "codex")
	outcomeCLI(t, f.service, 0, "config", "set", "profiles.routing.kind", "codex")
	outcomeCLI(t, f.service, 0, "config", "set", "dispatch", "[{type='ship', use='routing'}]")
	output = outcomeCLI(t, f.service, 0, "relaunch", "t1")
	if !strings.Contains(output, "working") {
		t.Fatal(output)
	}
	// Recover a missing Rider pane using the same persisted implicit kind.
	f.fake.SnapshotValue.Panes = f.fake.SnapshotValue.Panes[:1]
	outcomeCLI(t, f.service, 0, "relaunch", "t1")
	for _, call := range f.fake.Calls {
		if call.Method == "agent.start" {
			if call.Params["kind"] != "pi" {
				t.Fatalf("relaunch changed implicit kind: %#v", call)
			}
			for _, arg := range call.Params["args"].([]string) {
				if arg == "--model" || arg == "--thinking" {
					t.Fatalf("implicit Profile supplied model/effort: %#v", call)
				}
			}
		}
	}
	updated, err := f.db.Task(context.Background(), f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Profile != task.Profile || updated.DispatchRule != task.DispatchRule || updated.WorktreePath != task.WorktreePath {
		t.Fatalf("resolution changed: %#v", updated)
	}
	outcomeCLI(t, f.service, 0, "config", "set", "profiles.named.kind", "codex")
	outcomeCLI(t, f.service, 0, "relaunch", "t1", "--profile", "named")
	updated, err = f.db.Task(context.Background(), f.project.ID, "t1")
	if err != nil || updated.Profile != "named" {
		t.Fatalf("explicit switch: %#v %v", updated, err)
	}
}

func TestFirstOutcomeReadinessRecomputed(t *testing.T) {
	previousTTL := forgeProbeCacheTTL
	forgeProbeCacheTTL = 0 // This test changes fake CLI state between checks.
	t.Cleanup(func() { forgeProbeCacheTTL = previousTTL })
	f := newFirstOutcomeFixture(t)
	bin := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	for _, cli := range []string{"gh", "glab", "no-mistakes"} {
		script := "#!/bin/sh\n[ -e '" + filepath.Join(f.root, cli+"-ready") + "' ] && exit 0\nprintf 'repo not initialized\\n'\nexit 1\n"
		if err := os.WriteFile(filepath.Join(bin, cli), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Skip root fetches in this auth-only fixture, so it never contacts a forge.
	if err := f.db.RecordCheckoutAttempt(context.Background(), f.project.ID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.repo, "remote", "add", "origin", "https://github.com/example/project.git")
	check := func(want, absent []string) {
		t.Helper()
		for _, args := range [][]string{{"--json"}, {"lead", "--json"}} {
			output := outcomeCLI(t, f.service, 0, args...)
			var result struct {
				Readiness []readinessGap `json:"readiness"`
			}
			if err := json.Unmarshal([]byte(output), &result); err != nil {
				t.Fatal(err)
			}
			codes := map[string]bool{}
			for _, gap := range result.Readiness {
				codes[gap.Code] = true
				if gap.Consequence == "" || gap.Fix == "" {
					t.Fatalf("incomplete gap: %#v", gap)
				}
				if gap.Code == "gate_empty" && (!gap.UserOnly || gap.Key != "defaults.gate") {
					t.Fatalf("Gate authority: %#v", gap)
				}
			}
			for _, code := range want {
				if !codes[code] {
					t.Fatalf("%v missing %s: %s", args, code, output)
				}
			}
			for _, code := range absent {
				if codes[code] {
					t.Fatalf("%v retained %s: %s", args, code, output)
				}
			}
		}
	}
	check([]string{"forge_auth", "gate_empty", "autonomy_ask", "machine_setup"}, []string{"no_mistakes_uninitialized"})
	if err := os.WriteFile(filepath.Join(f.root, "gh-ready"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	outcomeCLI(t, f.service, 0, "config", "set", "defaults.gate", "['true']", "--project", "shop", "--user-approved", "Use true as the fixture Gate")
	outcomeCLI(t, f.service, 0, "config", "set", "autonomy.yolo", "true", "--project", "shop", "--user-approved", "Grant standing fixture Autonomy")
	check([]string{"machine_setup"}, []string{"forge_auth", "gate_empty", "autonomy_ask"})
	outcomeCLI(t, f.service, 0, "config", "set", "defaults.forge", "gitlab", "--project", "shop")
	gitTest(t, f.repo, "remote", "set-url", "origin", "https://gitlab.example.com/group/project.git")
	output := outcomeCLI(t, f.service, 0, "--json")
	if !strings.Contains(output, "gitlab.example.com") || !strings.Contains(output, "glab auth login --hostname gitlab.example.com") {
		t.Fatal(output)
	}
	output = outcomeCLI(t, f.service, 0, "doctor", "--json")
	var doctorFields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &doctorFields); err != nil {
		t.Fatal(err)
	}
	if _, ok := doctorFields["readiness"]; ok {
		t.Fatalf("doctor added readiness: %s", output)
	}
	if err := os.WriteFile(filepath.Join(f.root, "glab-ready"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	check([]string{"machine_setup"}, []string{"forge_auth"})
	if err := os.WriteFile(filepath.Join(f.root, "no-mistakes-ready"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	outcomeCLI(t, f.service, 0, "config", "set", "defaults.landing_mode", "no-mistakes", "--project", "shop")
	if err := os.Remove(filepath.Join(f.root, "no-mistakes-ready")); err != nil {
		t.Fatal(err)
	}
	check([]string{"machine_setup", "no_mistakes_uninitialized"}, []string{"forge_auth"})
	if err := os.WriteFile(filepath.Join(f.root, "no-mistakes-ready"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	// Applying machine setup is a User-shell action; fake its read-only Herdr responses.
	f.fake.RunOut["plugin link "+filepath.Join(f.home, "plugin", "herdr-plugin.toml")] = []byte(`{}`)
	outcomeCLI(t, f.service, 0, "setup", "--binary", "/usr/bin/true")
	f.fake.RunOut["plugin list --json"] = []byte(`{"plugins":[{"id":"posse.herdr","path":"` + filepath.Join(f.home, "plugin", "herdr-plugin.toml") + `"}]}`)
	check(nil, []string{"forge_auth", "gate_empty", "autonomy_ask", "no_mistakes_uninitialized", "machine_setup"})
	output = outcomeCLI(t, f.service, 0, "--json")
	if strings.Contains(output, `"readiness"`) {
		t.Fatalf("fully ready status retained readiness: %s", output)
	}
	// Deleting one installed asset makes setup pending again without a first-run flag.
	if err := os.Remove(filepath.Join(f.root, "pi", "extensions", "posse-worker-guard.ts")); err != nil {
		t.Fatal(err)
	}
	check([]string{"machine_setup"}, []string{"no_mistakes_uninitialized"})
}

func TestFirstOutcomeInstructionsAndOptionalSetupAgree(t *testing.T) {
	f := newFirstOutcomeFixture(t)
	output := outcomeCLI(t, f.service, 0, "lead")
	for _, rule := range []string{"Default workflow guidance", "lightest workflow", "one Ship Brief and dispatch it directly", "Scouts, specifications, tickets and separate review Tasks are optional", "consequential questions", "Investigate repository facts before asking the User", "runtime obligations", "explicit yes", "--user-approved", "one-off permission never becomes standing Autonomy", "deliverable", "verification", "permitted effects", "return conditions", "setup and personalization optional", "at most once", "language they write in", "lowkey", "posse preferences set <lead|rider> --file <file>", "Riders cannot write preferences"} {
		if !strings.Contains(output, rule) {
			t.Fatalf("Lead missing %q: %s", rule, output)
		}
	}
	for _, personalProfile := range []string{"pi-luna", "pi-sol", "claude-sonnet", "claude-opus", "codex-sol"} {
		if strings.Contains(output, personalProfile) {
			t.Fatalf("Lead instructions hard-coded personal Profile %q: %s", personalProfile, output)
		}
	}
	cfg, err := config.Load(f.home, "shop")
	if err != nil {
		t.Fatal(err)
	}
	composed, err := composeLeadInstructions(f.home, f.project, cfg, "pi", nil)
	if err != nil {
		t.Fatal(err)
	}
	text := composed.Text
	for _, personalProfile := range []string{"pi-luna", "pi-sol", "claude-sonnet", "claude-opus", "codex-sol"} {
		if strings.Contains(text, personalProfile) {
			t.Fatalf("Lead system prompt hard-coded personal Profile %q: %s", personalProfile, text)
		}
	}
	for _, rule := range []string{"Runtime obligations", leadLanguageRule(cfg)} {
		if !strings.Contains(text, rule) {
			t.Fatalf("system prompt omitted rule: %s", rule)
		}
	}
	for _, language := range []string{"en", "vi"} {
		outcomeCLI(t, f.service, 0, "config", "set", "identity.lead.language", language)
		output = outcomeCLI(t, f.service, 0, "lead")
		if !strings.Contains(output, "explicitly configured language: "+language) {
			t.Fatal(output)
		}
	}
	outcomeCLI(t, f.service, 0, "config", "unset", "identity.lead.language")
	if output = outcomeCLI(t, f.service, 0, "lead"); !strings.Contains(output, "language they write in") {
		t.Fatal(output)
	}
}
