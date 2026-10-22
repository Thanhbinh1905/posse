package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestConfigRollbackPreservesOriginalMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("[defaults]\nmax_workers = 4\n")
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	snapshot, mode, exists, err := configSnapshot(path)
	if err != nil || !exists || mode.Perm() != 0o640 {
		t.Fatalf("config snapshot = exists %v mode %o err %v", exists, mode.Perm(), err)
	}
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreConfigFile(path, snapshot, exists, mode); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(original) {
		t.Fatalf("restored bytes = %q, err %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("restored mode = %o, err %v", info.Mode().Perm(), err)
	}
}

func TestConfigSetRejectsUnknownProfileOnValidHome(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	service := testService(home, herdr.NewFake())
	t.Chdir(repo)
	if code, output := runCLI(t, service, "config", "show"); code != 0 {
		t.Fatalf("fresh config is invalid: code=%d output=%s", code, output)
	}

	code, output := runCLI(t, service, "config", "set", "dispatch", `[{ type = "change", use = "ghost" }]`)
	if code != 1 || !strings.Contains(output, "references unknown Profile ghost") {
		t.Fatalf("invalid Profile reference was accepted: code=%d output=%s", code, output)
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("rejected edit left config behind: %v", err)
	}
	if code, output := runCLI(t, service, "config", "show"); code != 0 {
		t.Fatalf("rejected edit made the valid home invalid: code=%d output=%s", code, output)
	}
}

func TestConfigUnsetRejectsNewSemanticErrorAlongsideExistingError(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	path := filepath.Join(home, "config.toml")
	original := []byte("[profiles.fast]\nkind = 'custom'\nmodel = 'model-1'\n[kinds.custom]\nmodel_args = ['--model', '{model}']\n[dispatch.default]\nuse = 'missing'\n")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	service := testService(home, herdr.NewFake())
	t.Chdir(repo)

	code, output := runCLI(t, service, "config", "unset", "kinds.custom.model_args")
	if code != 1 || !strings.Contains(output, "Worker kind has no model_args template") {
		t.Fatalf("unset introduced a new semantic error: code=%d output=%s", code, output)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("rejected unset changed the original config: %q err=%v", data, err)
	}
}

func TestConfigSetRejectsNewErrorAlongsideTwoExistingErrors(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	path := filepath.Join(home, "config.toml")
	original := []byte("[defaults]\nreview = 'on_risk'\n[pool]\nkeep_idle = -1\n")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	service := testService(home, herdr.NewFake())
	t.Chdir(repo)

	code, output := runCLI(t, service, "config", "set", "dispatch", `[{ type = "change", use = "ghost" }]`)
	if code != 1 || !strings.Contains(output, "references unknown Profile ghost") {
		t.Fatalf("edit introduced a third invalid setting: code=%d output=%s", code, output)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("rejected edit changed the original invalid config: %q err=%v", data, err)
	}
}

func TestConfigEditsCanRepairMultipleInvalidSettings(t *testing.T) {
	for _, edits := range [][][]string{
		{{"config", "set", "defaults.review", "none"}, {"config", "set", "pool.keep_idle", "2"}},
		{{"config", "set", "pool.keep_idle", "2"}, {"config", "set", "defaults.review", "none"}},
	} {
		t.Run(edits[0][2], func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			initRepo(t, repo)
			home := filepath.Join(root, "posse")
			path := filepath.Join(home, "config.toml")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("[defaults]\nreview = 'on_risk'\n[pool]\nkeep_idle = -1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			service := testService(home, herdr.NewFake())
			t.Chdir(repo)
			for _, args := range edits {
				if code, output := runCLI(t, service, args...); code != 0 {
					t.Fatalf("could not repair %v while another setting was invalid: %d %s", args, code, output)
				}
			}
			if code, output := runCLI(t, service, "config", "show"); code != 0 {
				t.Fatalf("config remained invalid after both repairs: %d %s", code, output)
			}
		})
	}
}

func TestConfigProjectReviewShorthandCanBeRepairedByCLI(t *testing.T) {
	for _, action := range []string{"set", "unset"} {
		t.Run(action, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			initRepo(t, repo)
			home := filepath.Join(root, "posse")
			db, err := store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			project, err := db.CreateProject(context.Background(), "shop", repo, "main")
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, "projects", project.Name, "config.toml")
			if err := os.WriteFile(path, []byte("review = 'on_risk'\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			service := testService(home, herdr.NewFake())
			t.Chdir(repo)
			if code, output := runCLI(t, service, "config", "show", "--project", "shop"); code != 1 || !strings.Contains(output, "posse config set defaults.review none --project shop") {
				t.Fatalf("invalid Project policy lacks an actionable repair: code=%d output=%s", code, output)
			}
			args := []string{"config", action, "defaults.review", "--project", "shop"}
			if action == "set" {
				args = []string{"config", action, "defaults.review", "none", "--project", "shop"}
			}
			if code, output := runCLI(t, service, args...); code != 0 {
				t.Fatalf("Project shorthand was not repaired with %s: %d %s", action, code, output)
			}
			if _, err := config.Load(home, "shop"); err != nil {
				t.Fatalf("Project config remains invalid after %s: %v", action, err)
			}
		})
	}
}

func TestConfigUserOnlyBoundaryForPaneRoles(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	workerID, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 1, Type: "research", Title: "Config", State: store.StateSpawning, MergeMode: "local", PaneID: "w1:p2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), workerID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	service := testService(home, herdr.NewFake())
	output, errorsOut := &bytes.Buffer{}, &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	t.Chdir(repo)

	t.Setenv("HERDR_PANE_ID", "w1:p2")
	for _, args := range [][]string{
		{"config", "set", "defaults.max_workers", "6", "--project", "shop"},
		{"config", "set", "defaults.gate", `["make check"]`, "--project", "shop"},
		{"config", "set", "review_loop.max_rounds", "2", "--project", "shop"},
	} {
		if code := cli.Run(args); code != 1 || !strings.Contains(output.String()+errorsOut.String(), "worker_forbidden") {
			t.Fatalf("Worker was allowed to change config with %v: code=%d out=%s err=%s", args, code, output, errorsOut)
		}
		output.Reset()
		errorsOut.Reset()
	}
	if code := cli.Run([]string{"config", "show", "--project", "shop"}); code != 0 {
		t.Fatalf("Worker could not read config: %s %s", output, errorsOut)
	}
	output.Reset()
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	if code := cli.Run([]string{"config", "set", "defaults.max_workers", "6", "--project", "shop"}); code != 0 {
		t.Fatalf("Lead could not change a shared setting: %s %s", output, errorsOut)
	}
	output.Reset()
	if code := cli.Run([]string{"config", "set", "autonomy.merge", `"auto"`, "--project", "shop"}); code != 1 || !strings.Contains(output.String()+errorsOut.String(), "user_only") {
		t.Fatalf("Lead was allowed to change a User-only setting: code=%d out=%s err=%s", code, output, errorsOut)
	}
	output.Reset()
	errorsOut.Reset()
	if code := cli.Run([]string{"config", "set", "review_loop.max_rounds", "2", "--project", "shop"}); code != 1 || !strings.Contains(output.String()+errorsOut.String(), "user_only") {
		t.Fatalf("Lead was allowed to change a User-only validation setting: code=%d out=%s err=%s", code, output, errorsOut)
	}
	if _, err := os.Stat(filepath.Join(home, "projects", "shop", "config.toml")); err != nil {
		t.Fatal(err)
	}
}

func TestConfigUserOnlyApprovalIsRecordedForLeadOnly(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 1, Type: "research", Title: "Config", State: store.StateSpawning, MergeMode: "local", PaneID: "w1:p2"}); err != nil {
		t.Fatal(err)
	}
	service := testService(home, herdr.NewFake())
	output, errorsOut := &bytes.Buffer{}, &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	t.Chdir(repo)
	run := func(wantCode int, wantError string, args ...string) {
		t.Helper()
		output.Reset()
		errorsOut.Reset()
		if code := cli.Run(args); code != wantCode || (wantError != "" && !strings.Contains(output.String()+errorsOut.String(), wantError)) {
			t.Fatalf("posse %v: code=%d stdout=%s stderr=%s; want code %d, error %q", args, code, output, errorsOut, wantCode, wantError)
		}
	}
	projectPath := filepath.Join(home, "projects", "shop", "config.toml")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	for _, args := range [][]string{
		{"config", "set", "autonomy.yolo", "true", "--project", "shop"},
		{"config", "set", "autonomy.yolo", "true", "--project", "shop", "--user-approved", "  "},
		{"config", "unset", "autonomy.yolo", "--project", "shop"},
		{"config", "unset", "autonomy.yolo", "--project", "shop", "--user-approved", "  "},
	} {
		run(1, "user_only", args...)
	}
	if _, err := os.Stat(projectPath); !os.IsNotExist(err) {
		t.Fatalf("refused Lead command wrote config: %v", err)
	}
	quote := "set yolo auto merge for this repo"
	run(0, "", "config", "set", "autonomy.yolo", "true", "--project", "shop", "--user-approved", quote)
	var projectID int64
	var key, action, value, recordedQuote string
	if err := db.QueryRowContext(context.Background(), `SELECT project_id,key,action,value,user_quote FROM config_approvals ORDER BY id DESC LIMIT 1`).Scan(&projectID, &key, &action, &value, &recordedQuote); err != nil {
		t.Fatal(err)
	}
	if projectID != project.ID || key != "autonomy.yolo" || action != "set" || value != "true" || recordedQuote != quote {
		t.Fatalf("recorded approval = %d %q %q %q %q", projectID, key, action, value, recordedQuote)
	}
	run(0, "", "config", "unset", "autonomy.yolo", "--project", "shop", "--user-approved", "  unset yolo  ")
	var unsetValue *string
	if err := db.QueryRowContext(context.Background(), `SELECT project_id,key,action,value,user_quote FROM config_approvals ORDER BY id DESC LIMIT 1`).Scan(&projectID, &key, &action, &unsetValue, &recordedQuote); err != nil {
		t.Fatal(err)
	}
	if projectID != project.ID || key != "autonomy.yolo" || action != "unset" || unsetValue != nil || recordedQuote != "  unset yolo  " {
		t.Fatalf("recorded unset approval = %d %q %q %v %q", projectID, key, action, unsetValue, recordedQuote)
	}
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM config_approvals`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("Lead approval count = %d, %v", count, err)
	}
	run(1, "config_invalid", "config", "set", "autonomy.yolo", "not-a-boolean", "--project", "shop", "--user-approved", quote)
	run(0, "", "config", "set", "defaults.review", `"quick"`, "--user-approved", quote)
	if err := db.QueryRowContext(context.Background(), `SELECT project_id,key,action,value,user_quote FROM config_approvals ORDER BY id DESC LIMIT 1`).Scan(&projectID, &key, &action, &value, &recordedQuote); err != nil {
		t.Fatal(err)
	}
	if projectID != project.ID || key != "defaults.review" || action != "set" || value != `"quick"` || recordedQuote != quote {
		t.Fatalf("global approval = %d %q %q %q %q", projectID, key, action, value, recordedQuote)
	}
	t.Setenv("HERDR_PANE_ID", "w1:p2")
	run(1, "worker_forbidden", "config", "set", "autonomy.yolo", "true", "--project", "shop", "--user-approved", quote)
	run(1, "worker_forbidden", "config", "unset", "autonomy.yolo", "--project", "shop", "--user-approved", quote)
	run(1, "worker_forbidden", "config", "set", "defaults.max_workers", "6", "--project", "shop", "--user-approved", quote)
	t.Setenv("HERDR_PANE_ID", "")
	run(0, "", "config", "set", "autonomy.yolo", "true", "--project", "shop")
	run(0, "", "config", "unset", "autonomy.yolo", "--project", "shop")
	run(1, "config_invalid", "config", "unset", "defaults.gate")
	run(1, "config_invalid", "config", "set", "gate", `['make check']`)
	run(1, "config_invalid", "config", "unset", "gate")
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM config_approvals`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("unexpected approval count = %d, %v", count, err)
	}
	before, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `CREATE TRIGGER reject_config_approval BEFORE INSERT ON config_approvals BEGIN SELECT RAISE(ABORT, 'approval blocked'); END`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	run(1, "approval blocked", "config", "set", "autonomy.yolo", "true", "--project", "shop", "--user-approved", quote)
	after, err := os.ReadFile(projectPath)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("failed approval changed config: before=%q after=%q err=%v", before, after, err)
	}
}

func TestConfigSetWarnsWhenSelectingNoMistakesPolicies(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProject(context.Background(), "shop", repo, "main"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	output, errorsOut := &bytes.Buffer{}, &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	t.Chdir(repo)
	installReadyNoMistakes(t)
	for _, key := range []string{"defaults.merge_mode", "repositories.api.merge_mode", "defaults.review", "repositories.api.review"} {
		output.Reset()
		errorsOut.Reset()
		if code := cli.Run([]string{"config", "set", key, "no-mistakes", "--project", "shop"}); code != 0 || !strings.Contains(output.String()+errorsOut.String(), "deprecated") {
			t.Fatalf("config set %s no-mistakes: code=%d stdout=%s stderr=%s", key, code, output, errorsOut)
		}
	}
}

func TestProjectAddRegistersWithoutStartingLead(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	service := testService(filepath.Join(root, "posse"), herdr.NewFake())
	output := &bytes.Buffer{}
	cli := service.CLI()
	cli.Out = output
	t.Chdir(repo)
	if code := cli.Run([]string{"project", "add", "--name", "registered"}); code != 0 {
		t.Fatalf("project add exit=%d output=%s", code, output)
	}
	db, err := store.Open(filepath.Join(root, "posse"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.ProjectByName(context.Background(), "registered")
	if err != nil || project.LeadPaneID != "" || project.HerdrWorkspaceID != "" {
		t.Fatalf("project add started or did not register a Lead: %#v, %v", project, err)
	}
}

func TestNextTaskSequenceIgnoresBranchNamesAndSkipsTaskDirectories(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	gitTest(t, repo, "branch", "posse/t1")
	gitTest(t, repo, "branch", "posse/t3")
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "projects", "shop", "tasks", "t1"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, sequence, err := createTaskWithSequence(context.Background(), db, project, home, store.Task{Type: "research", Title: "Sequence", ShortName: "sequence", MergeMode: "local"})
	if err != nil || sequence != 2 {
		t.Fatalf("next safe sequence = %d, %v; want 2", sequence, err)
	}
}

func TestReadAndSignalContinueWhenHerdrIsDown(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 1, Type: "research", Title: "Offline", MergeMode: "local", PaneID: "w1:p2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.Server.Running = false
	service := testService(home, fake)
	output, errorsOut := &bytes.Buffer{}, &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	t.Chdir(repo)
	t.Setenv("HERDR_PANE_ID", "")
	if code := cli.Run([]string{"list"}); code != 0 {
		t.Fatalf("read-only list failed while Herdr was down: code=%d out=%s err=%s", code, output, errorsOut)
	}
	output.Reset()
	t.Setenv("HERDR_PANE_ID", "w1:p2")
	if code := cli.Run([]string{"signal", "needs-decision", "blocked offline"}); code != 0 {
		t.Fatalf("Worker Signal failed while Herdr was down: code=%d out=%s err=%s", code, output, errorsOut)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	task, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil || task.State != store.StateNeedsDecision {
		t.Fatalf("offline Signal did not persist state: %#v, %v", task, err)
	}
	notices, err := db.UndeliveredNotices(context.Background(), project.ID)
	if err != nil || len(notices) != 1 {
		t.Fatalf("offline Signal did not queue its Notice: %#v, %v", notices, err)
	}
}

func TestContextPrintsOnlyForRecordedPane(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "posse")
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 1, Type: "research", Title: "Worker context", MergeMode: "local", PaneID: "w1:p2"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	adapter := herdr.NewFake()
	adapter.SnapshotValue.Panes = []herdr.Pane{{PaneID: "w1:p1", Agent: "claude", AgentStatus: "running"}}
	service := testService(home, adapter)
	output, errorsOut := &bytes.Buffer{}, &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	t.Chdir(repo)
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	if code := cli.Run([]string{"_context"}); code != 0 {
		t.Fatalf("context failed: %s", errorsOut)
	}
	if got, want := output.String(), "role: lead\nproject: shop\nnext: Run `posse lead` to read the Lead protocol and continue\n"; got != want {
		t.Fatalf("Lead context = %q, want %q", got, want)
	}
	output.Reset()
	t.Setenv("HERDR_PANE_ID", "w1:p2")
	if code := cli.Run([]string{"_context"}); code != 0 {
		t.Fatalf("Worker context failed: %s", errorsOut)
	}
	if got, want := output.String(), "role: worker\nproject: shop\ntask: t1\nstate: spawning\nnext: Run `posse brief` to read your launch Brief and continue\n"; got != want {
		t.Fatalf("Worker context = %q, want %q", got, want)
	}
	output.Reset()
	t.Setenv("HERDR_PANE_ID", "w9:p9")
	if code := cli.Run([]string{"_context"}); code != 0 || output.Len() != 0 {
		t.Fatalf("unrecorded pane received context: code=%d output=%s", code, output)
	}
}

func TestConfigSchemaRunsThroughCLIInToonAndJSON(t *testing.T) {
	service := testService(t.TempDir(), nil)
	output, errorsOut := &bytes.Buffer{}, &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	if code := cli.Run([]string{"config", "schema"}); code != 0 {
		t.Fatalf("config schema TOON exit=%d stdout=%q stderr=%q", code, output, errorsOut)
	}
	if !strings.Contains(output.String(), "notice_delivery") || !strings.Contains(output.String(), "claude: watch") || !strings.Contains(output.String(), "codex: codex-queue") {
		t.Fatalf("config schema TOON omitted typed map default: %s", output)
	}
	output.Reset()
	if code := cli.Run([]string{"config", "schema", "--json"}); code != 0 {
		t.Fatalf("config schema JSON exit=%d stdout=%q stderr=%q", code, output, errorsOut)
	}
	var decoded struct {
		Keys []struct {
			Key      string          `json:"key"`
			Default  json.RawMessage `json:"default"`
			UserOnly bool            `json:"user_only"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatalf("config schema JSON invalid: %v: %s", err, output)
	}
	userOnly := map[string]bool{}
	for _, row := range decoded.Keys {
		userOnly[row.Key] = row.UserOnly
	}
	for key, want := range map[string]bool{"review_loop.max_rounds": true, "gate_loop.max_fix_rounds": false, "defaults.gate": false, "ci_loop.max_fix_rounds": true, "pool.setup": true, "kinds.<kind>.lead_auto_approve": true, "autonomy.yolo": true, "autonomy.review": true, "autonomy.merge": true, "defaults.merge_mode": false} {
		if userOnly[key] != want {
			t.Fatalf("config schema user_only for %s = %v, want %v", key, userOnly[key], want)
		}
	}
	for _, row := range decoded.Keys {
		if row.Key != "kinds.<kind>.notice_delivery" {
			continue
		}
		var defaults map[string]string
		if err := json.Unmarshal(row.Default, &defaults); err != nil || defaults["claude"] != "watch" || defaults["pi"] != "pi-extension" || defaults["opencode"] != "opencode-plugin" {
			t.Fatalf("notice_delivery JSON default = %s, %v", row.Default, err)
		}
		return
	}
	t.Fatal("config schema JSON omitted notice_delivery")
}

func TestContextSuppressesDatabaseErrors(t *testing.T) {
	for name, contents := range map[string][]byte{"corrupt": []byte("not a sqlite database"), "missing tables": nil} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, "posse.db"), contents, 0o600); err != nil {
				t.Fatal(err)
			}
			service := testService(home, nil)
			output, errorsOut := &bytes.Buffer{}, &bytes.Buffer{}
			cli := service.CLI()
			cli.Out, cli.ErrOut = output, errorsOut
			t.Setenv("HERDR_PANE_ID", "pane-with-no-context")
			if code := cli.Run([]string{"_context"}); code != 0 || output.Len() != 0 || errorsOut.Len() != 0 {
				t.Fatalf("_context leaked a database error: code=%d output=%q stderr=%q", code, output, errorsOut)
			}
		})
	}
}

func TestContextDoesNotCallRecordedButMissingLeadLive(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProject(context.Background(), "shop", repo, "main"); err != nil {
		t.Fatal(err)
	}
	project, err := db.ProjectByRoot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	output := &bytes.Buffer{}
	cli := service.CLI()
	cli.Out = output
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	if code := cli.Run([]string{"_context"}); code != 0 || output.Len() != 0 {
		t.Fatalf("_context claimed an unverified Lead: code=%d output=%s", code, output)
	}
}

func TestRecoverRebuildIsUserOnlyAndFindsOrphanedSandboxes(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	worktree := filepath.Join(root, "orphan-sandbox")
	gitTest(t, repo, "worktree", "add", "-b", "posse/t2", worktree, "main")
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 1, Type: "research", Title: "Saved", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	service := testService(home, herdr.NewFake())
	output, errorsOut := &bytes.Buffer{}, &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	t.Chdir(repo)
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	if code := cli.Run([]string{"recover", "--rebuild"}); code != 1 || !strings.Contains(output.String()+errorsOut.String(), "user_only") {
		t.Fatalf("non-User rebuild was allowed: code=%d out=%s err=%s", code, output, errorsOut)
	}
	output.Reset()
	errorsOut.Reset()
	t.Setenv("HERDR_PANE_ID", "")
	if code := cli.Run([]string{"recover", "--rebuild"}); code != 0 || !strings.Contains(output.String(), "rebuilt_tasks: 2") {
		t.Fatalf("User rebuild failed: code=%d out=%s err=%s", code, output, errorsOut)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects, err := db.Projects(context.Background())
	if err != nil || len(projects) != 1 {
		t.Fatalf("rebuilt projects = %#v, %v", projects, err)
	}
	original, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil || original.State != store.StateWorking {
		t.Fatalf("snapshot Task was not restored: %#v, %v", original, err)
	}
	orphan, err := db.Task(context.Background(), project.ID, "t2")
	if err != nil || orphan.State != store.StateLost || orphan.Branch != "posse/t2" || filepath.Clean(orphan.WorktreePath) != filepath.Clean(worktree) {
		t.Fatalf("orphaned Sandbox was not recovered: %#v, %v", orphan, err)
	}
}
