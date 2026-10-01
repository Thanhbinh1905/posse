//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// reviewIdentityFixture exercises review admission through the real CLI and an
// isolated Herdr server. Fake harnesses record launch arguments without making
// model calls or changing a remote repository.
func reviewIdentityFixture(t *testing.T) *prLifecycleFixture {
	t.Helper()
	fixture := newPRLifecycleFixture(t)
	t.Cleanup(func() { _ = fixture.db.Close() })
	for _, setting := range [][2]string{
		{"defaults.landing_mode", "local"},
		{"defaults.auto_unsaddle", "never"},
		{"defaults.max_workers", "20"},
		{"profiles.author.kind", "claude"},
		{"profiles.author.model", "model-a"},
		{"profiles.author-b.kind", "claude"},
		{"profiles.author-b.model", "model-b"},
		{"profiles.reviewer.kind", "claude"},
		{"profiles.reviewer.model", "model-c"},
		{"profiles.same-model.kind", "claude"},
		{"profiles.same-model.model", "model-a"},
	} {
		runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", setting[0], setting[1])
	}
	for _, kind := range []string{"claude", "codex", "pi", "opencode"} {
		script := "#!/bin/sh\n" +
			"printf '%s\\t%s\\n' \"$PWD\" \"$*\" >> \"$POSSE_TEST_ROOT/review-identity-args.log\"\n" +
			"herdr pane report-agent \"$HERDR_PANE_ID\" --source posse.fake --agent " + kind + " --state idle >/dev/null 2>&1\n" +
			"while IFS= read -r line; do herdr pane report-agent \"$HERDR_PANE_ID\" --source posse.fake --agent " + kind + " --state working >/dev/null 2>&1; done\n"
		if err := os.WriteFile(filepath.Join(fixture.root, "bin", kind), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

func rideReviewIdentityTask(t *testing.T, fixture *prLifecycleFixture, title, profile, reviewOf string) (int, string) {
	t.Helper()
	taskType := "ship"
	if reviewOf != "" {
		taskType = "review"
	}
	name := strings.ReplaceAll(strings.ToLower(title), " ", "-")
	brief := filepath.Join(fixture.root, name+".md")
	text := fmt.Sprintf("---\ntype: %s\ntitle: %s\ndone_when: checked\n", taskType, title)
	if reviewOf != "" {
		text += "review_of: " + reviewOf + "\n"
	}
	text += "---\nInspect the candidate.\n"
	if err := os.WriteFile(brief, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return runReviewIdentityCLI(t, fixture, "ride", "--brief", brief, "--name", name, "--profile", profile)
}

func runReviewIdentityCLI(t *testing.T, fixture *prLifecycleFixture, args ...string) (int, string) {
	t.Helper()
	command := exec.Command(fixture.binary, args...)
	command.Dir, command.Env = fixture.repo, fixture.leadEnv
	output, err := command.CombinedOutput()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	t.Logf("posse %s: code=%d\n%s", strings.Join(args, " "), code, output)
	return code, string(output)
}

func requireReviewIdentityRefusal(t *testing.T, code int, output string) {
	t.Helper()
	if code != 1 || !strings.Contains(output, "review_identity_ineligible") {
		t.Fatalf("expected review independence refusal, got code=%d\n%s", code, output)
	}
}

func TestReviewRejectsExplicitAuthorSessionResume(t *testing.T) {
	fixture := reviewIdentityFixture(t)
	const session = "99e0a111-1ad9-4010-a1a1-1e2944973bce"
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.author.args", `["--session-id","`+session+`"]`)
	if code, output := rideReviewIdentityTask(t, fixture, "Author candidate", "author", ""); code != 0 {
		t.Fatalf("author spawn: code=%d %s", code, output)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.reviewer.args", `["--resume","`+session+`"]`)
	code, output := rideReviewIdentityTask(t, fixture, "Resumed author review", "reviewer", "t1")
	if code == 0 {
		runReviewIdentityCLI(t, fixture, "show", "t2", "--full")
		args, _ := os.ReadFile(filepath.Join(fixture.root, "review-identity-args.log"))
		t.Logf("agent.start argument evidence:\n%s", args)
	}
	requireReviewIdentityRefusal(t, code, output)
}

func TestReviewRejectsAttachedSessionSelectors(t *testing.T) {
	const authorSession = "99e0a111-1ad9-4010-a1a1-1e2944973bce"
	for _, test := range []struct {
		name string
		kind string
		args []string
	}{
		{name: "Claude attached resume", kind: "claude", args: []string{"-r" + authorSession}},
		{name: "Claude clustered print and resume", kind: "claude", args: []string{"-pr" + authorSession}},
		{name: "OpenCode clustered continue and session", kind: "opencode", args: []string{"-cs", authorSession}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := reviewIdentityFixture(t)
			if test.kind != "claude" {
				runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.reviewer.kind", test.kind)
			}
			if code, output := rideReviewIdentityTask(t, fixture, "Author candidate", "author", ""); code != 0 {
				t.Fatalf("author spawn: code=%d %s", code, output)
			}
			setReviewIdentityProfileArgs(t, fixture, "reviewer", test.args)
			code, output := rideReviewIdentityTask(t, fixture, "Rejected attached", "reviewer", "t1")
			requireReviewIdentityRefusal(t, code, output)
			setReviewIdentityProfileArgs(t, fixture, "reviewer", nil)
			if code, output := rideReviewIdentityTask(t, fixture, "Independent review", "reviewer", "t1"); code != 0 {
				t.Fatalf("initial independent review: code=%d %s", code, output)
			}
			setReviewIdentityProfileArgs(t, fixture, "reviewer", test.args)
			code, output = runReviewIdentityCLI(t, fixture, "relaunch", "t2", "--profile", "reviewer")
			requireReviewIdentityRefusal(t, code, output)
		})
	}
}

func TestReviewResumeTemplateRejectsSecondSessionSelector(t *testing.T) {
	fixture := reviewIdentityFixture(t)
	const authorSession = "99e0a111-1ad9-4010-a1a1-1e2944973bce"
	if code, output := rideReviewIdentityTask(t, fixture, "Author candidate", "author", ""); code != 0 {
		t.Fatalf("author spawn: code=%d %s", code, output)
	}
	if code, output := rideReviewIdentityTask(t, fixture, "Independent review", "reviewer", "t1"); code != 0 {
		t.Fatalf("initial independent review: code=%d %s", code, output)
	}
	review := fixture.mustTask(t, "t2")
	if _, err := fixture.db.ExecContext(context.Background(), "UPDATE tasks SET agent_session=? WHERE id=?", `{"session_id":"review-session"}`, review.ID); err != nil {
		t.Fatal(err)
	}
	resumeArgs, _ := json.Marshal([]string{"--resume", "{session}", "-r" + authorSession})
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "kinds.claude.resume_args", string(resumeArgs))
	code, output := runReviewIdentityCLI(t, fixture, "relaunch", "t2")
	requireReviewIdentityRefusal(t, code, output)
}

func TestReviewRejectsAttachedCodexModelConfig(t *testing.T) {
	fixture := reviewIdentityFixture(t)
	if code, output := rideReviewIdentityTask(t, fixture, "Author candidate", "author", ""); code != 0 {
		t.Fatalf("author spawn: code=%d %s", code, output)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "kinds.codex.model_args", `["-c","model=\"{model}\""]`)
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.reviewer.kind", "codex")
	setReviewIdentityProfileArgs(t, fixture, "reviewer", []string{`-cmodel="model-a"`})
	code, output := rideReviewIdentityTask(t, fixture, "Attached config review", "reviewer", "t1")
	requireReviewIdentityRefusal(t, code, output)
	setReviewIdentityProfileArgs(t, fixture, "reviewer", nil)
	if code, output := rideReviewIdentityTask(t, fixture, "Independent codex", "reviewer", "t1"); code != 0 {
		t.Fatalf("initial independent review: code=%d %s", code, output)
	}
	setReviewIdentityProfileArgs(t, fixture, "reviewer", []string{`-cmodel="model-a"`})
	code, output = runReviewIdentityCLI(t, fixture, "relaunch", "t2", "--profile", "reviewer")
	requireReviewIdentityRefusal(t, code, output)
}

func TestAuthorRelaunchResumeModelOverrideIsRecordedUnknown(t *testing.T) {
	fixture := reviewIdentityFixture(t)
	if code, output := rideReviewIdentityTask(t, fixture, "Author candidate", "author", ""); code != 0 {
		t.Fatalf("author spawn: code=%d %s", code, output)
	}
	author := fixture.mustTask(t, "t1")
	if _, err := fixture.db.ExecContext(context.Background(), "UPDATE tasks SET agent_session=? WHERE id=?", `{"session_id":"author-session"}`, author.ID); err != nil {
		t.Fatal(err)
	}
	resumeArgs, _ := json.Marshal([]string{"--resume", "{session}", "--model", "model-b"})
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "kinds.claude.resume_args", string(resumeArgs))
	if code, output := runReviewIdentityCLI(t, fixture, "relaunch", "t1"); code != 0 {
		t.Fatalf("author relaunch: code=%d %s", code, output)
	}
	identities, err := fixture.db.TaskLaunchIdentities(context.Background(), author.ID)
	if err != nil || len(identities) != 2 || identities[1].ModelKnown {
		t.Fatalf("author relaunch identity = %#v, err=%v; want launch 2 model unknown", identities, err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.reviewer.model", "model-b")
	code, output := rideReviewIdentityTask(t, fixture, "Resumed model review", "reviewer", "t1")
	requireReviewIdentityRefusal(t, code, output)
}

func setReviewIdentityProfileArgs(t *testing.T, fixture *prLifecycleFixture, profile string, args []string) {
	t.Helper()
	if args == nil {
		args = []string{}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles."+profile+".args", string(encoded))
}

func TestReviewRejectsQuotedSameModelOverride(t *testing.T) {
	fixture := reviewIdentityFixture(t)
	if code, output := rideReviewIdentityTask(t, fixture, "Author candidate", "author", ""); code != 0 {
		t.Fatalf("author spawn: code=%d %s", code, output)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "kinds.codex.model_args", `["-c","model=\"{model}\""]`)
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.reviewer.kind", "codex")
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.reviewer.args", `["-c","model=\"model-a\""]`)
	code, output := rideReviewIdentityTask(t, fixture, "Same model review", "reviewer", "t1")
	if code == 0 {
		runReviewIdentityCLI(t, fixture, "show", "t2", "--full")
		reviewer := fixture.mustTask(t, "t2")
		identities, err := fixture.db.TaskLaunchIdentities(context.Background(), reviewer.ID)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("persisted configured identity: %#v", identities)
		args, _ := os.ReadFile(filepath.Join(fixture.root, "review-identity-args.log"))
		t.Logf("agent.start argument evidence:\n%s", args)
	}
	requireReviewIdentityRefusal(t, code, output)
}

func TestReviewRejectsMalformedModelAliasOverride(t *testing.T) {
	fixture := reviewIdentityFixture(t)
	if code, output := rideReviewIdentityTask(t, fixture, "Author candidate", "author", ""); code != 0 {
		t.Fatalf("author spawn: code=%d %s", code, output)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.reviewer.kind", "codex")
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.reviewer.args", `["--model"]`)
	code, output := rideReviewIdentityTask(t, fixture, "Ambiguous alias review", "reviewer", "t1")
	if code == 0 {
		runReviewIdentityCLI(t, fixture, "show", "t2", "--full")
		args, _ := os.ReadFile(filepath.Join(fixture.root, "review-identity-args.log"))
		t.Logf("agent.start argument evidence:\n%s", args)
	}
	requireReviewIdentityRefusal(t, code, output)
}
