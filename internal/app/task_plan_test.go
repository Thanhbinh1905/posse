package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func TestUnsupportedBriefsRefuseBeforePreparationOrResourceAllocation(t *testing.T) {
	for _, field := range []string{"executor: lead\n", "execution: in-place\n", "repos: [api]\n", "base: {api: main}\n", "target: [main]\n"} {
		t.Run(strings.TrimSpace(field), func(t *testing.T) {
			f := newStartFixture(t)
			text := "---\ntype: change\ntitle: Launch probe\ndone_when: checked\n" + field + "---\nInspect.\n"
			if err := os.WriteFile(f.brief, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			code, output := f.start(t)
			if code != 1 || !strings.Contains(output, "brief_invalid") {
				t.Fatalf("unsupported Brief was not refused: code=%d %s", code, output)
			}
			if len(f.fake.Calls) != 0 {
				t.Fatalf("admission called Herdr before refusal: %#v", f.fake.Calls)
			}
			db, err := store.Open(f.home)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			tasks, err := db.Tasks(context.Background(), f.project.ID, true)
			if err != nil || len(tasks) != 0 {
				t.Fatalf("refusal allocated Task: %#v %v", tasks, err)
			}
			sandboxes, err := db.Sandboxes(context.Background(), f.project.ID)
			if err != nil || len(sandboxes) != 0 {
				t.Fatalf("refusal allocated Sandbox: %#v %v", sandboxes, err)
			}
			intents, err := db.Intents(context.Background(), f.project.ID)
			if err != nil || len(intents) != 0 {
				t.Fatalf("refusal allocated intent: %#v %v", intents, err)
			}
			for _, directory := range []string{"scratch", "pool", filepath.Join("projects", "shop", "tasks")} {
				if _, err := os.Stat(filepath.Join(f.home, directory)); !os.IsNotExist(err) {
					t.Fatalf("refusal allocated %s: %v", directory, err)
				}
			}
			if branches := strings.TrimSpace(gitTest(t, f.repo, "for-each-ref", "--format=%(refname)", "refs/heads/posse/")); branches != "" {
				t.Fatalf("refusal allocated branches: %s", branches)
			}
		})
	}
}

func TestWorkerStatusReadsOnlyOwnAdmittedStateWithoutMaintenance(t *testing.T) {
	f := newStartFixture(t)
	if code, output := f.start(t); code != 0 {
		t.Fatalf("start code=%d %s", code, output)
	}
	task, _ := f.onlyTask(t)
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	siblingID, err := db.CreateTask(context.Background(), f.project.ID, store.Task{Seq: 2, Type: "research", Title: "private sibling", Profile: "private-profile"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.StartIntent(context.Background(), f.project.ID, siblingID, "start", "done:task.create", "{}", 999999); err != nil {
		t.Fatal(err)
	}
	beforeIntents, err := db.Intents(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeTask, err := db.TaskByID(context.Background(), f.project.ID, siblingID)
	if err != nil {
		t.Fatal(err)
	}
	// Broken current config and a missing Herdr adapter must not affect status.
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("not valid TOML ["), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PANE_ID", task.PaneID)
	t.Setenv(workerHomeEnv, f.home)
	t.Chdir(task.WorktreePath)
	cli := testService(f.home, nil).CLI()
	output := &bytes.Buffer{}
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"status", "--json"}); code != 0 {
		t.Fatalf("status triggered preparation: code=%d %s", code, output)
	}
	var view struct {
		Task      string        `json:"task"`
		Admission string        `json:"admission"`
		Plan      taskplan.Plan `json:"plan"`
	}
	if err := json.Unmarshal(output.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Task != "t1" || view.Admission != "admitted" || view.Plan.Version != taskplan.Version || strings.Contains(output.String(), "private sibling") || strings.Contains(output.String(), "private-profile") {
		t.Fatalf("status exposed wrong Task: %s", output)
	}
	output.Reset()
	if code := cli.Run([]string{"status", "t2"}); code != 2 {
		t.Fatalf("status allowed sibling selection: code=%d %s", code, output)
	}
	afterTask, err := db.TaskByID(context.Background(), f.project.ID, siblingID)
	if err != nil || !reflect.DeepEqual(beforeTask, afterTask) {
		t.Fatalf("status recovered or cleaned sibling: %#v %v", afterTask, err)
	}
	afterIntents, err := db.Intents(context.Background(), f.project.ID)
	if err != nil || !reflect.DeepEqual(beforeIntents, afterIntents) {
		t.Fatalf("status settled unrelated intents: %#v %v", afterIntents, err)
	}
}

func TestAdmittedBriefAndPreferencesDoNotFollowMutableCopies(t *testing.T) {
	f := newStartFixture(t)
	configText := "[identity.lead]\nlanguage = \"vi\"\n\n[defaults]\nmerge_mode = \"local\"\n\n[profiles.claude]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"claude\"\n"
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	original := []byte("---\r\ntype: change\r\ntitle: Launch probe\r\ndone_when: checked\r\nticket: 12\r\n---\r\nUser words stay exact.\r\n")
	if err := os.WriteFile(f.brief, original, 0o600); err != nil {
		t.Fatal(err)
	}
	prefPath := filepath.Join(f.home, "preferences", "worker.md")
	if err := os.MkdirAll(filepath.Dir(prefPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prefPath, []byte("Original preference.\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := f.start(t); code != 0 {
		t.Fatalf("start code=%d %s", code, output)
	}
	task, _ := f.onlyTask(t)
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil || !bytes.Equal(plan.Inputs[0].Bytes, original) || plan.ProseLanguage != "vi" {
		t.Fatalf("selected Brief lost exact bytes: %#v %v", plan, err)
	}
	if err := os.WriteFile(filepath.Join(plan.Paths.Output, "brief.md"), []byte(strings.ReplaceAll(string(original), "12", "999")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prefPath, []byte("Edited preference."), 0o600); err != nil {
		t.Fatal(err)
	}
	brief, err := readTaskBrief(f.home, f.project, task)
	if err != nil || brief.Ticket == nil || brief.Ticket.Number != 12 {
		t.Fatalf("frozen ticket followed mutable file: %#v %v", brief, err)
	}
	if err := os.Remove(filepath.Join(plan.Paths.Output, "brief.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(plan.Paths.Output, "launch.md")); err != nil {
		t.Fatal(err)
	}
	if err := restoreAdmittedLaunchFiles(f.home, f.project, task, plan); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(filepath.Join(plan.Paths.Output, "brief.md"))
	if err != nil || !bytes.Equal(restored, original) {
		t.Fatalf("crash restore changed Brief: %q %v", restored, err)
	}
	launch, err := os.ReadFile(filepath.Join(plan.Paths.Output, "launch.md"))
	if err != nil || !strings.Contains(string(launch), "Original preference.") || strings.Contains(string(launch), "Edited preference.") || !strings.Contains(string(launch), "configured language (\"vi\")") {
		t.Fatalf("crash restore used edited preferences: %s %v", launch, err)
	}
}

func TestStartFreezesValidationLoopSettingsAndBriefTightening(t *testing.T) {
	f := newStartFixture(t)
	global := `[defaults]
merge_mode = "local"

[profiles.claude]
kind = "claude"

[dispatch.default]
use = "claude"

[review_loop]
enabled = true
max_rounds = 3
block_at = "P1"
auto_start = false
docs = true
guidance = ["docs/review.md"]

[gate_loop]
max_fix_rounds = 2

[ci_loop]
max_fix_rounds = 1
rerun_cancelled = 2
`
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	brief := "---\ntype: change\ntitle: Launch probe\ndone_when: the Worker acts\nreview_rounds: 2\n---\nCreate a committed file.\n"
	if err := os.WriteFile(f.brief, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := f.start(t); code != 0 {
		t.Fatalf("start code=%d %s", code, output)
	}
	task, _ := f.onlyTask(t)
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil || plan.ValidationLoops == nil {
		t.Fatalf("admitted validation loops = %#v, %v", plan, err)
	}
	frozen := plan.ValidationLoops
	if frozen.Review.MaxRounds != 3 || frozen.BriefReviewRounds != 2 || frozen.MaxReviewRounds() != 2 || frozen.Review.AutoStart || !frozen.Review.Docs || !reflect.DeepEqual(frozen.Review.Guidance, []string{"docs/review.md"}) || frozen.Gate.MaxFixRounds != 2 || frozen.CI.MaxFixRounds != 1 || frozen.CI.RerunCancelled != 2 {
		t.Fatalf("admitted validation settings = %#v", frozen)
	}

	changed := strings.ReplaceAll(global, "enabled = true", "enabled = false")
	changed = strings.ReplaceAll(changed, "max_rounds = 3", "max_rounds = 1")
	changed = strings.ReplaceAll(changed, "block_at = \"P1\"", "block_at = \"P2\"")
	changed = strings.ReplaceAll(changed, "auto_start = false", "auto_start = true")
	changed = strings.ReplaceAll(changed, "docs = true", "docs = false")
	changed = strings.ReplaceAll(changed, "docs/review.md", "changed.md")
	changed = strings.ReplaceAll(changed, "max_fix_rounds = 2", "max_fix_rounds = 0")
	changed = strings.ReplaceAll(changed, "max_fix_rounds = 1", "max_fix_rounds = 0")
	changed = strings.ReplaceAll(changed, "rerun_cancelled = 2", "rerun_cancelled = 0")
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	task, _ = f.onlyTask(t)
	updated, err := task.AdmittedPlan()
	if err != nil || updated == nil || !reflect.DeepEqual(updated.ValidationLoops, frozen) {
		t.Fatalf("admitted plan followed later config edits: %#v, %v", updated, err)
	}
}

func TestStartRejectsValidationConfigAboveThreeRounds(t *testing.T) {
	f := newStartFixture(t)
	global := "[review_loop]\nmax_rounds = 4\n"
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := f.start(t); code != 1 || !strings.Contains(output, "config_invalid") || !strings.Contains(output, "review_loop.max_rounds") {
		t.Fatalf("invalid review cap exit=%d output=%s", code, output)
	}
	if f.fake.CallCount("tab.create") != 0 || f.fake.CallCount("agent.start") != 0 {
		t.Fatalf("invalid loop config allocated Worker resources: %#v", f.fake.Calls)
	}
}

func TestStartRejectsBriefReviewRoundsAboveConfiguredCap(t *testing.T) {
	f := newStartFixture(t)
	global := `[defaults]
merge_mode = "local"

[profiles.claude]
kind = "claude"

[dispatch.default]
use = "claude"

[review_loop]
max_rounds = 2
`
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	brief := "---\ntype: change\ntitle: Launch probe\ndone_when: the Worker acts\nreview_rounds: 3\n---\nCreate a committed file.\n"
	if err := os.WriteFile(f.brief, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := f.start(t); code != 1 || !strings.Contains(output, "brief_invalid") || !strings.Contains(output, "cannot exceed") {
		t.Fatalf("raised Brief cap exit=%d output=%s", code, output)
	}
	if f.fake.CallCount("tab.create") != 0 || f.fake.CallCount("agent.start") != 0 {
		t.Fatalf("invalid Brief allocated Worker resources: %#v", f.fake.Calls)
	}
}

func TestAdmittedCodexInstructionsDoNotReadCurrentHarnessConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("developer_instructions = 'original selected instructions'\napi_key = 'unconsumed credential'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inputs, err := selectedSkillInputs("codex")
	if err != nil {
		t.Fatal(err)
	}
	plan := &taskplan.Plan{Inputs: inputs}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("developer_instructions = 'edited instructions'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args, err := appendAdmittedSkillArgs("codex", []string{"-c", `developer_instructions="Profile instructions"`}, posseLaunchSkills{CodexInstructions: "frozen skills"}, plan)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(args)
	if !strings.Contains(string(encoded), "original selected instructions") || strings.Contains(string(encoded), "edited instructions") || strings.Contains(string(encoded), "unconsumed credential") {
		t.Fatalf("launch followed mutable or unconsumed config: %s", encoded)
	}
	for _, input := range inputs {
		if strings.Contains(string(input.Bytes), "unconsumed credential") {
			t.Fatal("snapshotted unrelated credential")
		}
	}
}
