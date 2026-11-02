package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func writeTestHandoff(t *testing.T, home string, project store.Project, body string) string {
	t.Helper()
	path := handoffPath(home, project.Name)
	contents := fmt.Sprintf("<!-- posse-handoff\nwritten_at: %s\nharness: %q\nsession: %q\ngeneration: 1\n-->\n\n%s", time.Now().UTC().Format(time.RFC3339Nano), "claude", "test-session", body)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPromptOnlyLeadReceivesHandoffThroughPosseLead(t *testing.T) {
	for _, kind := range []string{"opencode", "custom"} {
		t.Run(kind, func(t *testing.T) {
			f := newPreferenceSetFixture(t)
			t.Chdir(f.repo)
			cfg, err := config.Load(f.home, "shop")
			if err != nil {
				t.Fatal(err)
			}
			if kind == "custom" {
				cfg.Kinds[kind] = config.Kind{}
			}
			const note = "Prompt-only handoff must survive until posse lead returns it.\n"
			pending := writeTestHandoff(t, f.home, f.project, note)
			launch, err := f.service.prepareLeadLaunch(f.home, f.project, cfg, kind)
			if err != nil {
				t.Fatal(err)
			}
			if launch.HandoffLoaded {
				t.Fatal("prompt-only launch marked the handoff consumed before posse lead returned it")
			}
			if strings.Contains(strings.Join(launch.Args, " "), "developer_instructions") {
				t.Fatalf("%s unexpectedly received direct system-prompt instructions: %#v", kind, launch.Args)
			}
			userOutput := runPreferenceSetCLI(t, f.service, 0, "lead")
			if !strings.Contains(userOutput, strings.TrimSpace(note)) {
				t.Fatalf("posse lead did not show the pending handoff to the User: %s", userOutput)
			}
			if _, err := os.Stat(pending); err != nil {
				t.Fatalf("User invocation consumed the Lead handoff: %v", err)
			}
			t.Setenv("HERDR_PANE_ID", "w1:p1")
			t.Setenv("HERDR_WORKSPACE_ID", "w1")
			output := runPreferenceSetCLI(t, f.service, 0, "lead")
			if !strings.Contains(output, strings.TrimSpace(note)) {
				t.Fatalf("posse lead did not return the handoff: %s", output)
			}
			if _, err := os.Stat(pending); !os.IsNotExist(err) {
				t.Fatalf("posse lead returned but did not consume the handoff: %v", err)
			}
			archive := filepath.Join(handoffArchiveDir(f.home, f.project.Name), "handoff-0001.md")
			if archived, err := os.ReadFile(archive); err != nil || !strings.Contains(string(archived), note) {
				t.Fatalf("posse lead did not archive the returned handoff: %q, %v", archived, err)
			}
		})
	}
}

func TestHandoffWriteLoadsOnceAndArchivesOnLeadStartup(t *testing.T) {
	f := newPreferenceSetFixture(t)
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLeadAgent(context.Background(), f.project.ID, store.LeadAgent{Kind: "claude", Session: "claude-session-1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(f.repo)
	source := filepath.Join(f.root, "handoff.md")
	const note = "Open question: should the delivery remain local?\nNext: check the frozen profile decision.\n"
	if err := os.WriteFile(source, []byte(note), 0o600); err != nil {
		t.Fatal(err)
	}
	output := runPreferenceSetCLI(t, f.service, 0, "handoff", "write", "--file", source)
	if !strings.Contains(output, "state: written") {
		t.Fatalf("handoff write did not report success: %s", output)
	}

	pending := filepath.Join(f.home, "projects", "shop", "handoff.md")
	contents, err := os.ReadFile(pending)
	if err != nil || !strings.Contains(string(contents), "generation: 1") || !strings.Contains(string(contents), `harness: "claude"`) || !strings.Contains(string(contents), `session: "claude-session-1"`) || !strings.Contains(string(contents), note) {
		t.Fatalf("handoff note lacks provenance or exact body: %q, %v", contents, err)
	}

	cfg, err := config.Load(f.home, "shop")
	if err != nil {
		t.Fatal(err)
	}
	launch, err := f.service.prepareLeadLaunch(f.home, f.project, cfg, "pi")
	if err != nil {
		t.Fatal(err)
	}
	if !launch.HandoffLoaded {
		t.Fatal("Lead launch did not retain handoff archive ownership")
	}
	first, err := os.ReadFile(filepath.Join(f.home, "projects", "shop", "lead.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(first), note) != 1 {
		t.Fatalf("Lead instructions must include the handoff exactly once: %s", first)
	}
	if !strings.Contains(string(first), "Before `posse up --replace`") || !strings.Contains(string(first), "Write a handoff only after the User asks") {
		t.Fatalf("Lead guidance omits the opt-in handoff trigger: %s", first)
	}
	if _, err := os.Stat(pending); err != nil {
		t.Fatalf("handoff must remain pending until the agent starts: %v", err)
	}
	if err := archiveHandoff(f.home, f.project); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatalf("started Lead did not remove the pending note: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(f.home, "projects", "shop", "handoff-archive"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("handoff archive entries = %v, %v", entries, err)
	}
	archived, err := os.ReadFile(filepath.Join(f.home, "projects", "shop", "handoff-archive", entries[0].Name()))
	if err != nil || !bytes.Contains(archived, []byte(note)) {
		t.Fatalf("archived note lost its contents: %q, %v", archived, err)
	}

	if _, err := f.service.prepareLeadLaunch(f.home, f.project, cfg, "claude"); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(f.home, "projects", "shop", "lead.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(second), note) {
		t.Fatalf("consumed handoff reloaded on the next Lead start: %s", second)
	}
}

func TestWritePendingHandoffDoesNotOverwriteAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handoff.md")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writePendingHandoff(path, []byte("second")); !os.IsExist(err) {
		t.Fatalf("write to existing handoff error = %v, want file exists", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "first" {
		t.Fatalf("existing handoff was replaced: %q, %v", contents, err)
	}
}

func TestHandoffWriteDoesNotOverwriteAnUnconsumedNote(t *testing.T) {
	f := newPreferenceSetFixture(t)
	t.Chdir(f.repo)
	first := filepath.Join(f.root, "first.md")
	second := filepath.Join(f.root, "second.md")
	if err := os.WriteFile(first, []byte("First handoff.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("Second handoff.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPreferenceSetCLI(t, f.service, 0, "handoff", "write", "--file", first)
	output := runPreferenceSetCLI(t, f.service, 1, "handoff", "write", "--file", second)
	if !strings.Contains(output, "handoff_pending") {
		t.Fatalf("second write did not identify the pending note: %s", output)
	}
	contents, err := os.ReadFile(handoffPath(f.home, "shop"))
	if err != nil || !strings.Contains(string(contents), "First handoff.") || strings.Contains(string(contents), "Second handoff.") {
		t.Fatalf("second write replaced pending note: %q, %v", contents, err)
	}
}

func TestHandoffWriteRejectsEmptyAndOversizedNotes(t *testing.T) {
	f := newPreferenceSetFixture(t)
	t.Chdir(f.repo)
	for _, test := range []struct {
		name    string
		content string
	}{
		{name: "empty", content: "  \n"},
		{name: "oversized", content: strings.Repeat("x", handoffMaxBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := filepath.Join(f.root, test.name+".md")
			if err := os.WriteFile(source, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			cli := f.service.CLI()
			cli.Out, cli.ErrOut = &output, &output
			if code := cli.Run([]string{"handoff", "write", "--file", source}); code == 0 {
				t.Fatalf("invalid handoff accepted: %s", output.String())
			}
		})
	}
	if _, err := os.Stat(filepath.Join(f.home, "projects", "shop", "handoff.md")); !os.IsNotExist(err) {
		t.Fatalf("invalid handoff created a pending note: %v", err)
	}
}

func TestHandoffThatExceedsRemainingInstructionBudgetDoesNotBlockStartup(t *testing.T) {
	f := newPreferenceSetFixture(t)
	cfg, err := config.Load(f.home, "shop")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := composeLeadInstructions(f.home, f.project, cfg, "claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Identity.Lead.Persona = strings.Repeat("p", leadInstructionMaxBytes-len(baseline.Text)-1000)
	source := filepath.Join(f.root, "handoff.md")
	if err := os.WriteFile(source, []byte(strings.Repeat("n", 2*1024)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(f.repo)
	runPreferenceSetCLI(t, f.service, 0, "handoff", "write", "--file", source)
	if _, err := f.service.prepareLeadLaunch(f.home, f.project, cfg, "claude"); err != nil {
		t.Fatalf("handoff exceeded the budget and blocked Lead startup: %v", err)
	}
	instructions, err := os.ReadFile(filepath.Join(f.home, "projects", "shop", "lead.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(instructions), "would exceed the Lead instruction budget") {
		t.Fatalf("startup instructions lack a budget warning: %s", instructions)
	}
	if _, err := os.Stat(handoffPath(f.home, "shop")); err != nil {
		t.Fatalf("note that did not fit was consumed: %v", err)
	}
}

func TestInvalidHandoffWarnsWithoutBlockingLeadStartup(t *testing.T) {
	for _, test := range []struct {
		name    string
		content []byte
		warning string
	}{
		{name: "malformed", content: []byte("not a valid handoff"), warning: "note header is malformed or the body is empty"},
		{name: "oversized", content: []byte(strings.Repeat("x", handoffMaxBytes+1)), warning: "exceeds the size limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPreferenceSetFixture(t)
			pending := handoffPath(f.home, "shop")
			if err := os.WriteFile(pending, test.content, 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(f.home, "shop")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.prepareLeadLaunch(f.home, f.project, cfg, "claude"); err != nil {
				t.Fatalf("invalid handoff blocked startup: %v", err)
			}
			instructions, err := os.ReadFile(filepath.Join(f.home, "projects", "shop", "lead.md"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(instructions), test.warning) {
				t.Fatalf("startup instructions lack an actionable warning: %s", instructions)
			}
			if _, err := os.Stat(pending); err != nil {
				t.Fatalf("invalid note should remain available for repair: %v", err)
			}
		})
	}
}

func TestHandoffWriteUsesRecordedLeadIdentity(t *testing.T) {
	f := newPreferenceSetFixture(t)
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLeadAgent(context.Background(), f.project.ID, store.LeadAgent{Kind: "claude", Session: "session-42"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(f.repo)
	source := filepath.Join(f.root, "identity.md")
	if err := os.WriteFile(source, []byte("Next step.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPreferenceSetCLI(t, f.service, 0, "handoff", "write", "--file", source)
	contents, err := os.ReadFile(filepath.Join(f.home, "projects", "shop", "handoff.md"))
	if err != nil || !strings.Contains(string(contents), `harness: "claude"`) || !strings.Contains(string(contents), `session: "session-42"`) {
		t.Fatalf("handoff provenance omitted recorded Lead identity: %q, %v", contents, err)
	}
}
