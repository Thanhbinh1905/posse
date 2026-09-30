package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadMergesProjectOverridesAndPrependsDispatchRules(t *testing.T) {
	home := t.TempDir()
	global := `[identity.lead]
name = "Sheriff"
language = "vi"

[defaults]
max_workers = 5
landing_mode = "pr"
gate = ["go test ./..."]

[profiles.deep]
kind = "claude"
model = "opus"

[profiles.fast]
kind = "codex"

[[dispatch]]
type = "review"
use = "deep"

[dispatch.default]
use = "deep"
`
	project := `landing_mode = "no-mistakes"
gate = ["go test ./...", "go vet ./..."]

[autonomy]
yolo = true

[[dispatch]]
type = "ship"
use = "fast"
`
	writeConfig(t, filepath.Join(home, "config.toml"), global)
	writeConfig(t, filepath.Join(home, "projects", "shop", "config.toml"), project)
	cfg, err := Load(home, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.MaxWorkers != 5 || cfg.Defaults.LandingMode != "no-mistakes" || len(cfg.Defaults.Gate) != 2 {
		t.Fatalf("merged defaults = %#v", cfg.Defaults)
	}
	if cfg.Identity.Lead.Name != "Sheriff" || cfg.Identity.Lead.Language != "vi" {
		t.Fatalf("merged Identity = %#v", cfg.Identity.Lead)
	}
	if len(cfg.Dispatch) != 2 || cfg.Dispatch[0].Type != "ship" || cfg.Dispatch[1].Type != "review" {
		t.Fatalf("Dispatch Rules = %#v", cfg.Dispatch)
	}
	if cfg.DispatchDefault.Use != "deep" {
		t.Fatalf("default Profile = %q", cfg.DispatchDefault.Use)
	}
	if cfg.Autonomy.Review != "lead" || cfg.Autonomy.Land != "auto" {
		t.Fatalf("yolo expansion = %#v", cfg.Autonomy)
	}
	if cfg.Kinds["claude"].Prepare != "claude-trust" || cfg.Kinds["codex"].AutoApproveArgs[0] != "--dangerously-bypass-approvals-and-sandbox" {
		t.Fatalf("built-in Worker kinds were lost: %#v", cfg.Kinds)
	}
}

func TestLeadDeliveryDefaultsByKind(t *testing.T) {
	cfg, err := Load(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		delivery        string
		systemPrompt    []string
		leadAutoApprove bool
	}{
		"claude":   {NoticeDeliveryLookout, []string{"--append-system-prompt-file", "{file}"}, true},
		"codex":    {NoticeDeliveryCodexQueue, []string{"-c", "developer_instructions={text}"}, true},
		"pi":       {NoticeDeliveryPiExtension, []string{"--append-system-prompt", "{file}"}, false},
		"opencode": {NoticeDeliveryOpenCodePlugin, nil, true},
	}
	for kind, expected := range want {
		got := cfg.Kinds[kind]
		if got.NoticeDelivery != expected.delivery || !reflect.DeepEqual(got.SystemPromptArgs, expected.systemPrompt) || got.LeadAutoApprove != expected.leadAutoApprove {
			t.Errorf("%s Lead defaults = delivery %q system prompt %#v lead auto-approve %t, want %q %#v %t", kind, got.NoticeDelivery, got.SystemPromptArgs, got.LeadAutoApprove, expected.delivery, expected.systemPrompt, expected.leadAutoApprove)
		}
	}
}

func TestHarnessCapabilityDefaultsByKind(t *testing.T) {
	cfg, err := Load(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Kinds["claude"].BackgroundCommands || cfg.Kinds["codex"].BackgroundCommands || cfg.Kinds["pi"].BackgroundCommands {
		t.Fatalf("background_commands defaults = %#v", cfg.Kinds)
	}
	if !reflect.DeepEqual(cfg.Kinds["codex"].LeadArgs, []string{"--sandbox", "danger-full-access"}) || len(cfg.Kinds["claude"].LeadArgs) != 0 || len(cfg.Kinds["pi"].LeadArgs) != 0 || len(cfg.Kinds["opencode"].LeadArgs) != 0 {
		t.Fatalf("lead_args defaults = %#v", cfg.Kinds)
	}
	open := cfg.Kinds["opencode"]
	if !reflect.DeepEqual(open.ModelArgs, []string{"--model", "{model}"}) || !reflect.DeepEqual(open.ResumeArgs, []string{"--session", "{session}"}) || !reflect.DeepEqual(open.AutoApproveArgs, []string{"--auto"}) || !open.LeadAutoApprove || len(open.EffortArgs) != 0 || open.Prepare != "" || open.Steer || open.BackgroundCommands {
		t.Fatalf("OpenCode capability defaults = %#v", open)
	}
}

func TestLeadAutoApproveCanBeDisabledInConfig(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "config.toml"), "[kinds.claude]\nlead_auto_approve = false\n\n[kinds.codex]\nlead_auto_approve = false\n\n[kinds.opencode]\nlead_auto_approve = false\n")
	cfg, err := Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"claude", "codex", "opencode"} {
		if cfg.Kinds[kind].LeadAutoApprove {
			t.Errorf("%s lead_auto_approve = true, want false", kind)
		}
		if len(cfg.Kinds[kind].AutoApproveArgs) == 0 {
			t.Errorf("%s auto_approve_args were lost when opting out: %#v", kind, cfg.Kinds[kind])
		}
	}
	if cfg.Kinds["pi"].LeadAutoApprove {
		t.Fatal("Pi unexpectedly enables Lead auto-approval")
	}
}

func TestLookoutDeliveryRequiresBackgroundCommands(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "config.toml"), "[kinds.codex]\nnotice_delivery = \"lookout\"\n")
	_, err := Load(home, "")
	invalid, ok := err.(*InvalidError)
	if !ok || invalid.Key != "kinds.codex.notice_delivery" || !strings.Contains(invalid.Reason, "background_commands") {
		t.Fatalf("expected lookout without background_commands to be rejected, got %v", err)
	}
}

func TestPartialKindTableKeepsBuiltInLeadDelivery(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "config.toml"), "[kinds.pi]\nmodel_args = [\"--model\", \"{model}\"]\n")
	cfg, err := Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kinds["pi"].NoticeDelivery != NoticeDeliveryPiExtension || len(cfg.Kinds["pi"].SystemPromptArgs) == 0 {
		t.Fatalf("partial kinds.pi lost built-in Lead delivery: %#v", cfg.Kinds["pi"])
	}
	writeConfig(t, filepath.Join(home, "config.toml"), "[kinds.opencode]\nmodel_args = [\"-m\", \"{model}\"]\n")
	cfg, err = Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kinds["opencode"].NoticeDelivery != NoticeDeliveryOpenCodePlugin || !reflect.DeepEqual(cfg.Kinds["opencode"].ModelArgs, []string{"-m", "{model}"}) {
		t.Fatalf("partial kinds.opencode lost built-in delivery: %#v", cfg.Kinds["opencode"])
	}
}

func TestUnknownNoticeDeliveryIsRejected(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "config.toml"), "[kinds.codex]\nnotice_delivery = \"telepathy\"\n")
	_, err := Load(home, "")
	invalid, ok := err.(*InvalidError)
	if !ok || invalid.Key != "kinds.codex.notice_delivery" {
		t.Fatalf("expected notice_delivery config error, got %v", err)
	}
}

func TestSteeringDefaultsAndOverrides(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "config.toml"), "[kinds.claude]\nsteer = false\n[kinds.unknown]\nbackground_commands = true\n")
	cfg, err := Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kinds["claude"].Steer || !cfg.Kinds["codex"].Steer || !cfg.Kinds["pi"].Steer || cfg.Kinds["unknown"].Steer || cfg.Kinds["opencode"].Steer {
		t.Fatalf("steering capabilities = %#v", cfg.Kinds)
	}
}

func TestGlobalAutonomyIsRejected(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "config.toml"), "[autonomy]\nland='auto'\n")
	_, err := Load(home, "")
	invalid, ok := err.(*InvalidError)
	if !ok || invalid.Key != "autonomy" {
		t.Fatalf("expected autonomy config error, got %v", err)
	}
}

func TestLoadRejectsUnknownAndInvalidKeys(t *testing.T) {
	for _, tc := range []struct{ contents, key string }{
		{"[defaults]\nmax_workers=0\n", "defaults.max_workers"},
		{"[defaults]\nlanding_mode='unsafe'\n", "defaults.landing_mode"},
		{"[defaults]\nunknown=true\n", "defaults.unknown"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			home := t.TempDir()
			writeConfig(t, filepath.Join(home, "config.toml"), tc.contents)
			_, err := Load(home, "")
			invalid, ok := err.(*InvalidError)
			if !ok || invalid.Key != tc.key {
				t.Fatalf("expected %q config error, got %#v", tc.key, err)
			}
		})
	}
}

func TestDispatchDefaultOnly(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "config.toml"), "[profiles.fast]\nkind='codex'\n[dispatch.default]\nuse='fast'\n")
	cfg, err := Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DispatchDefault.Use != "fast" {
		t.Fatalf("dispatch.default.use = %q", cfg.DispatchDefault.Use)
	}
}

func TestWorkerIdentityAcceptsLegacyDisplayPrefix(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	writeConfig(t, path, "[identity.worker]\ndisplay_prefix = \"rider\"\n")
	cfg, err := Load(home, "shop")
	if err != nil || cfg.Identity.Worker.DisplayPrefix != "rider" {
		t.Fatalf("display_prefix config = %#v, %v", cfg.Identity.Worker, err)
	}
	writeConfig(t, path, "[identity.worker]\nname_prefix = \"rider\"\n")
	_, err = Load(home, "shop")
	invalid, ok := err.(*InvalidError)
	if !ok || invalid.File != path || !strings.Contains(invalid.Key, "name_prefix") {
		t.Fatalf("old worker identity key error = %#v", err)
	}
}

func TestProfileModelAndEffortRequireKindTemplatesInTheOwningFile(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		profile string
		key     string
		project bool
	}{
		{name: "global model", profile: "model = \"x1\"\n", key: "profiles.deep.model"},
		{name: "global effort", profile: "effort = \"high\"\n", key: "profiles.deep.effort"},
		{name: "project model", profile: "model = \"x1\"\n", key: "profiles.deep.model", project: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home := t.TempDir()
			globalPath := filepath.Join(home, "config.toml")
			global := "[kinds.custom]\nauto_approve_args = []\n"
			if !testCase.project {
				global += "\n[profiles.deep]\nkind = \"custom\"\n" + testCase.profile
			}
			writeConfig(t, globalPath, global)
			wantFile := globalPath
			if testCase.project {
				wantFile = filepath.Join(home, "projects", "shop", "config.toml")
				writeConfig(t, wantFile, "[profiles.deep]\nkind = \"custom\"\n"+testCase.profile)
			}
			_, err := Load(home, "shop")
			invalid, ok := err.(*InvalidError)
			if !ok || invalid.Key != testCase.key || invalid.File != wantFile {
				t.Fatalf("template-less Profile value error = %#v, want key=%q file=%q", err, testCase.key, wantFile)
			}
		})
	}
}

func writeConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
