package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestConfiguredModelIdentityUsesOnlyDeterminableConfiguredValues(t *testing.T) {
	kind := config.Kind{ModelArgs: []string{"--model", "{model}"}}
	for _, test := range []struct {
		name      string
		profile   config.Profile
		wantModel string
		wantKnown bool
	}{
		{name: "profile model", profile: config.Profile{Model: "model-a"}, wantModel: "model-a", wantKnown: true},
		{name: "explicit override", profile: config.Profile{Model: "model-a", Args: []string{"--model", "model-b"}}, wantModel: "model-b", wantKnown: true},
		{name: "joined override", profile: config.Profile{Model: "model-a", Args: []string{"--model=model-b"}}, wantModel: "model-b", wantKnown: true},
		{name: "multiple overrides", profile: config.Profile{Model: "model-a", Args: []string{"--model", "model-b", "--model", "model-c"}}, wantModel: "model-a", wantKnown: false},
		{name: "missing override value", profile: config.Profile{Model: "model-a", Args: []string{"--model"}}, wantModel: "model-a", wantKnown: false},
		{name: "unconfigured model", profile: config.Profile{}, wantModel: "", wantKnown: false},
		{name: "explicitly configured model override", profile: config.Profile{Args: []string{"--model", "model-b"}}, wantModel: "model-b", wantKnown: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			model, known := configuredModelIdentity(test.profile, kind)
			if model != test.wantModel || known != test.wantKnown {
				t.Fatalf("configured model = %q known=%t, want %q known=%t", model, known, test.wantModel, test.wantKnown)
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

func TestReviewRideRecordsAndShowsDistinctConfiguredIdentities(t *testing.T) {
	fixture := newRideFixture(t)
	configureReviewIdentityFixture(t, fixture, "[profiles.reviewer]\nkind = \"pi\"\nmodel = \"model-configured\"\nargs = [\"--model\", \"model-b\"]\n")
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
