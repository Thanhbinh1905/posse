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
			f := newRideFixture(t)
			text := "---\ntype: ship\ntitle: Launch probe\ndone_when: checked\n" + field + "---\nInspect.\n"
			if err := os.WriteFile(f.brief, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			code, output := f.ride(t)
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
			mounts, err := db.Mounts(context.Background(), f.project.ID)
			if err != nil || len(mounts) != 0 {
				t.Fatalf("refusal allocated Mount: %#v %v", mounts, err)
			}
			intents, err := db.Intents(context.Background(), f.project.ID)
			if err != nil || len(intents) != 0 {
				t.Fatalf("refusal allocated intent: %#v %v", intents, err)
			}
			for _, directory := range []string{"scratch", "remuda", filepath.Join("projects", "shop", "tasks")} {
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

func TestRiderStatusReadsOnlyOwnAdmittedStateWithoutMaintenance(t *testing.T) {
	f := newRideFixture(t)
	if code, output := f.ride(t); code != 0 {
		t.Fatalf("ride code=%d %s", code, output)
	}
	task, _ := f.onlyTask(t)
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	siblingID, err := db.CreateTask(context.Background(), f.project.ID, store.Task{Seq: 2, Type: "scout", Title: "private sibling", Profile: "private-profile"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.StartIntent(context.Background(), f.project.ID, siblingID, "ride", "done:task.create", "{}", 999999); err != nil {
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
	if view.Task != "t1" || view.Admission != "admitted" || view.Plan.Version != 1 || strings.Contains(output.String(), "private sibling") || strings.Contains(output.String(), "private-profile") {
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
	f := newRideFixture(t)
	configText := "[identity.lead]\nlanguage = \"vi\"\n\n[defaults]\nlanding_mode = \"local\"\n\n[profiles.claude]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"claude\"\n"
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	original := []byte("---\r\ntype: ship\r\ntitle: Launch probe\r\ndone_when: checked\r\nticket: 12\r\n---\r\nUser words stay exact.\r\n")
	if err := os.WriteFile(f.brief, original, 0o600); err != nil {
		t.Fatal(err)
	}
	prefPath := filepath.Join(f.home, "preferences", "rider.md")
	if err := os.MkdirAll(filepath.Dir(prefPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prefPath, []byte("Original preference.\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := f.ride(t); code != 0 {
		t.Fatalf("ride code=%d %s", code, output)
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
	if err := restoreAdmittedLaunchFiles(f.project, task, plan); err != nil {
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
