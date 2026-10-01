//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestReviewRejectsCaseVariantCodexModelKeysAtSpawnAndRelaunch(t *testing.T) {
	for _, test := range []struct {
		name string
		args string
	}{
		{name: "separated mixed-case key", args: `["-c","Model=\"{model}\""]`},
		{name: "attached uppercase key", args: `["-cMODEL=\"{model}\""]`},
		{name: "long equals mixed-case key", args: `["--config=mOdEl=\"{model}\""]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := reviewIdentityFixture(t)
			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.author.kind", "codex")
			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "kinds.codex.model_args", `["-m","{model}"]`)
			if code, output := rideReviewIdentityTask(t, fixture, "Author candidate", "author", ""); code != 0 {
				t.Fatalf("author spawn: code=%d %s", code, output)
			}
			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.reviewer.kind", "codex")
			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.reviewer.model", "model-c")
			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "kinds.codex.model_args", test.args)

			code, output := rideReviewIdentityTask(t, fixture, "Case-variant spawn", "reviewer", "t1")
			requireReviewIdentityRefusal(t, code, output)
			tasks, err := fixture.db.Tasks(context.Background(), fixture.project.ID, true)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("refused spawn allocated a Review Task: tasks=%d err=%v", len(tasks), err)
			}

			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "kinds.codex.model_args", `["-c","model=\"{model}\""]`)
			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "kinds.codex.effort_args", `["-c","model_reasoning_effort={effort}"]`)
			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.reviewer.effort", "high")
			if code, output := rideReviewIdentityTask(t, fixture, "Independent reviewer", "reviewer", "t1"); code != 0 {
				t.Fatalf("valid Codex Review spawn: code=%d %s", code, output)
			}
			reviewer := fixture.mustTask(t, "t2")
			before, err := fixture.db.TaskLaunchIdentities(context.Background(), reviewer.ID)
			if err != nil || len(before) != 1 || !before[0].ModelKnown || before[0].ConfiguredModel != "model-c" {
				t.Fatalf("initial Review launch identities = %#v, err=%v", before, err)
			}

			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "kinds.codex.model_args", test.args)
			code, output = runReviewIdentityCLI(t, fixture, "relaunch", "t2", "--profile", "reviewer")
			requireReviewIdentityRefusal(t, code, output)
			after, err := fixture.db.TaskLaunchIdentities(context.Background(), reviewer.ID)
			if err != nil || len(after) != len(before) {
				t.Fatalf("refused relaunch recorded another launch: before=%#v after=%#v err=%v", before, after, err)
			}
		})
	}
}

func TestReviewRejectsCodexTOMLModelLiteralAndCommentsAtSpawnAndRelaunch(t *testing.T) {
	for _, test := range []struct {
		name      string
		profile   string
		modelArgs []string
	}{
		{name: "triple literal string", profile: "''model-a''", modelArgs: []string{"-c", "model='{model}'"}},
		{name: "literal string with inline comment", profile: "'model-a'#comment", modelArgs: []string{"-c", "model={model}"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := reviewIdentityFixture(t)
			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.author.kind", "codex")
			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.reviewer.kind", "codex")
			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "kinds.codex.model_args", `["-m","{model}"]`)
			if code, output := rideReviewIdentityTask(t, fixture, "Author candidate", "author", ""); code != 0 {
				t.Fatalf("author spawn: code=%d %s", code, output)
			}

			profileModel, err := json.Marshal(test.profile)
			if err != nil {
				t.Fatal(err)
			}
			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.reviewer.model", string(profileModel))
			modelArgs, err := json.Marshal(test.modelArgs)
			if err != nil {
				t.Fatal(err)
			}
			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "kinds.codex.model_args", string(modelArgs))
			code, output := rideReviewIdentityTask(t, fixture, "Rejected TOML Review", "reviewer", "t1")
			requireReviewIdentityRefusal(t, code, output)
			tasks, err := fixture.db.Tasks(context.Background(), fixture.project.ID, true)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("refused spawn allocated a Review Task: tasks=%d err=%v", len(tasks), err)
			}

			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.reviewer.model", `"model-c"`)
			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "kinds.codex.model_args", `["-m","{model}"]`)
			if code, output := rideReviewIdentityTask(t, fixture, "Independent reviewer", "reviewer", "t1"); code != 0 {
				t.Fatalf("independent review spawn: code=%d %s", code, output)
			}
			reviewer := fixture.mustTask(t, "t2")
			before, err := fixture.db.TaskLaunchIdentities(context.Background(), reviewer.ID)
			if err != nil || len(before) != 1 {
				t.Fatalf("Review launch identities = %#v, err=%v", before, err)
			}

			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.reviewer.model", string(profileModel))
			runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "kinds.codex.model_args", string(modelArgs))
			code, output = runReviewIdentityCLI(t, fixture, "relaunch", "t2", "--profile", "reviewer")
			requireReviewIdentityRefusal(t, code, output)
			after, err := fixture.db.TaskLaunchIdentities(context.Background(), reviewer.ID)
			if err != nil || len(after) != len(before) {
				t.Fatalf("refused relaunch recorded another launch: before=%#v after=%#v err=%v", before, after, err)
			}
		})
	}
}

func TestCodexCaseVariantModelKeysKeepLoopbackModelAtDefault(t *testing.T) {
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("Codex CLI is not installed")
	}
	root := newFixtureRoot(t, fixturePrefix("codex-case-loopback-"))
	home := filepath.Join(root, "home")
	codexHome := filepath.Join(root, "codex")
	for _, dir := range []string{home, codexHome, filepath.Join(root, "xdg"), filepath.Join(root, "posse")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var models []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode loopback request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		models = append(models, request.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"loopback probe stops before completion"}}`))
	}))
	defer server.Close()

	codexConfig := fmt.Sprintf(`model = "model-a"
model_provider = "posse-loopback"
[model_providers.posse-loopback]
name = "Posse loopback probe"
base_url = %q
wire_api = "responses"
requires_openai_auth = false
`, server.URL+"/v1")
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(codexConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"HOME=" + home,
		"CODEX_HOME=" + codexHome,
		"XDG_CONFIG_HOME=" + filepath.Join(root, "xdg"),
		"POSSE_HOME=" + filepath.Join(root, "posse"),
		"PATH=" + os.Getenv("PATH"),
		"LANG=C.UTF-8",
		"NO_COLOR=1",
	}
	for _, test := range []struct {
		name      string
		args      []string
		wantModel string
	}{
		{name: "configured default", wantModel: "model-a"},
		{name: "valid lowercase model key", args: []string{"-c", `model="model-c"`}, wantModel: "model-c"},
		{name: "separated mixed-case model key is ignored", args: []string{"-c", `Model="model-c"`}, wantModel: "model-a"},
		{name: "attached uppercase model key is ignored", args: []string{`-cMODEL="model-c"`}, wantModel: "model-a"},
		{name: "long equals mixed-case model key is ignored", args: []string{`--config=mOdEl="model-c"`}, wantModel: "model-a"},
	} {
		t.Run(test.name, func(t *testing.T) {
			mu.Lock()
			models = nil
			mu.Unlock()
			args := append([]string{}, test.args...)
			args = append(args, "exec", "--skip-git-repo-check", "--sandbox", "read-only", "--json", "-")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, codex, args...)
			command.Dir, command.Env = home, env
			command.Stdin = strings.NewReader("Reply with ok.\n")
			output, runErr := command.CombinedOutput()
			t.Logf("codex exit=%v output=%s", runErr, output)
			mu.Lock()
			got := append([]string(nil), models...)
			mu.Unlock()
			if len(got) == 0 {
				t.Fatalf("Codex made no loopback request; err=%v output=%s", runErr, output)
			}
			for _, model := range got {
				if model != test.wantModel {
					t.Errorf("Codex loopback selected model %q, want %q; requests=%#v", model, test.wantModel, got)
				}
			}
		})
		if t.Failed() {
			return
		}
	}
}
