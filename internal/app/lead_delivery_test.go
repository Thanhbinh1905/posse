package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type codexLeadFixture struct {
	home    string
	db      *store.DB
	project store.Project
	fake    *herdr.Fake
	service *Service
	log     string
}

// newCodexLeadFixture sets up a Project whose Lead pane runs codex with the
// given Herdr state, one undelivered Notice, and a fake `codex` on PATH that
// records its arguments and exits with exitCode.
func newCodexLeadFixture(t *testing.T, lead herdr.Pane, focusedPaneID string, exitCode string) codexLeadFixture {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "codex.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\nexit " + exitCode + "\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", lead.PaneID, "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if project, err = db.ProjectByID(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "done"}); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{FocusedPaneID: focusedPaneID, Panes: []herdr.Pane{lead}}
	return codexLeadFixture{home: home, db: db, project: project, fake: fake, service: testService(home, fake), log: log}
}

func (f codexLeadFixture) codexCalls(t *testing.T) string {
	t.Helper()
	contents, err := os.ReadFile(f.log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(contents)
}

func (f codexLeadFixture) undelivered(t *testing.T) int {
	t.Helper()
	notices, err := f.db.UndeliveredNotices(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	return len(notices)
}

var codexSession = json.RawMessage(`{"source":"herdr:codex","agent":"codex","kind":"id","value":"thread-1"}`)

func TestCodexLeadReceivesNoticesThroughItsQueueWhileFocusedAndBusy(t *testing.T) {
	lead := herdr.Pane{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "codex", AgentStatus: "working", Focused: true, AgentSession: codexSession}
	fixture := newCodexLeadFixture(t, lead, "w1:p1", "0")
	if err := fixture.service.deliverNotices(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	if got, want := fixture.codexCalls(t), "queue --thread thread-1 --message [posse | Posse -> Lead project | notice #1]\nbody: \"project done - done. Notice ids: 1. Lowkey mode off: report every Notice and ack it.\"\n"; got != want {
		t.Fatalf("codex calls = %q, want %q", got, want)
	}
	if fixture.fake.CallCount("agent.prompt") != 0 {
		t.Fatalf("focused codex Lead received typed input: %#v", fixture.fake.Calls)
	}
	if fixture.undelivered(t) != 0 {
		t.Fatal("queued Notice stayed undelivered")
	}
}

func TestLowkeyCodexQueueCarriesNoticeAndCurrentRule(t *testing.T) {
	lead := herdr.Pane{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "codex", AgentStatus: "working", Focused: true, AgentSession: codexSession}
	fixture := newCodexLeadFixture(t, lead, "w1:p1", "0")
	if _, err := config.SetFile(config.ConfigPath(fixture.home, "shop"), "lowkey.lead", "true", true); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.deliverNotices(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	queued := fixture.codexCalls(t)
	for _, want := range []string{"[posse | Posse -> Lead project | notice #1]", "Lowkey mode on", "Notice ids: 1", "message the User only"} {
		if !strings.Contains(queued, want) {
			t.Fatalf("codex wake missing %q: %s", want, queued)
		}
	}
}

func TestFailedCodexQueueFallsBackToTypedPromptOnlyWhenUnfocused(t *testing.T) {
	focused := herdr.Pane{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "codex", AgentStatus: "idle", Focused: true, AgentSession: codexSession}
	fixture := newCodexLeadFixture(t, focused, "w1:p1", "1")
	if err := fixture.service.deliverNotices(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	if fixture.fake.CallCount("agent.prompt") != 0 || fixture.undelivered(t) != 1 {
		t.Fatalf("failed queue into a focused Lead: prompts=%d undelivered=%d", fixture.fake.CallCount("agent.prompt"), fixture.undelivered(t))
	}

	unfocused := focused
	unfocused.Focused = false
	fixture = newCodexLeadFixture(t, unfocused, "", "1")
	if err := fixture.service.deliverNotices(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	if fixture.fake.CallCount("agent.prompt") != 1 || fixture.undelivered(t) != 0 {
		t.Fatalf("failed queue into an unfocused Lead: prompts=%d undelivered=%d", fixture.fake.CallCount("agent.prompt"), fixture.undelivered(t))
	}
}

func TestLowkeyTypedWakeCarriesNoticeText(t *testing.T) {
	lead := herdr.Pane{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "codex", AgentStatus: "idle"}
	fixture := newCodexLeadFixture(t, lead, "", "0")
	if _, err := config.SetFile(config.ConfigPath(fixture.home, "shop"), "lowkey.lead", "true", true); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.deliverNotices(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, call := range fixture.fake.Calls {
		if call.Method == "agent.prompt" {
			text, _ := call.Params["text"].(string)
			found = strings.Contains(text, "project done - done") && strings.Contains(text, "Lowkey mode on")
		}
	}
	if !found {
		t.Fatalf("typed wake missing lowkey Notice: %#v", fixture.fake.Calls)
	}
}

func TestClaudeTypedNoticeHasMatchingDeliveryRecord(t *testing.T) {
	lead := herdr.Pane{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"}
	fixture := newCodexLeadFixture(t, lead, "", "0")
	notices, err := fixture.db.UndeliveredNotices(context.Background(), fixture.project.ID)
	if err != nil || len(notices) != 1 {
		t.Fatalf("notice fixture: %v %#v", err, notices)
	}
	if err := fixture.service.deliverNotices(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(claudeNoticeDirectory(fixture.home, "shop"), fmt.Sprint(notices[0].ID)+".txt")
	recorded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range fixture.fake.Calls {
		if call.Method == "agent.prompt" {
			if text, _ := call.Params["text"].(string); string(recorded) == text && strings.HasPrefix(text, "[posse | Posse -> Lead") {
				return
			}
		}
	}
	t.Fatalf("Claude Notice record does not match delivered prompt: %q", recorded)
}

func TestPiLowkeyRoutineDoesNotShowHerdrNotice(t *testing.T) {
	lead := herdr.Pane{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "pi", AgentStatus: "idle"}
	fixture := newCodexLeadFixture(t, lead, "", "0")
	if _, err := config.SetFile(config.ConfigPath(fixture.home, "shop"), "lowkey.lead", "true", true); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.AckNotices(context.Background(), fixture.project.ID, []string{"1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.CreateNotice(context.Background(), store.Notice{ProjectID: fixture.project.ID, Kind: "pr_opened", Summary: "routine"}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.deliverNotices(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	if fixture.fake.CallCount("notification.show") != 0 || fixture.fake.CallCount("agent.prompt") != 0 || fixture.undelivered(t) != 1 {
		t.Fatalf("quiet Pi Notice went through Herdr: calls=%#v undelivered=%d", fixture.fake.Calls, fixture.undelivered(t))
	}
}

func TestPiExtensionOwnsDeliveryEvenWhenUnfocused(t *testing.T) {
	lead := herdr.Pane{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "pi", AgentStatus: "idle"}
	fixture := newCodexLeadFixture(t, lead, "", "0")
	if err := fixture.service.deliverNotices(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	if fixture.fake.CallCount("agent.prompt") != 0 || fixture.undelivered(t) != 1 {
		t.Fatalf("Pi extension's batch was stolen by typed fallback: calls=%#v undelivered=%d", fixture.fake.Calls, fixture.undelivered(t))
	}
}

func TestOpenCodePluginOwnsNoticeDeliveryWithoutTyping(t *testing.T) {
	lead := herdr.Pane{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "opencode", AgentStatus: "idle", Focused: true}
	fixture := newCodexLeadFixture(t, lead, "w1:p1", "0")
	if err := fixture.service.deliverNotices(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	if fixture.fake.CallCount("agent.prompt") != 0 || fixture.undelivered(t) != 1 {
		t.Fatalf("OpenCode plugin batch was stolen: calls=%#v undelivered=%d", fixture.fake.Calls, fixture.undelivered(t))
	}
}

func TestCodexLeadWithoutSessionUsesTypedPrompt(t *testing.T) {
	lead := herdr.Pane{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "codex", AgentStatus: "idle"}
	fixture := newCodexLeadFixture(t, lead, "", "0")
	if err := fixture.service.deliverNotices(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	if fixture.codexCalls(t) != "" || fixture.fake.CallCount("agent.prompt") != 1 {
		t.Fatalf("session-less codex Lead: codex=%q prompts=%d", fixture.codexCalls(t), fixture.fake.CallCount("agent.prompt"))
	}
}

func TestAgentSessionIDReadsHerdrSessionValue(t *testing.T) {
	for input, want := range map[string]string{
		string(codexSession):         "thread-1",
		`{"session_id":"legacy"}`:    "legacy",
		`"plain"`:                    "plain",
		"null":                       "",
		`{"kind":"path","value":""}`: "",
		`{"session_id":"{\"session_id\":\"bad\"}"}`: "",
	} {
		if got := agentSessionID("codex", input); got != want {
			t.Errorf("agentSessionID(%s) = %q, want %q", input, got, want)
		}
	}
}

func TestLeadComposerRetainsWorkspaceAndAnsweredDecisionGuidance(t *testing.T) {
	home := t.TempDir()
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	composed, err := composeLeadInstructions(home, store.Project{Name: "workspace", Kind: store.ProjectKindWorkspace}, cfg, "codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{
		"Read shared workspace-root files such as CLAUDE.md and docs/",
		"posse project scan` after the User adds or removes a repository",
		"posse apply <decision>` to relaunch or discard",
		"posse ride --from-leftover <decision>",
		"Never perform these actions before the User answers",
	} {
		if !strings.Contains(composed.Text, rule) {
			t.Errorf("composed Lead instructions omitted %q", rule)
		}
	}
}

func TestBuiltInLeadInstructionsStayWithinBudget(t *testing.T) {
	home := t.TempDir()
	service := testService(home, nil)
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}

	for _, project := range []store.Project{
		{Name: "shop"},
		{Name: "workspace", Kind: store.ProjectKindWorkspace},
	} {
		for _, kind := range []string{"claude", "codex", "pi", "opencode"} {
			t.Run(project.Name+"/"+kind, func(t *testing.T) {
				launch, err := service.prepareLeadLaunch(home, project, cfg, kind)
				if err != nil {
					t.Fatal(err)
				}

				var injected string
				if kind == "codex" {
					for _, arg := range launch.Args {
						if !strings.HasPrefix(arg, "developer_instructions=") {
							continue
						}
						var values map[string]string
						if _, err := toml.Decode(arg, &values); err != nil {
							t.Fatal(err)
						}
						injected = values["developer_instructions"]
						break
					}
				} else {
					contents, err := os.ReadFile(filepath.Join(home, "projects", project.Name, "lead.md"))
					if err != nil {
						t.Fatal(err)
					}
					injected = string(contents)
				}
				if injected == "" {
					t.Fatal("Lead instructions were not injected")
				}
				if size := len(injected); size > leadInstructionMaxBytes {
					t.Fatalf("%s instructions = %d bytes, over the %d-byte budget", kind, size, leadInstructionMaxBytes)
				}
				if gotWorkspace := strings.Contains(injected, workspaceRule); gotWorkspace != project.IsWorkspace() {
					t.Fatalf("workspace suffix present = %t, want %t", gotWorkspace, project.IsWorkspace())
				}
			})
		}
	}
}

func TestPrepareLeadLaunchRejectsOversizedIdentity(t *testing.T) {
	home := t.TempDir()
	service := testService(home, nil)
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Identity.Lead.Persona = strings.Repeat("x", leadInstructionMaxBytes)
	project := store.Project{Name: "shop"}
	composed, err := composeLeadInstructions(home, project, cfg, "codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	actualSize := len(composed.Text)

	launch, err := service.prepareLeadLaunch(home, project, cfg, "codex")
	if err == nil {
		t.Fatalf("prepareLeadLaunch returned launch with %d-byte instructions: %#v", actualSize, launch)
	}
	failure, ok := err.(*axi.Error)
	if !ok {
		t.Fatalf("prepareLeadLaunch error = %T %v, want *axi.Error", err, err)
	}
	if failure.Code != "lead_instructions_too_large" {
		t.Fatalf("error code = %q, want lead_instructions_too_large", failure.Code)
	}
	wantMessage := fmt.Sprintf("Lead instructions exceed the 7500-token budget: %d bytes (maximum 30000 bytes at 4 bytes per token)", actualSize)
	if failure.Message != wantMessage {
		t.Fatalf("error message = %q, want %q", failure.Message, wantMessage)
	}
	if _, err := os.Stat(filepath.Join(home, "projects", project.Name, "lead.md")); !os.IsNotExist(err) {
		t.Fatalf("oversized instructions file stat error = %v, want not-exist", err)
	}
	if _, err := service.prepareLeadLaunch(home, project, cfg, "opencode"); err == nil {
		t.Fatal("oversized OpenCode instructions were accepted")
	}
	if _, err := os.Stat(filepath.Join(home, "projects", project.Name, "lead-opencode-plugin.js")); !os.IsNotExist(err) {
		t.Fatalf("oversized OpenCode instructions generated a plugin: %v", err)
	}
}

func TestLeadLaunchByKind(t *testing.T) {
	home := t.TempDir()
	service := testService(home, nil)
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	launch := func(kind string) leadLaunch {
		launch, err := service.prepareLeadLaunch(home, store.Project{Name: "shop"}, cfg, kind)
		if err != nil {
			t.Fatal(err)
		}
		return launch
	}
	instructions := filepath.Join(home, "projects", "shop", "lead.md")

	claude := launch("claude")
	plugin := filepath.Join(home, "projects", "shop", "lead-claude-lowkey")
	if !equalStrings(claude.Args, []string{"--dangerously-skip-permissions", "--append-system-prompt-file", instructions, "--plugin-dir", plugin}) || claude.TypedPrompt != "" ||
		claude.Env["CLAUDE_CODE_ENABLE_FUNCTION_HOOKS"] != "1" || claude.Env["POSSE_LOWKEY_CONFIG"] != config.ConfigPath(home, "shop") ||
		claude.Env["POSSE_LOWKEY_GLOBAL_CONFIG"] != config.ConfigPath(home, "") || claude.Env["POSSE_LOWKEY_NOTICES_DIR"] != claudeNoticeDirectory(home, "shop") {
		t.Fatalf("claude launch = %#v", claude)
	}
	for _, file := range []string{".claude-plugin/plugin.json", "hooks/hooks.json", "hooks/register.ts", "lib/presentation.ts"} {
		if _, err := os.ReadFile(filepath.Join(plugin, file)); err != nil {
			t.Fatalf("Claude Lead plugin %s: %v", file, err)
		}
	}

	codex := launch("codex")
	if len(codex.Args) != 6 || !equalStrings(codex.Args[:4], []string{"--dangerously-bypass-approvals-and-sandbox", "--sandbox", "danger-full-access", "-c"}) || !strings.HasPrefix(codex.Args[4], "developer_instructions=\"# Posse Lead instructions\\n\\nYou are the Lead for Project shop.") || strings.Contains(codex.Args[4], "\n") || codex.Args[5] != codexOpeningPrompt || codex.TypedPrompt != "" {
		t.Fatalf("codex launch = %#v", codex)
	}
	for _, rule := range []string{"posse queues each batch of Notices", noPollRule, "End your turn and let that message wake you."} {
		if !strings.Contains(codex.Args[4], rule) {
			t.Fatalf("codex instructions omitted %q: %s", rule, codex.Args[4])
		}
	}
	pi := launch("pi")
	extension := filepath.Join(home, "projects", "shop", "lead-pi-extension.ts")
	if !equalStrings(pi.Args, []string{"--append-system-prompt", instructions, "--extension", extension}) || pi.TypedPrompt != "" {
		t.Fatalf("pi launch = %#v", pi)
	}
	contents, err := os.ReadFile(extension)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(executable)
	if !strings.Contains(string(contents), "const posse = "+string(quoted)+";") || !strings.Contains(string(contents), `["lookout", "--json", "--quiet-routine", "--handoff"`) || !strings.Contains(string(contents), "wake = result.wake") || !strings.Contains(string(contents), `customType: "posse-notices"`) || !strings.Contains(string(contents), `origin: "posse"`) || !strings.Contains(string(contents), `triggerTurn: true`) || !strings.Contains(string(contents), `sessionHasReceipt(delivery)`) || !strings.Contains(string(contents), `pi.registerCommand("lowkey"`) || !strings.Contains(string(contents), `AssistantMessageComponent.prototype.updateContent`) || !strings.Contains(string(contents), `ui?.setHiddenThinkingLabel(active ? "" : undefined)`) {
		t.Fatalf("pi extension does not run this posse binary's lookout:\n%s", contents)
	}
	for _, frame := range []string{"░░▒▓", "░▒▓█", "▒▓█▓", "▓█▓▒", "█▓▒░", "▓▒░░"} {
		if !strings.Contains(string(contents), frame) {
			t.Errorf("generated pi extension is missing Slab frame %q", frame)
		}
	}
	for _, part := range []string{`const slabIntervalMs = 225;`, `placement: "aboveEditor"`, `ui.setWorkingVisible(!showSlab)`, `pi.on("agent_start"`, `pi.on("agent_settled"`, `clearInterval(animation)`} {
		if !strings.Contains(string(contents), part) {
			t.Errorf("generated pi extension is missing Slab lifecycle behavior %q", part)
		}
	}
	if strings.Contains(string(contents), "tui.appendPrompt") {
		t.Fatal("pi extension touches the composer")
	}
}

func TestLeadLaunchAutoApprovalCanBeDisabledPerKind(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[kinds.claude]\nlead_auto_approve = false\n\n[kinds.codex]\nlead_auto_approve = false\n\n[kinds.opencode]\nlead_auto_approve = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}

	for _, kind := range []string{"claude", "codex", "opencode"} {
		t.Run(kind, func(t *testing.T) {
			if cfg.Kinds[kind].LeadAutoApprove {
				t.Fatalf("%s lead_auto_approve = true after config opt-out", kind)
			}
			launch, err := service.prepareLeadLaunch(home, store.Project{Name: "shop"}, cfg, kind)
			if err != nil {
				t.Fatal(err)
			}
			for _, arg := range cfg.Kinds[kind].AutoApproveArgs {
				for _, launchArg := range launch.Args {
					if launchArg == arg {
						t.Fatalf("Lead auto-approve argument %q remained after opt-out: %#v", arg, launch.Args)
					}
				}
			}
		})
	}
}

func TestLeadLaunchDoesNotDuplicateProfileAutoApproveArgs(t *testing.T) {
	home := t.TempDir()
	service := testService(home, nil)
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}

	for _, kind := range []string{"claude", "codex", "opencode"} {
		t.Run(kind, func(t *testing.T) {
			approvalArgs := cfg.Kinds[kind].AutoApproveArgs
			profileName := "lead-" + kind
			cfg.Profiles[profileName] = config.Profile{Kind: kind, Args: append([]string(nil), approvalArgs...)}
			cfg.Lead.Profiles[kind] = profileName
			launch, err := service.prepareLeadLaunch(home, store.Project{Name: "shop"}, cfg, kind)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for start := 0; start+len(approvalArgs) <= len(launch.Args); start++ {
				if equalStrings(launch.Args[start:start+len(approvalArgs)], approvalArgs) {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("auto-approve args occur %d times in Lead argv: %#v", count, launch.Args)
			}
		})
	}
}

func TestClaudeLowkeyVersionEvidence(t *testing.T) {
	for version, verified := range map[string]bool{
		"2.1.272 (Claude Code)": true, "2.1.280 (Claude Code)": true,
		"2.1.282 (Claude Code)": true, "2.1.283 (Claude Code)": true,
		"2.1.284 (Claude Code)": false, "garbage": false,
	} {
		if claudeLowkeyVerified(version) != verified {
			t.Errorf("version %q verified = %v", version, !verified)
		}
	}
}

func TestDoctorClassifiesClaudeLowkeyVersions(t *testing.T) {
	bin := t.TempDir()
	claude := filepath.Join(bin, "claude")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, testCase := range []struct {
		version string
		status  string
		detail  string
	}{
		{"2.1.283 (Claude Code)", "ok", "function hooks verified"},
		{"2.1.284 (Claude Code)", "info", "not yet verified; the mod probes its hooks and falls back to stock rendering"},
		{"2.1.281 (Claude Code)", "warn", "function hooks unverified"},
	} {
		t.Run(testCase.version, func(t *testing.T) {
			if err := os.WriteFile(claude, []byte("#!/bin/sh\necho '"+testCase.version+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			service := testService(t.TempDir(), nil)
			var output bytes.Buffer
			cli := service.CLI()
			cli.Out, cli.ErrOut = &output, &output
			if code := cli.Run([]string{"doctor", "--json"}); code != 0 {
				t.Fatalf("doctor: %d %s", code, output.String())
			}
			var result struct {
				Checks []map[string]string `json:"checks"`
				Help   []map[string]string `json:"help"`
			}
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			for _, check := range result.Checks {
				if check["check"] != "Claude Code lowkey" {
					continue
				}
				if check["status"] != testCase.status || !strings.Contains(check["detail"], testCase.detail) {
					t.Fatalf("Claude lowkey doctor check = %#v, want %s containing %q", check, testCase.status, testCase.detail)
				}
				if testCase.status != "ok" {
					for _, help := range result.Help {
						if help["check"] == "Claude Code lowkey" && strings.Contains(help["action"], "POSSE_CLAUDE_LOWKEY_LIVE_E2E=1 scripts/claude-lowkey-live-e2e.sh") {
							return
						}
					}
					t.Fatalf("doctor help omitted live verification command: %s", output.String())
				}
				return
			}
			t.Fatalf("doctor omitted Claude lowkey check: %s", output.String())
		})
	}
}

func TestOpenCodeLeadLaunchAndPluginConfig(t *testing.T) {
	home := t.TempDir()
	service := testService(home, nil)
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profiles["open-lead"] = config.Profile{Kind: "opencode", Model: "provider/model", Args: []string{"--mini"}}
	cfg.Lead.Profiles["opencode"] = "open-lead"
	launch, err := service.prepareLeadLaunch(home, store.Project{Name: "shop"}, cfg, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(launch.Args, []string{"--auto", "--mini", "--model", "provider/model", "--prompt", codexOpeningPrompt}) || !launch.StartsBusy || launch.TypedPrompt != "" {
		t.Fatalf("OpenCode launch = %#v", launch)
	}
	if !strings.Contains(strings.Join(launch.Args, " "), "--auto") {
		t.Fatalf("OpenCode Lead did not receive its default auto-approve flag: %#v", launch.Args)
	}
	var inline struct {
		Plugin []string `json:"plugin"`
	}
	if err := json.Unmarshal([]byte(launch.Env["OPENCODE_CONFIG_CONTENT"]), &inline); err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(home, "projects", "shop", "lead-opencode-plugin.js")
	if !equalStrings(inline.Plugin, []string{"file://" + plugin}) {
		t.Fatalf("inline plugin config = %#v", inline)
	}
	contents, err := os.ReadFile(plugin)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(executable)
	for _, part := range []string{"const posse = " + string(quoted), "experimental.chat.system.transform", "output.system.push(instructions)", `"lookout", "--json", "--quiet-routine", "--handoff"`, "client.session.prompt(", "client.session.messages(", "Posse batch receipt:", "priorSession", "--receipt-outcome", "event.type === \"session.idle\""} {
		if !strings.Contains(string(contents), part) {
			t.Fatalf("OpenCode plugin missing %q", part)
		}
	}
	if strings.Contains(string(contents), "tui.appendPrompt") {
		t.Fatal("plugin touches the composer")
	}
}

func TestOpenCodeConfigPreservesInlineUserPlugins(t *testing.T) {
	merged, err := openCodePluginConfig(`{"plugin":["npm:existing"],"model":"provider/model"}`, "/tmp/lead.js")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Plugin []string `json:"plugin"`
		Model  string   `json:"model"`
	}
	if err := json.Unmarshal([]byte(merged), &cfg); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(cfg.Plugin, []string{"npm:existing", "file:///tmp/lead.js"}) || cfg.Model != "provider/model" {
		t.Fatalf("overwrote inline config: %s", merged)
	}
	if _, err := openCodePluginConfig(`{"plugin":"bad"}`, "/tmp/lead.js"); err == nil {
		t.Fatal("accepted invalid plugin config")
	}
}

func TestLeadLaunchWithoutConfiguredArgumentsUsesEmptyArray(t *testing.T) {
	home := t.TempDir()
	service := testService(home, nil)
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Kinds["custom"] = config.Kind{}

	launch, err := service.prepareLeadLaunch(home, store.Project{Name: "shop"}, cfg, "custom")
	if err != nil {
		t.Fatal(err)
	}
	if launch.Args == nil {
		t.Fatal("Lead args are nil")
	}
	encoded, err := json.Marshal(launch.Args)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("Lead args JSON = %s, want []", encoded)
	}
}

// processInfoAdapter reports the given process group as the pane's foreground
// until that group has exited, then the shell.
type processInfoAdapter struct {
	*herdr.Fake
	group int
}

func (a *processInfoAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method == "pane.process_info" {
		foreground := a.group
		if syscall.Kill(-a.group, 0) != nil {
			foreground = 1000000
		}
		return json.Marshal(map[string]any{"process_info": map[string]any{"foreground_process_group_id": foreground, "shell_pid": 1000000}})
	}
	return a.Fake.Call(ctx, method, params)
}

func TestStopPaneAgentEndsTheForegroundProcessGroup(t *testing.T) {
	for name, script := range map[string]string{
		"exits on SIGTERM": "echo ready; exec sleep 60",
		"ignores SIGTERM":  "trap '' TERM; echo ready; while :; do :; done",
	} {
		t.Run(name, func(t *testing.T) {
			previousStop, previousKill := agentStopGrace, agentKillGrace
			agentStopGrace, agentKillGrace = 300*time.Millisecond, 2*time.Second
			t.Cleanup(func() { agentStopGrace, agentKillGrace = previousStop, previousKill })
			agent := exec.Command("/bin/sh", "-c", script)
			agent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			stdout, err := agent.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := agent.Start(); err != nil {
				t.Fatal(err)
			}
			if _, err := stdout.Read(make([]byte, 6)); err != nil {
				t.Fatal(err)
			}
			exited := make(chan struct{})
			go func() { _ = agent.Wait(); close(exited) }()
			t.Cleanup(func() { _ = syscall.Kill(-agent.Process.Pid, syscall.SIGKILL); <-exited })
			service := testService(t.TempDir(), &processInfoAdapter{Fake: herdr.NewFake(), group: agent.Process.Pid})
			if err := service.stopPaneAgent(context.Background(), "w1:p1"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-exited:
			case <-time.After(3 * time.Second):
				t.Fatal("agent process group survived stopPaneAgent")
			}
		})
	}
}

func TestStopPaneAgentLeavesAShellPaneAlone(t *testing.T) {
	fake := herdr.NewFake()
	fake.Results["pane.process_info"] = json.RawMessage(`{"process_info":{"foreground_process_group_id":42,"shell_pid":42}}`)
	if err := testService(t.TempDir(), fake).stopPaneAgent(context.Background(), "w1:p1"); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("pane.process_info") != 1 {
		t.Fatalf("shell pane calls = %#v", fake.Calls)
	}
}

func TestUpCodexExecsWithItsOpeningPromptAndTypesNothing(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	t.Chdir(repo)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Focused: true}}}
	service := testService(home, fake)
	var output strings.Builder
	cli := service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"up", "--codex", "--name", "shop", "--yes"}); code != 0 {
		t.Fatalf("up --codex: code=%d output=%s", code, output.String())
	}
	plan := service.pendingLead
	if plan == nil || plan.NeedsPrompt || len(plan.Args) < 2 || plan.Args[len(plan.Args)-1] != codexOpeningPrompt || !equalStrings(plan.Args[:2], []string{"--dangerously-bypass-approvals-and-sandbox", "--sandbox"}) {
		t.Fatalf("codex Lead exec plan = %#v", plan)
	}
	if fake.CallCount("agent.prompt") != 0 {
		t.Fatalf("up --codex typed into the Lead pane: %#v", fake.Calls)
	}
}

func TestWaitLeadStartedAcceptsABusyStart(t *testing.T) {
	fake := herdr.NewFake()
	fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"working","launch_pending":true}}`)
	service := testService(t.TempDir(), fake)
	if err := service.waitLeadStarted(context.Background(), "w1:p1", leadLaunch{StartsBusy: true}); err != nil {
		t.Fatalf("busy start: %v", err)
	}
	if fake.CallCount("agent.get") != 1 {
		t.Fatalf("busy start polled %d times", fake.CallCount("agent.get"))
	}
}

func TestIdentityTextLabelsSetFieldsOnly(t *testing.T) {
	for identity, want := range map[config.IdentityRole]string{
		{Name: "Lead", Language: "en"}: "name: Lead; language: en",
		{Name: "Sheriff", Persona: "Direct, brief.", Language: "vi", AddressUser: "anh"}: "name: Sheriff; persona: Direct, brief.; language: vi; address_user: anh",
		{}: "none",
	} {
		if got := identityText(identity); got != want {
			t.Errorf("identityText(%#v) = %q, want %q", identity, got, want)
		}
	}
}

func TestNoticeRulesForbidPollingForEveryDelivery(t *testing.T) {
	for delivery, wake := range map[string]string{
		config.NoticeDeliveryLookout:        "After (re)starting it, end your turn and let it wake you.",
		config.NoticeDeliveryCodexQueue:     "End your turn and let that message wake you.",
		config.NoticeDeliveryPiExtension:    "End your turn and let that message wake you.",
		config.NoticeDeliveryOpenCodePlugin: "End your turn and let that message wake you.",
		config.NoticeDeliveryPrompt:         "End your turn and let that prompt wake you.",
	} {
		rule := noticeRule(delivery)
		if !strings.Contains(rule, noPollRule) || !strings.Contains(rule, wake) {
			t.Errorf("%s Notice rule = %q", delivery, rule)
		}
		if delivery == config.NoticeDeliveryLookout && !strings.Contains(rule, "If it returns with `state=stopped` and `reason=update`, restart `posse lookout` on your next wake.") {
			t.Errorf("Claude Lead lookout restart rule = %q", rule)
		}
	}
}

func TestWorkerWaitRulesByKind(t *testing.T) {
	background := workerWaitRules(config.Kind{BackgroundCommands: true})
	foreground := workerWaitRules(config.Kind{})
	const backgroundRule = "Run long commands (full test suites, e2e, builds) as background commands and never poll them"
	const ciRule = "Do not wait for PR CI after `posse holler done`; posse watches the PR and the Lead sends fix instructions."
	if !strings.Contains(background, backgroundRule) || !strings.Contains(background, ciRule) {
		t.Fatalf("background Worker rules = %q", background)
	}
	if strings.Contains(foreground, backgroundRule) || !strings.Contains(foreground, ciRule) {
		t.Fatalf("foreground Worker rules = %q", foreground)
	}
}
