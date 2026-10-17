package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/execution"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func TestInPlaceOutputTracksOnlyPostAdmissionChangesAndNeverResetsSource(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{"README.md": "pre-existing dirty edit\n", "src/existing.go": "package src\n", ".git/config": "[core]\n"} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plan := taskplan.Plan{Execution: taskplan.Execution{Mode: "in-place", Scope: "directory", Paths: []string{"src"}}, Paths: taskplan.Paths{Output: filepath.Join(t.TempDir(), "tasks", "t1")}}
	baseline, err := scanInPlaceFiles(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range baseline.Files {
		if file.Path == ".git/config" {
			t.Fatal("baseline included Git metadata")
		}
	}
	baseline.Scopes = []string{"src"}
	baseline.Digest = inPlaceSnapshotDigest(baseline.Files, baseline.Scopes)
	encoded, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(inPlaceBaselinePath(plan), encoded); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src/existing.go"), []byte("package src\n\nfunc Changed() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src/new.go"), []byte("package src\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := captureInPlaceOutput(ctx, plan, root)
	if err != nil {
		t.Fatal(err)
	}
	if output.BaselineSHA256 != baseline.Digest || output.OutputSHA256 == "" || len(output.ChangedFiles) != 2 || output.ChangedFiles[0].Path != "src/existing.go" || output.ChangedFiles[1].Path != "src/new.go" {
		t.Fatalf("completion evidence = %#v", output)
	}
	if err := os.Mkdir(filepath.Join(root, "empty-outside"), 0o700); err != nil {
		t.Fatal(err)
	}
	emptyOutput, err := captureInPlaceOutput(ctx, plan, root)
	var emptyScopeFailure *axi.Error
	if !errors.As(err, &emptyScopeFailure) || emptyScopeFailure.Code != "in_place_scope_violation" || emptyOutput.ScopeViolation == "" || len(emptyOutput.ChangedFiles) != 3 || emptyOutput.ChangedFiles[0].Path != "empty-outside" {
		t.Fatalf("empty out-of-scope directory was not captured: output=%#v err=%v", emptyOutput, err)
	}
	if err := os.Remove(filepath.Join(root, "empty-outside")); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(root, "README.md")); err != nil || string(contents) != "pre-existing dirty edit\n" {
		t.Fatalf("pre-existing edit changed: %q, %v", contents, err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("outside-scope edit remains\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = captureInPlaceOutput(ctx, plan, root)
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "in_place_scope_violation" {
		t.Fatalf("out-of-scope edit error = %v", err)
	}
	if contents, readErr := os.ReadFile(filepath.Join(root, "README.md")); readErr != nil || string(contents) != "outside-scope edit remains\n" {
		t.Fatalf("out-of-scope edit was rolled back: %q, %v", contents, readErr)
	}
	persisted, err := os.ReadFile(inPlaceOutputPath(plan))
	if err != nil {
		t.Fatal(err)
	}
	var recorded inPlaceOutput
	if err := json.Unmarshal(persisted, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.OutputSHA256 == "" || recorded.ScopeViolation == "" || len(recorded.ChangedFiles) != 3 {
		t.Fatalf("scope violation evidence was not retained: %#v", recorded)
	}
}

func TestInPlaceRootCannotBeReplacedBySymlink(t *testing.T) {
	parent := t.TempDir()
	root := t.TempDir()
	link := filepath.Join(parent, "project")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := validateInPlaceRoot(link, root); err == nil {
		t.Fatal("registered Project root symlink was accepted")
	}
	if _, err := scanInPlaceFiles(context.Background(), link); err == nil {
		t.Fatal("in-place scanner followed a root symlink")
	}
}

func TestInPlaceSymlinksCannotEscapeLeasedPaths(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"src", "other"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../other", filepath.Join(root, "src", "linked")); err != nil {
		t.Fatal(err)
	}
	files, err := scanInPlaceFiles(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateInPlaceSymlinks(root, []string{"src"}, files.Files); err == nil {
		t.Fatal("symlink escaped its leased scope")
	}
	if err := os.Remove(filepath.Join(root, "src", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("data", filepath.Join(root, "src", "linked")); err != nil {
		t.Fatal(err)
	}
	files, err = scanInPlaceFiles(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateInPlaceSymlinks(root, []string{"src"}, files.Files); err != nil {
		t.Fatalf("symlink within scope was refused: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../.git", filepath.Join(root, "src", "metadata")); err != nil {
		t.Fatal(err)
	}
	if _, err := canonicalInPlaceScopes(root, []string{"src/metadata"}); err == nil {
		t.Fatal("scope alias resolving to Git metadata was accepted")
	}
}

func TestInPlaceScopeSymlinkTargetCannotChangeAfterAdmission(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, directory := range []string{"docs", "tests"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, directory, "file.txt"), []byte(directory+" baseline\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("docs", filepath.Join(root, "src")); err != nil {
		t.Fatal(err)
	}
	baseline, err := scanInPlaceFiles(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalInPlaceScopes(root, []string{"src"})
	if err != nil {
		t.Fatal(err)
	}
	baseline.Scopes, err = relativeInPlaceScopes(root, canonical)
	if err != nil {
		t.Fatal(err)
	}
	baseline.Digest = inPlaceSnapshotDigest(baseline.Files, baseline.Scopes)
	plan := taskplan.Plan{Execution: taskplan.Execution{Mode: "in-place", Scope: "directory", Paths: []string{"src"}}, Paths: taskplan.Paths{Output: filepath.Join(t.TempDir(), "tasks", "t1")}}
	encoded, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(inPlaceBaselinePath(plan), encoded); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "src")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tests", filepath.Join(root, "src")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tests", "file.txt"), []byte("changed through redirected scope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateInPlacePlanScopes(root, plan.Execution.Paths, baseline.Scopes); err == nil {
		t.Fatal("relaunch accepted a scope symlink redirected after admission")
	}
	output, err := captureInPlaceOutput(ctx, plan, root)
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "in_place_scope_violation" || output.ScopeViolation == "" {
		t.Fatalf("redirected scope was not recorded as a violation: output=%#v err=%v", output, err)
	}
}

func TestInPlaceScopeNoticesDeduplicateEvidence(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateDirectoryProject(ctx, "local", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Scoped change", ShortName: "scoped-change", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	output := inPlaceOutput{OutputSHA256: "same-evidence", ScopeViolation: "changed docs outside src"}
	for range 2 {
		if err := recordInPlaceScopeNotice(ctx, db, project, task, output); err != nil {
			t.Fatal(err)
		}
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "in_place_scope_violation" {
		t.Fatalf("duplicate evidence notices = %#v, %v", notices, err)
	}
	if _, err := db.AckNotices(ctx, project.ID, []string{strconv.FormatInt(notices[0].ID, 10)}); err != nil {
		t.Fatal(err)
	}
	if err := recordInPlaceScopeNotice(ctx, db, project, task, output); err != nil {
		t.Fatal(err)
	}
	allNotices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(allNotices) != 1 {
		t.Fatalf("acknowledged evidence notice was repeated: %#v, %v", allNotices, err)
	}
	output.OutputSHA256 = "new-evidence"
	if err := recordInPlaceScopeNotice(ctx, db, project, task, output); err != nil {
		t.Fatal(err)
	}
	notices, err = db.Notices(ctx, project.ID, true)
	if err != nil || len(notices) != 1 {
		t.Fatalf("new evidence notice was not recorded: %#v, %v", notices, err)
	}
}

func TestInPlaceTeardownDoesNotClaimTheLeadWorkspace(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	project := store.Project{ID: 1, Name: "local", Root: root, Kind: store.ProjectKindDirectory, HerdrWorkspaceID: "w1", LeadPaneID: "w1:p1", LeadLabel: "posse:local:lead"}
	task := store.Task{ID: 2, Seq: 1, ShortName: "edit-files", WorktreePath: root, PaneLabel: "posse:local:t1", AgentName: agentName(project.Name, 1, 1), HerdrWorkspaceID: "w1"}
	workspace := herdr.Workspace{WorkspaceID: "w1", Root: root}
	workspace.Worktree.CheckoutPath = root
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{
		Workspaces: []herdr.Workspace{workspace},
		Tabs:       []herdr.Tab{{TabID: "t1", WorkspaceID: "w1", Label: "Lead"}, {TabID: "t2", WorkspaceID: "w1", Label: task.ShortName}},
		Panes: []herdr.Pane{
			{PaneID: "w1:p1", WorkspaceID: "w1", TabID: "t1", Label: project.LeadLabel, CWD: root, Agent: "claude"},
			{PaneID: "w1:p2", WorkspaceID: "w1", TabID: "t2", Label: task.PaneLabel, CWD: root, Agent: "claude"},
		},
		Agents: []herdr.Agent{{Name: task.AgentName, PaneID: "w1:p2"}},
	}
	service := testService(home, fake)
	plan, err := service.planTaskPaneTeardown(context.Background(), project, task)
	if err != nil {
		t.Fatal(err)
	}
	if plan.workspaces["w1"] || len(plan.panes.Foreign) != 0 || !plan.paneIDs["w1:p2"] {
		t.Fatalf("in-place teardown claimed the Lead workspace: %#v", plan)
	}
}

func TestInPlaceRiderCannotStartProjectLead(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	projectRoot := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(projectRoot, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	harness := newUpHarness(t, root, projectRoot)
	db, err := store.Open(harness.home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateDirectoryProject(ctx, "source", projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	briefBytes := []byte("---\ntype: ship\ntitle: Edit source\ndone_when: checks pass\nexecution: in-place\npaths: [src]\n---\n")
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief:     taskplan.Select("brief", "brief.md", briefBytes),
		Execution: taskplan.Execution{Mode: "mount", Scope: "directory"},
		Launch:    taskplan.Launch{ProfileName: "deep", Profile: config.Profile{Kind: "pi", Model: "model"}, Args: []string{"--model", "model"}, ModelKnown: true, ConfiguredModel: "model"},
		Review:    "none", Delivery: "local", Autonomy: config.Autonomy{Review: "ask", Land: "ask"},
		Paths:      taskplan.Paths{Scratch: filepath.Join(harness.home, "scratch", "source", "tasks", "t1"), Output: filepath.Join(harness.home, "projects", "source", "tasks", "t1")},
		UserChoice: &taskplan.UserChoice{Quote: "I approve edits to src", Actor: "lead", Source: "ride --user-approved"},
	})
	if err != nil {
		t.Fatal(err)
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Edit source", ShortName: "edit-source", PlanJSON: string(planJSON), WorktreePath: projectRoot, LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTaskLaunch(ctx, taskID, projectRoot, "w1", "w1:p1", "posse:source:t1", "posse-source-t1-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if code := harness.run("up", "--claude"); code == 0 || !strings.Contains(harness.out.String(), "worker_forbidden") {
		t.Fatalf("CLI did not refuse `up` from an in-place Rider: code=%d output=%s", code, harness.out.String())
	}
	var output strings.Builder
	err = harness.service.upCore(&axi.Context{Context: ctx, Out: &output, ErrOut: &output}, []string{"--claude"})
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "lead_only" {
		t.Fatalf("up handler did not refuse an in-place Rider pane: %v output=%s", err, output.String())
	}
	t.Setenv("HERDR_PANE_ID", "w1:p3")
	harness.fake.SnapshotValue.Panes = append(harness.fake.SnapshotValue.Panes, herdr.Pane{PaneID: "w1:p3", WorkspaceID: "w1"})
	if code := harness.run("down"); code == 0 || !strings.Contains(harness.out.String(), "tasks_open") {
		t.Fatalf("User shell was mistaken for the in-place Rider: code=%d output=%s", code, harness.out.String())
	}
}

func TestInPlaceLeasesPersistAcrossReopenAndRefuseOverlappingScopes(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	root := filepath.Join(home, "source")
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateDirectoryProject(ctx, "source", root)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateProject(ctx, "nested", filepath.Join(root, "src", "nested"), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Edit source", ShortName: "edit-source", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	plan := taskplan.Plan{Execution: taskplan.Execution{Mode: "in-place", Scope: "directory", Paths: []string{"src"}}}
	if err := ensureInPlaceLeases(ctx, db, project, task, plan); err != nil {
		t.Fatal(err)
	}
	leases, err := db.ResourceLeases(ctx, project.ID)
	if err != nil || len(leases) != 1 || leases[0].OwnerTaskID != task.ID || leases[0].ScopePath != filepath.Join(root, "src") {
		t.Fatalf("in-place lease = %#v, %v", leases, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = execution.NewManager(db).AcquirePath(ctx, filepath.Join(root, "src", "nested"), store.ResourceLeaseRequest{
		OwnerProjectID: other.ID, Owner: "overlapping Task", SourceKey: "overlapping-task",
	})
	if !errors.Is(err, store.ErrResourceConflict) {
		t.Fatalf("overlapping scope after reopen = %v, want persisted conflict", err)
	}
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	task, err = db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureInPlaceLeases(ctx, db, project, task, plan); err != nil {
		t.Fatalf("relaunch could not reuse its durable claim: %v", err)
	}
	leases, err = db.ResourceLeases(ctx, project.ID)
	if err != nil || len(leases) != 1 || leases[0].SourceKey != inPlaceSourceKey(task.ID, filepath.Join(root, "src")) {
		t.Fatalf("relaunch duplicated or changed the claim: %#v, %v", leases, err)
	}
}

func TestInPlaceRideCompletesGitAndPlainDirectoriesWithoutCommitOrLand(t *testing.T) {
	for _, withGit := range []bool{false, true} {
		name := "plain directory"
		if withGit {
			name = "Git directory"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			directory := filepath.Join(root, "project")
			if err := os.MkdirAll(filepath.Join(directory, "src"), 0o700); err != nil {
				t.Fatal(err)
			}
			if withGit {
				initRepo(t, directory)
			}
			preexisting := filepath.Join(directory, "README.md")
			if err := os.WriteFile(preexisting, []byte("pre-existing dirty change\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			headBefore := ""
			if withGit {
				headBefore = strings.TrimSpace(gitTest(t, directory, "rev-parse", "HEAD"))
			}
			home := filepath.Join(root, "posse")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			configText := "[defaults]\nlanding_mode = \"pr\"\ngate = [\"test -f src/created.txt\"]\n\n[profiles.fast]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"fast\"\n"
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
				t.Fatal(err)
			}
			briefPath := filepath.Join(root, "brief.md")
			brief := "---\ntype: ship\ntitle: Add a local file\ndone_when: the approved check passes\nexecution: in-place\npaths: [src]\n---\nCreate src/created.txt.\n"
			if err := os.WriteFile(briefPath, []byte(brief), 0o600); err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			project, err := db.CreateDirectoryProject(ctx, "project", directory)
			if err == nil {
				err = db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:project:lead")
			}
			if closeErr := db.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				t.Fatal(err)
			}
			fake := herdr.NewFake()
			fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:project:lead", Agent: "claude", AgentStatus: "idle"}}}
			fake.Results["tab.create"] = json.RawMessage(`{"tab":{"tab_id":"w1:t2"},"root_pane":{"pane_id":"w1:p2","tab_id":"w1:t2"}}`)
			fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
			service := testService(home, fake)
			t.Chdir(directory)
			t.Setenv("HERDR_ENV", "1")
			t.Setenv("HERDR_PANE_ID", "w1:p1")
			t.Setenv("HERDR_WORKSPACE_ID", "w1")
			var output strings.Builder
			cli := service.CLI()
			cli.Out, cli.ErrOut = &output, &output
			if code := cli.Run([]string{"ride", "--brief", briefPath, "--name", "add-local-file"}); code == 0 || !strings.Contains(output.String(), "in_place_consent_required") {
				t.Fatalf("unconsented ride = %d\n%s", code, output.String())
			}
			if fake.CallCount("tab.create") != 0 {
				t.Fatalf("unconsented ride launched a Rider: %#v", fake.Calls)
			}
			db, err = store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			unconsentedTasks, err := db.Tasks(ctx, project.ID, true)
			_ = db.Close()
			if err != nil || len(unconsentedTasks) != 0 {
				t.Fatalf("unconsented ride created Tasks: %#v, %v", unconsentedTasks, err)
			}
			for _, unsupported := range []struct {
				name, title, brief, code string
			}{
				{"PR delivery", "Publish the local change", "---\ntype: ship\ntitle: Publish the local change\ndone_when: checks pass\nexecution: in-place\npaths: [src]\nlanding_mode: pr\n---\n", "in_place_delivery_unsupported"},
				{"ticket closure", "Close the ticket", "---\ntype: ship\ntitle: Close the ticket\ndone_when: checks pass\nexecution: in-place\npaths: [src]\nticket: 148\n---\n", "in_place_ticket_unsupported"},
				{"issue references", "Add an issue reference", "---\ntype: ship\ntitle: Add an issue reference\ndone_when: checks pass\nexecution: in-place\npaths: [src]\nrefs: [18]\n---\n", "in_place_ticket_unsupported"},
				{"Review", "Review a branch", "---\ntype: review\ntitle: Review a branch\ndone_when: report is saved\nreview_of: t1\nexecution: in-place\npaths: [src]\n---\n", "in_place_review_unsupported"},
				{"Review policy", "Review local edits", "---\ntype: ship\ntitle: Review local edits\ndone_when: checks pass\nexecution: in-place\npaths: [src]\nreview: local\n---\n", "in_place_review_unsupported"},
				{"Git ref", "Edit from a base ref", "---\ntype: ship\ntitle: Edit from a base ref\ndone_when: checks pass\nexecution: in-place\npaths: [src]\nbase: main\n---\n", "in_place_git_scope_unsupported"},
				{"no-mistakes", "Run no-mistakes", "---\ntype: ship\ntitle: Run no-mistakes\ndone_when: checks pass\nexecution: in-place\npaths: [src]\nlanding_mode: no-mistakes\n---\n", "in_place_delivery_unsupported"},
			} {
				unsupportedPath := filepath.Join(root, "unsupported-brief.md")
				if err := os.WriteFile(unsupportedPath, []byte(unsupported.brief), 0o600); err != nil {
					t.Fatal(err)
				}
				output.Reset()
				name := taskTitleSlug(unsupported.title)
				if code := cli.Run([]string{"ride", "--brief", unsupportedPath, "--name", name, "--user-approved", "I approve edits under src"}); code == 0 || !strings.Contains(output.String(), unsupported.code) {
					t.Fatalf("unsupported %s ride = %d\n%s", unsupported.name, code, output.String())
				}
				if fake.CallCount("tab.create") != 0 {
					t.Fatalf("unsupported %s ride launched a Rider: %#v", unsupported.name, fake.Calls)
				}
			}
			db, err = store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			unsupportedTasks, err := db.Tasks(ctx, project.ID, true)
			_ = db.Close()
			if err != nil || len(unsupportedTasks) != 0 {
				t.Fatalf("incompatible requests created Tasks: %#v, %v", unsupportedTasks, err)
			}
			output.Reset()
			if code := cli.Run([]string{"ride", "--brief", briefPath, "--name", "add-local-file", "--user-approved", "I approve edits under src"}); code != 0 {
				t.Fatalf("ride: %d\n%s", code, output.String())
			}
			output.Reset()
			if code := cli.Run([]string{"holler", "working", "Lead must not impersonate the Rider"}); code == 0 || !strings.Contains(output.String(), "worker_task_unknown") {
				t.Fatalf("Lead signal in the shared in-place directory = %d\n%s", code, output.String())
			}
			output.Reset()
			db, err = store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			tasks, err := db.Tasks(ctx, project.ID, true)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("Tasks = %#v, %v", tasks, err)
			}
			task := tasks[0]
			if task.State != store.StateWorking || task.Branch != "" || task.MountID != 0 || filepath.Clean(task.WorktreePath) != directory {
				t.Fatalf("in-place Task custody = %#v", task)
			}
			mounts, err := db.Mounts(ctx, project.ID)
			if err != nil || len(mounts) != 0 {
				t.Fatalf("in-place ride created Mounts: %#v, %v", mounts, err)
			}
			plan, err := task.AdmittedPlan()
			if err != nil || plan == nil {
				t.Fatalf("admitted in-place plan = %#v, %v", plan, err)
			}
			if _, err := os.Stat(inPlaceBaselinePath(*plan)); err != nil {
				t.Fatalf("baseline missing before agent launch: %v", err)
			}
			leases, err := db.ResourceLeases(ctx, project.ID)
			if err != nil || len(leases) != 1 || leases[0].OwnerTaskID != task.ID || leases[0].ScopePath != filepath.Join(directory, "src") {
				t.Fatalf("in-place path lease = %#v, %v", leases, err)
			}
			fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{PaneID: "w1:p2", WorkspaceID: "w1", Label: task.PaneLabel, Agent: "claude", AgentStatus: "working"})
			fake.Results["pane.read"] = json.RawMessage(`{"text":"Rider working"}`)
			t.Setenv("HERDR_PANE_ID", "w1:p2")
			output.Reset()
			if code := cli.Run([]string{"publish", "Attempt to publish in-place changes"}); code == 0 || !strings.Contains(output.String(), "publish_refused") {
				t.Fatalf("in-place Rider publish = %d\n%s", code, output.String())
			}
			output.Reset()
			if code := cli.Run([]string{"holler", "needs-decision", "Attach findings", "--findings", "README.md"}); code == 0 || !strings.Contains(output.String(), "in-place findings must come from this Task's scratch directory") {
				t.Fatalf("in-place findings accepted from source root = %d\n%s", code, output.String())
			}
			t.Setenv("HERDR_PANE_ID", "w1:p1")
			nestedPath := filepath.Join(directory, "src", "nested")
			if err := os.MkdirAll(nestedPath, 0o700); err != nil {
				t.Fatal(err)
			}
			overlapBrief := filepath.Join(root, "overlap-brief.md")
			if err := os.WriteFile(overlapBrief, []byte(`---
type: ship
title: Add overlapping file
done_when: nested file exists
execution: in-place
paths: [src/nested]
---
Create the nested file.
`), 0o600); err != nil {
				t.Fatal(err)
			}
			output.Reset()
			if code := cli.Run([]string{"ride", "--brief", overlapBrief, "--name", "add-overlapping-file", "--user-approved", "I approve edits under src/nested"}); code == 0 || !strings.Contains(output.String(), "resource_conflict") {
				t.Fatalf("overlapping in-place ride = %d\n%s", code, output.String())
			}
			parentTasks, err := db.Tasks(ctx, project.ID, true)
			if err != nil || len(parentTasks) != 2 || parentTasks[1].State != store.StateFailed || parentTasks[1].Branch != "" || fake.CallCount("tab.create") != 1 {
				t.Fatalf("overlap failure launched a Rider or lost its Task record: %#v calls=%#v err=%v", parentTasks, fake.Calls, err)
			}
			t.Setenv("HERDR_PANE_ID", "")
			output.Reset()
			if code := cli.Run([]string{"project", "show"}); code != 0 {
				t.Fatalf("User shell in shared in-place directory was treated as a Rider: %d\n%s", code, output.String())
			}
			t.Setenv("HERDR_PANE_ID", "w1:p1")
			if fake.CallCount("worktree.open") != 0 || fake.CallCount("tab.create") != 1 {
				t.Fatalf("in-place Rider placement = %#v", fake.Calls)
			}
			t.Setenv("HERDR_PANE_ID", "w1:p2")
			output.Reset()
			if code := cli.Run([]string{"holler", "done", "The approved check must pass"}); code == 0 || !strings.Contains(output.String(), "in_place_check_failed") {
				t.Fatalf("failed approved check was not refused: %d\n%s", code, output.String())
			}
			failedCheckData, err := os.ReadFile(inPlaceOutputPath(*plan))
			if err != nil {
				t.Fatalf("failed check did not persist output evidence: %v", err)
			}
			var failedCheckOutput inPlaceOutput
			if err := json.Unmarshal(failedCheckData, &failedCheckOutput); err != nil || len(failedCheckOutput.ChangedFiles) != 1 || failedCheckOutput.ChangedFiles[0].Path != "src/nested" || failedCheckOutput.CurrentSHA256 == "" {
				t.Fatalf("failed check evidence = %#v, %v", failedCheckOutput, err)
			}
			t.Setenv("HERDR_PANE_ID", "w1:p1")

			if err := os.WriteFile(filepath.Join(directory, "src", "created.txt"), []byte("new source\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			fake.SnapshotValue.Panes = fake.SnapshotValue.Panes[:1]
			if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateLost, "cli", "simulated interrupted in-place Rider"); err != nil {
				t.Fatal(err)
			}
			if err := captureLostInPlaceEvidence(ctx, db, project); err != nil {
				t.Fatalf("capture interrupted in-place output: %v", err)
			}
			if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "task_lost", Summary: "interrupted"}); err != nil {
				t.Fatal(err)
			}
			if err := service.raiseNoticeDecisions(ctx, db, project); err != nil {
				t.Fatal(err)
			}
			decisions, err := db.Decisions(ctx, project.ID, true)
			var recovery *store.Decision
			for i := range decisions {
				if decisions[i].TaskID == task.ID {
					recovery = &decisions[i]
					break
				}
			}
			if err != nil || recovery == nil || strings.Join(recovery.Options, ",") != "relaunch,preserve" {
				t.Fatalf("in-place recovery options = %#v, %v", decisions, err)
			}
			output.Reset()
			if code := cli.Run([]string{"unsaddle", "t2"}); code == 0 || !strings.Contains(output.String(), "decision_required") {
				t.Fatalf("failed in-place Task stopped without User choice: %d\n%s", code, output.String())
			}
			var overlapRecovery *store.Decision
			for i := range decisions {
				if decisions[i].TaskID == parentTasks[1].ID {
					overlapRecovery = &decisions[i]
					break
				}
			}
			if overlapRecovery == nil {
				t.Fatalf("overlapping Task has no recovery Decision: %#v", decisions)
			}
			if _, err := db.AnswerDecision(ctx, project.ID, overlapRecovery.ID, "preserve", "Stop the failed attempt and keep all source changes"); err != nil {
				t.Fatal(err)
			}
			output.Reset()
			if code := cli.Run([]string{"apply", strconv.FormatInt(overlapRecovery.ID, 10)}); code != 0 {
				t.Fatalf("apply in-place preserve Decision: %d\n%s", code, output.String())
			}
			overlapTask, err := db.TaskByID(ctx, project.ID, parentTasks[1].ID)
			if err != nil || overlapTask.State != store.StateTornDown {
				t.Fatalf("preserve Decision did not stop the failed Task: %#v, %v", overlapTask, err)
			}
			overlapTransitions, err := db.TaskTransitions(ctx, overlapTask.ID, 20)
			if err != nil {
				t.Fatal(err)
			}
			approvalRecorded := false
			for _, transition := range overlapTransitions {
				if strings.Contains(transition.Note, "Stop the failed attempt and keep all source changes") {
					approvalRecorded = true
					break
				}
			}
			if !approvalRecorded {
				t.Fatalf("preserve approval quote was not recorded: %#v", overlapTransitions)
			}
			leases, err = db.ResourceLeases(ctx, project.ID)
			if err != nil || len(leases) != 1 || leases[0].OwnerTaskID != task.ID {
				t.Fatalf("preserve Decision released another Task's path lease: %#v, %v", leases, err)
			}
			lostOutputData, err := os.ReadFile(inPlaceOutputPath(*plan))
			if err != nil {
				t.Fatal(err)
			}
			var lostOutput inPlaceOutput
			if err := json.Unmarshal(lostOutputData, &lostOutput); err != nil || len(lostOutput.ChangedFiles) != 2 || lostOutput.ChangedFiles[0].Path != "src/created.txt" || lostOutput.ChangedFiles[1].Path != "src/nested" {
				t.Fatalf("interrupted output evidence = %#v, %v", lostOutput, err)
			}
			project, err = db.ProjectByID(ctx, project.ID)
			if err != nil {
				t.Fatal(err)
			}
			task, err = db.TaskByID(ctx, project.ID, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(home, project.Name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.relaunchTask(ctx, db, home, project, cfg, task, ""); err != nil {
				t.Fatalf("relaunch in-place Task: %v", err)
			}
			if err := db.ObsoleteResolvedDecisions(ctx, project.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(plan.Paths.Output, "in-place-output-launch-1.json")); err != nil {
				t.Fatalf("relaunch did not preserve the interrupted output manifest: %v", err)
			}
			task, err = db.TaskByID(ctx, project.ID, task.ID)
			if err != nil || task.State != store.StateWorking || task.Branch != "" || filepath.Clean(task.WorktreePath) != directory {
				t.Fatalf("recovered in-place Task = %#v, %v", task, err)
			}
			leases, err = db.ResourceLeases(ctx, project.ID)
			if err != nil || len(leases) != 1 {
				t.Fatalf("recovery lost in-place lease: %#v, %v", leases, err)
			}
			_ = db.Close()
			output.Reset()
			cli.Out, cli.ErrOut = &output, &output
			t.Setenv("HERDR_PANE_ID", "w1:p2")
			if code := cli.Run([]string{"holler", "done", "Created the file without a branch or commit"}); code != 0 {
				t.Fatalf("holler done: %d\n%s", code, output.String())
			}
			db, err = store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			task, err = db.TaskByID(ctx, project.ID, task.ID)
			if err != nil || task.State != store.StateReported || task.Branch != "" {
				t.Fatalf("completed in-place Task = %#v, %v", task, err)
			}
			signals, err := db.TaskSignals(ctx, task.ID, 5)
			if err != nil || len(signals) != 1 {
				t.Fatalf("completion signals = %#v, %v", signals, err)
			}
			output.Reset()
			t.Setenv("HERDR_PANE_ID", "w1:p1")
			if code := cli.Run([]string{"show", "t1", "--full"}); code != 0 || !strings.Contains(output.String(), "in_place_output_history") || !strings.Contains(output.String(), "in_place_output") {
				t.Fatalf("show omitted in-place evidence history: %d\n%s", code, output.String())
			}
			var evidence map[string]any
			if err := json.Unmarshal([]byte(signals[0].DataJSON), &evidence); err != nil {
				t.Fatal(err)
			}
			if evidence["output_digest"] == "" || evidence["baseline_digest"] == "" || len(evidence["changed_files"].([]any)) != 2 || len(evidence["checks"].([]any)) != 1 {
				t.Fatalf("completion evidence = %#v", evidence)
			}
			output.Reset()
			if code := cli.Run([]string{"land", "t1"}); code == 0 || !strings.Contains(output.String(), "land_refused") {
				t.Fatalf("in-place Task Land = %d\n%s", code, output.String())
			}
			if contents, err := os.ReadFile(preexisting); err != nil || string(contents) != "pre-existing dirty change\n" {
				t.Fatalf("pre-existing edit was not preserved: %q, %v", contents, err)
			}
			if withGit {
				headAfter := strings.TrimSpace(gitTest(t, directory, "rev-parse", "HEAD"))
				branch := strings.TrimSpace(gitTest(t, directory, "branch", "--show-current"))
				if headAfter != headBefore || branch != "main" {
					t.Fatalf("in-place execution changed Git history: HEAD %s -> %s, branch %s", headBefore, headAfter, branch)
				}
			}
			if len(leases) != 1 {
				t.Fatal("in-place path lease was released before Teardown")
			}
			_ = db.Close()

			output.Reset()
			cli.Out, cli.ErrOut = &output, &output
			t.Setenv("HERDR_PANE_ID", "w1:p1")
			if code := cli.Run([]string{"unsaddle", "t1", "--discard", "--user-approved", "Discard these source changes"}); code == 0 || !strings.Contains(output.String(), "teardown_refused") {
				t.Fatalf("in-place discard was not refused: %d\n%s", code, output.String())
			}
			output.Reset()
			if code := cli.Run([]string{"unsaddle", "t1"}); code != 0 {
				t.Fatalf("unsaddle: %d\n%s", code, output.String())
			}
			db, err = store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			leases, err = db.ResourceLeases(ctx, project.ID)
			if err != nil || len(leases) != 0 {
				t.Fatalf("Teardown kept in-place leases: %#v, %v", leases, err)
			}
			if contents, err := os.ReadFile(filepath.Join(directory, "src", "created.txt")); err != nil || string(contents) != "new source\n" {
				t.Fatalf("Teardown rolled back in-place source: %q, %v", contents, err)
			}
		})
	}
}
