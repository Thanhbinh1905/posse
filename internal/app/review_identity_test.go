package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

const t144AuthorSession = "99e0a111-1ad9-4010-a1a1-1e2944973bce"

func TestConfiguredModelIdentityFailsClosedOnOverrides(t *testing.T) {
	kind := config.Kind{ModelArgs: []string{"--model", "{model}"}}
	for _, test := range []struct {
		name      string
		profile   config.Profile
		kind      config.Kind
		wantModel string
		wantKnown bool
	}{
		{name: "profile model", profile: config.Profile{Model: "model-a"}, kind: kind, wantModel: "model-a", wantKnown: true},
		{name: "separate model override", profile: config.Profile{Model: "model-a", Args: []string{"--model", "model-b"}}, kind: kind, wantModel: "model-a"},
		{name: "joined model override", profile: config.Profile{Model: "model-a", Args: []string{"--model=model-b"}}, kind: kind, wantModel: "model-a"},
		{name: "Codex alias override", profile: config.Profile{Model: "model-a", Args: []string{"-m", "model-b"}}, kind: kind, wantModel: "model-a"},
		{name: "quoted config override", profile: config.Profile{Kind: "codex", Model: "model-a", Args: []string{"-c", `model="model-a"`}}, kind: config.Kind{ModelArgs: []string{"-c", `model="{model}"`}}, wantModel: "model-a"},
		{name: "malformed model alias", profile: config.Profile{Model: "model-a", Args: []string{"--model"}}, kind: kind, wantModel: "model-a"},
		{name: "effort setting does not override model", profile: config.Profile{Kind: "codex", Model: "model-a"}, kind: config.Kind{ModelArgs: []string{"-c", `model="{model}"`}, EffortArgs: []string{"-c", "model_reasoning_effort={effort}"}}, wantModel: "model-a", wantKnown: true},
		{name: "kind auto arguments can override model", profile: config.Profile{Model: "model-a"}, kind: config.Kind{ModelArgs: kind.ModelArgs, AutoApproveArgs: []string{"--model", "model-b"}}, wantModel: "model-a"},
		{name: "model template must set one model", profile: config.Profile{Model: "model-a"}, kind: config.Kind{ModelArgs: []string{"--model"}}, wantModel: "model-a"},
		{name: "model template cannot replace its own value", profile: config.Profile{Model: "model-a"}, kind: config.Kind{ModelArgs: []string{"--model", "{model}", "--model", "hardcoded"}}, wantModel: "model-a"},
		{name: "unconfigured model", profile: config.Profile{}, kind: kind, wantModel: ""},
		{name: "override without configured model", profile: config.Profile{Args: []string{"--model", "model-b"}}, kind: kind, wantModel: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			kindName := test.profile.Kind
			if kindName == "" {
				kindName = "claude"
			}
			model, known := configuredModelFromLaunchArgs(test.profile.Model, kindName, workerAgentArgs(test.profile, test.kind, ""))
			if model != test.wantModel || known != test.wantKnown {
				t.Fatalf("configured model = %q known=%t, want %q known=%t", model, known, test.wantModel, test.wantKnown)
			}
		})
	}
}

func TestLaunchArgumentAnalysisClassifiesPerHarnessSpellings(t *testing.T) {
	for _, test := range []struct {
		name         string
		kind         string
		args         []string
		wantModels   []string
		wantSessions []string
		wantUnknown  bool
	}{
		{name: "Claude attached resume", kind: "claude", args: []string{"-r" + t144AuthorSession}, wantSessions: []string{t144AuthorSession}},
		{name: "Claude clustered print and resume", kind: "claude", args: []string{"-pr" + t144AuthorSession}, wantSessions: []string{t144AuthorSession}},
		{name: "Claude attached long resume", kind: "claude", args: []string{"--resume=" + t144AuthorSession}, wantSessions: []string{t144AuthorSession}},
		{name: "missing model value does not swallow next option", kind: "claude", args: []string{"--model", "-r" + t144AuthorSession}, wantModels: []string{"<unknown>"}, wantSessions: []string{t144AuthorSession}},
		{name: "OpenCode continue and attached session cluster", kind: "opencode", args: []string{"-cs", t144AuthorSession}, wantSessions: []string{"", t144AuthorSession}},
		{name: "Codex attached config model", kind: "codex", args: []string{`-cmodel="model-a"`}, wantModels: []string{"model-a"}},
		{name: "Codex separated config model", kind: "codex", args: []string{"-c", `model="model-a"`}, wantModels: []string{"model-a"}},
		{name: "Codex effort config is not a model", kind: "codex", args: []string{"-c", "model_reasoning_effort=high"}},
		{name: "unclassified Codex config fails closed", kind: "codex", args: []string{"-c", "model_provider=local"}, wantUnknown: true},
		{name: "unknown model alias fails closed", kind: "claude", args: []string{"--model-id", "model-a"}, wantModels: []string{"<unclassified>"}, wantUnknown: true},
		{name: "Unclassified option fails closed", kind: "claude", args: []string{"--unknown-session-option", "value"}, wantUnknown: true},
		{name: "Unclassified positional fails closed", kind: "pi", args: []string{"custom-session"}, wantUnknown: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			analysis := analyzeLaunchArgs(test.kind, test.args)
			if !reflect.DeepEqual(analysis.models, test.wantModels) || !reflect.DeepEqual(analysis.sessions, test.wantSessions) || (len(analysis.unknown) > 0) != test.wantUnknown {
				t.Fatalf("analyzeLaunchArgs(%q, %#v) = %#v", test.kind, test.args, analysis)
			}
		})
	}
}

func TestLaunchIdentityIncludesRenderedResumeArguments(t *testing.T) {
	for _, test := range []struct {
		kindName string
		kind     config.Kind
		resume   []string
	}{
		{kindName: "claude", kind: config.Kind{ModelArgs: []string{"--model", "{model}"}}, resume: []string{"--resume", "{session}", "--model", "model-b"}},
		{kindName: "codex", kind: config.Kind{ModelArgs: []string{"-c", `model="{model}"`}}, resume: []string{"resume", "{session}", "-c", `model="model-b"`}},
		{kindName: "pi", kind: config.Kind{ModelArgs: []string{"--model", "{model}"}}, resume: []string{"--session", "{session}", "--model", "model-b"}},
		{kindName: "opencode", kind: config.Kind{ModelArgs: []string{"--model", "{model}"}}, resume: []string{"--session", "{session}", "--model", "model-b"}},
	} {
		t.Run(test.kindName, func(t *testing.T) {
			profile := config.Profile{Kind: test.kindName, Model: "model-a"}
			test.kind.ResumeArgs = test.resume
			initial := configuredLaunchIdentity("author", profile, test.kind, workerAgentArgs(profile, test.kind, ""))
			if !initial.ModelKnown || initial.ConfiguredModel != "model-a" {
				t.Fatalf("initial launch identity = %#v, want known model-a", initial)
			}
			relaunch := configuredLaunchIdentity("author", profile, test.kind, workerAgentArgs(profile, test.kind, "author-session"))
			if relaunch.ModelKnown {
				t.Fatalf("relaunch ignored its rendered model override: %#v", relaunch)
			}
		})
	}
}

func TestReviewSessionOverridesFailClosed(t *testing.T) {
	for _, test := range []struct {
		name string
		kind string
		args []string
		want bool
	}{
		{name: "Claude resume alias", kind: "claude", args: []string{"-r", "author-session"}, want: true},
		{name: "Claude continue alias", kind: "claude", args: []string{"-c"}, want: true},
		{name: "explicit session id", kind: "claude", args: []string{"--session-id=author-session"}, want: true},
		{name: "Codex resume command", kind: "codex", args: []string{"resume", "author-session"}, want: true},
		{name: "Codex fork command", kind: "codex", args: []string{"fork", "author-session"}, want: true},
		{name: "Codex session config", kind: "codex", args: []string{"-c", "session_id=author-session"}, want: true},
		{name: "OpenCode session shorthand", kind: "opencode", args: []string{"-s", "author-session"}, want: true},
		{name: "Pi explicit session", kind: "pi", args: []string{"--session", "author-session"}, want: true},
		{name: "Codex model config is not session selection", kind: "codex", args: []string{"-c", `model="model-a"`}},
		{name: "ordinary model argument", kind: "claude", args: []string{"--model", "model-a"}},
		{name: "unknown profile argument", kind: "claude", args: []string{"--unknown-option", "value"}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := reviewSessionOverrideReason(test.kind, test.args) != ""
			if got != test.want {
				t.Fatalf("reviewSessionOverrideReason(%q, %#v) refused=%t, want %t", test.kind, test.args, got, test.want)
			}
		})
	}
}

func TestReviewResumeArgsMustUsePosseSessionPlaceholder(t *testing.T) {
	for _, test := range []struct {
		name string
		kind string
		args []string
		want bool
	}{
		{name: "no configured resume arguments", kind: "claude", want: true},
		{name: "explicit session flag", kind: "claude", args: []string{"--resume", "{session}"}, want: true},
		{name: "Codex resume subcommand", kind: "codex", args: []string{"resume", "{session}"}, want: true},
		{name: "joined session flag", kind: "opencode", args: []string{"--session={session}"}, want: true},
		{name: "continue selects unknown session", kind: "claude", args: []string{"--continue"}},
		{name: "literal session id", kind: "claude", args: []string{"--resume", "author-session"}},
		{name: "second literal selector", kind: "claude", args: []string{"--resume", "{session}", "--session-id", "author-session"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := reviewResumeArgsRefusal(test.kind, test.args, "review-session") == ""
			if got != test.want {
				t.Fatalf("reviewResumeArgsRefusal(%q, %#v) safe=%t, want %t", test.kind, test.args, got, test.want)
			}
		})
	}
}

func TestReviewResumeArgsRefuseUnknownSessionOrModelOverrides(t *testing.T) {
	for _, test := range []struct {
		name    string
		args    []string
		session string
		want    string
	}{
		{name: "fresh launch does not use resume arguments", args: []string{"--resume", "author-session"}},
		{name: "resume own session", args: []string{"--resume", "{session}"}, session: "review-session"},
		{name: "refuse continuation", args: []string{"--continue"}, session: "review-session", want: "do not select only Posse's own"},
		{name: "refuse literal session", args: []string{"--resume", "author-session"}, session: "review-session", want: "do not select only Posse's own"},
		{name: "refuse model override", args: []string{"--resume", "{session}", "--model", "model-a"}, session: "review-session", want: "can override the Review Task model"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := reviewResumeArgsRefusal("claude", test.args, test.session)
			if test.want == "" && got != "" || test.want != "" && !strings.Contains(got, test.want) {
				t.Fatalf("reviewResumeArgsRefusal(%#v, %q) = %q, want containing %q", test.args, test.session, got, test.want)
			}
		})
	}
}

func TestReviewIdentityRequiresDistinctKnownProfileAndModel(t *testing.T) {
	author := []store.TaskLaunchIdentity{
		{TaskID: 10, LaunchNumber: 1, Profile: "author-a", ConfiguredModel: "model-a", ModelKnown: true},
		{TaskID: 10, LaunchNumber: 2, Profile: "author-b", ConfiguredModel: "model-b", ModelKnown: true},
	}
	for _, test := range []struct {
		name     string
		reviewer store.TaskLaunchIdentity
		known    bool
		want     bool
		contains string
	}{
		{name: "independent", reviewer: store.TaskLaunchIdentity{Profile: "review", ConfiguredModel: "model-c", ModelKnown: true}, known: true, want: true},
		{name: "same profile", reviewer: store.TaskLaunchIdentity{Profile: "author-a", ConfiguredModel: "model-c", ModelKnown: true}, known: true, contains: "Profile matches author launch 1"},
		{name: "same model despite different profile", reviewer: store.TaskLaunchIdentity{Profile: "review", ConfiguredModel: "model-b", ModelKnown: true}, known: true, contains: "configured model matches author launch 2"},
		{name: "unknown reviewer model", reviewer: store.TaskLaunchIdentity{Profile: "review", ConfiguredModel: "model-c"}, known: true, contains: "Review Task has no determinable configured model"},
		{name: "unknown author model", reviewer: store.TaskLaunchIdentity{Profile: "review", ConfiguredModel: "model-c", ModelKnown: true}, known: false, contains: "launch-identity history is incomplete"},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := compareReviewIdentity(author, test.known, test.reviewer)
			if check.Eligible != test.want || (test.contains != "" && !strings.Contains(check.Reason, test.contains)) {
				t.Fatalf("comparison = %#v, want eligible=%t and reason containing %q", check, test.want, test.contains)
			}
		})
	}
}

func TestReviewRideRefusesSameConfiguredIdentityWithComparedEvidence(t *testing.T) {
	fixture := newRideFixture(t)
	configureReviewIdentityFixture(t, fixture, "[profiles.reviewer]\nkind = \"pi\"\nmodel = \"model-b\"\n")
	seedReviewAuthor(t, fixture)
	brief := filepath.Join(filepath.Dir(fixture.brief), "review-brief.md")
	if err := os.WriteFile(brief, []byte("---\ntype: review\ntitle: Review launch identity\ndone_when: the author change is independently reviewed\nreview_of: t1\n---\nReview the candidate.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = output, output
	code := cli.Run([]string{"ride", "--brief", brief, "--name", "review-launch-identity", "--profile", "author"})
	if code != 1 || !strings.Contains(output.String(), "review_identity_ineligible") || !strings.Contains(output.String(), "Profile matches author launch 1") || !strings.Contains(output.String(), "Profile=author model=") || !strings.Contains(output.String(), "model-a") {
		t.Fatalf("same-identity review was not refused with comparison evidence: code=%d output=%s", code, output.String())
	}
	if fixture.fake.CallCount("agent.start") != 0 {
		t.Fatalf("ineligible review started an agent: %#v", fixture.fake.Calls)
	}
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tasks, err := db.Tasks(context.Background(), fixture.project.ID, true)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("refused review created a Task: %#v, %v", tasks, err)
	}
}

func TestReviewRelaunchRejectsProfileResumeOverrideBeforeHerdr(t *testing.T) {
	ctx := context.Background()
	fixture := newRelaunchFixture(t, store.StateLost)
	if _, err := fixture.db.NextTaskLaunchWithIdentity(ctx, fixture.task.ID, "deep", "sonnet", true); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(fixture.home, "config.toml")
	configText, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configText = append(configText, []byte("[profiles.reviewer]\nkind = \"claude\"\nmodel = \"opus\"\nargs = [\"--resume\", \"author-session\"]\n")...)
	if err := os.WriteFile(configPath, configText, 0o600); err != nil {
		t.Fatal(err)
	}
	mountPath := filepath.Join(filepath.Dir(fixture.home), "review-mount")
	if _, err := gitOutput(ctx, filepath.Join(filepath.Dir(fixture.home), "repo"), "worktree", "add", "-b", "posse/t2", mountPath, "main"); err != nil {
		t.Fatal(err)
	}
	reviewID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 2, Type: "review", Title: "Independent review", Profile: "reviewer", LandingMode: "local", Branch: "posse/t2", BaseRef: "main", WorktreePath: mountPath, HerdrWorkspaceID: "w1", PaneID: "w1:p2", PaneLabel: "posse:shop:t2", AgentName: "posse-shop-t2-1", AgentSession: `{"session_id":"review-session"}`, ReviewsTaskID: fixture.task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, reviewID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, reviewID, store.StateWorking, store.StateLost, "cli", "test relaunch"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,2,?,'held',?)`, fixture.project.ID, mountPath, reviewID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, reviewID, reviewID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.NextTaskLaunchWithIdentity(ctx, reviewID, "reviewer", "opus", true); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := fixture.db.TaskByID(ctx, fixture.project.ID, reviewID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.fake.SnapshotValue.Panes = append(fixture.fake.SnapshotValue.Panes, herdr.Pane{PaneID: reviewer.PaneID, WorkspaceID: reviewer.HerdrWorkspaceID, Label: reviewer.PaneLabel, Agent: "claude", AgentStatus: "working"})
	_, err = fixture.service.relaunchTask(ctx, fixture.db, fixture.home, fixture.project, cfg, reviewer, "")
	var refusal *axi.Error
	if !errors.As(err, &refusal) || refusal.Code != "review_identity_ineligible" || !strings.Contains(refusal.Message, "session selector") {
		t.Fatalf("Review relaunch with author session override was not refused: %v", err)
	}
	if len(fixture.fake.Calls) != 0 {
		t.Fatalf("ineligible Review relaunch called Herdr: %#v", fixture.fake.Calls)
	}
}

func TestReviewRideRecordsAndShowsDistinctConfiguredIdentities(t *testing.T) {
	fixture := newRideFixture(t)
	configureReviewIdentityFixture(t, fixture, "[profiles.reviewer]\nkind = \"pi\"\nmodel = \"model-b\"\n")
	seedReviewAuthor(t, fixture)
	brief := filepath.Join(filepath.Dir(fixture.brief), "review-brief.md")
	if err := os.WriteFile(brief, []byte("---\ntype: review\ntitle: Review launch identity\ndone_when: the author change is independently reviewed\nreview_of: t1\n---\nReview the candidate.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = output, output
	code := cli.Run([]string{"ride", "--brief", brief, "--name", "review-launch-identity", "--profile", "reviewer"})
	if code != 0 || !strings.Contains(output.String(), "review_identity") || !strings.Contains(output.String(), `model-b`) {
		t.Fatalf("independent review launch failed or omitted comparison: code=%d output=%s", code, output.String())
	}
	freshSession := false
	for _, call := range fixture.fake.Calls {
		if call.Method != "agent.start" || call.Params["kind"] != "pi" {
			continue
		}
		args := call.Params["args"].([]string)
		freshSession = !strings.Contains(strings.Join(args, " "), "author-session")
		for _, arg := range args {
			if arg == "--session" || arg == "--resume" || arg == "resume" {
				freshSession = false
			}
		}
	}
	if !freshSession {
		t.Fatalf("Review Task reused the Ship Task session: %#v", fixture.fake.Calls)
	}
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	task, err := db.Task(context.Background(), fixture.project.ID, "t2")
	if err != nil {
		t.Fatal(err)
	}
	identities, err := db.TaskLaunchIdentities(context.Background(), task.ID)
	if err != nil || task.Launches != 1 || len(identities) != 1 || identities[0].TaskID != task.ID || identities[0].LaunchNumber != 1 || identities[0].Profile != "reviewer" || identities[0].ConfiguredModel != "model-b" || !identities[0].ModelKnown {
		t.Fatalf("Review Task launch identity = %#v task=%#v, %v", identities, task, err)
	}
	show := &bytes.Buffer{}
	showCLI := fixture.service.CLI()
	showCLI.Out, showCLI.ErrOut = show, show
	if code := showCLI.Run([]string{"show", "t2"}); code != 0 || !strings.Contains(show.String(), "eligible: true") || !strings.Contains(show.String(), "model-a") || !strings.Contains(show.String(), "model-b") {
		t.Fatalf("Review Task show omitted its identity comparison: code=%d output=%s", code, show.String())
	}
}

func configureReviewIdentityFixture(t *testing.T, fixture rideFixture, reviewerProfile string) {
	t.Helper()
	configText := "[defaults]\nlanding_mode = \"local\"\n\n[profiles.author]\nkind = \"claude\"\nmodel = \"model-a\"\n\n" + reviewerProfile + "\n[dispatch.default]\nuse = \"author\"\n"
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
}

func seedReviewAuthor(t *testing.T, fixture rideFixture) {
	t.Helper()
	ctx := context.Background()
	gitTest(t, fixture.repo, "branch", "posse/author")
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	authorID, err := db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 1, Type: "ship", Title: "Author change", ShortName: "author", Profile: "author", LandingMode: "local", Branch: "posse/author", BaseRef: "main", AgentSession: `{"session_id":"author-session"}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(ctx, authorID, "author", "model-a", true); err != nil {
		t.Fatal(err)
	}
}
