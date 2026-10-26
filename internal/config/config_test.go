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
name = "Lead"
language = "vi"

[defaults]
max_workers = 5
merge_mode = "pr"
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
	project := `[defaults]
merge_mode = "local"
gate = ["go test ./...", "go vet ./..."]

[autonomy]
yolo = true

[[dispatch]]
type = "change"
use = "fast"
`
	writeConfig(t, filepath.Join(home, "config.toml"), global)
	writeConfig(t, filepath.Join(home, "projects", "shop", "config.toml"), project)
	cfg, err := Load(home, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.MaxWorkers != 5 || cfg.Defaults.MergeMode != "local" || !strings.Contains(strings.Join(cfg.Warnings, "\n"), "defaults.gate was removed") {
		t.Fatalf("merged defaults = %#v", cfg.Defaults)
	}
	if cfg.Identity.Lead.Name != "Lead" || cfg.Identity.Lead.Language != "vi" {
		t.Fatalf("merged Identity = %#v", cfg.Identity.Lead)
	}
	if len(cfg.Dispatch) != 2 || cfg.Dispatch[0].Type != "change" || cfg.Dispatch[1].Type != "review" {
		t.Fatalf("Dispatch Rules = %#v", cfg.Dispatch)
	}
	if cfg.DispatchDefault.Use != "deep" {
		t.Fatalf("default Profile = %q", cfg.DispatchDefault.Use)
	}
	if cfg.Autonomy.Review != "lead" || cfg.Autonomy.Merge != "auto" {
		t.Fatalf("yolo expansion = %#v", cfg.Autonomy)
	}
	if cfg.Kinds["claude"].Prepare != "claude-trust" || cfg.Kinds["codex"].AutoApproveArgs[0] != "--dangerously-bypass-approvals-and-sandbox" {
		t.Fatalf("built-in Worker kinds were lost: %#v", cfg.Kinds)
	}
}

func TestTargetBranchConfigDefaultsAndMemberOverrides(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "config.toml"), "[defaults]\ntarget_branch = \"integration\"\n")
	writeConfig(t, filepath.Join(home, "projects", "stack", "config.toml"), "[repositories.api]\ntarget_branch = \"staging\"\n")
	cfg, err := Load(home, "stack")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.TargetBranch != "integration" || cfg.Repositories["api"].TargetBranch != "staging" {
		t.Fatalf("target branch settings = defaults %q repositories %#v", cfg.Defaults.TargetBranch, cfg.Repositories)
	}
	for _, key := range []string{"defaults.target_branch", "repositories.api.target_branch"} {
		if IsUserOnly(key) {
			t.Errorf("branch selection key %s unexpectedly requires User-only approval", key)
		}
	}
	if _, found := Lookup("defaults.target_branch"); !found {
		t.Fatal("target branch default is absent from the config schema")
	}
	if _, found := Lookup("repositories.api.target_branch"); !found {
		t.Fatal("member target branch is absent from the config schema")
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
		"claude":   {NoticeDeliveryWatch, []string{"--append-system-prompt-file", "{file}"}, true},
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

func TestWatchDeliveryRequiresBackgroundCommands(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "config.toml"), "[kinds.codex]\nnotice_delivery = \"watch\"\n")
	_, err := Load(home, "")
	invalid, ok := err.(*InvalidError)
	if !ok || invalid.Key != "kinds.codex.notice_delivery" || !strings.Contains(invalid.Reason, "background_commands") {
		t.Fatalf("expected watch without background_commands to be rejected, got %v", err)
	}
}

func TestClaudeStopDeliveryDoesNotRequireBackgroundCommands(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "config.toml"), "[kinds.claude]\nbackground_commands = false\n")
	if _, err := Load(home, ""); err != nil {
		t.Fatalf("Claude Stop hook incorrectly requires Bash background support: %v", err)
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

func TestInterruptKeyDefaultsAndOverrides(t *testing.T) {
	home := t.TempDir()
	cfg, err := Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string][]string{
		"claude":   {"ctrl+c"},
		"codex":    {"esc"},
		"pi":       {"esc"},
		"opencode": {"esc"},
	} {
		if !reflect.DeepEqual(cfg.Kinds[kind].InterruptKeys, want) {
			t.Errorf("default %s interrupt keys = %v, want %v", kind, cfg.Kinds[kind].InterruptKeys, want)
		}
	}

	writeConfig(t, filepath.Join(home, "config.toml"), "[kinds.claude]\ninterrupt_keys = [\"esc\"]\n[kinds.custom]\ninterrupt_keys = [\"ctrl+x\", \"ctrl+c\"]\n")
	cfg, err = Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string][]string{
		"claude": {"esc"},
		"custom": {"ctrl+x", "ctrl+c"},
	} {
		if !reflect.DeepEqual(cfg.Kinds[kind].InterruptKeys, want) {
			t.Errorf("overridden %s interrupt keys = %v, want %v", kind, cfg.Kinds[kind].InterruptKeys, want)
		}
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
		{"[defaults]\nmerge_mode='unsafe'\n", "defaults.merge_mode"},
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

func TestLoadRefusesRemovedAutoRecover(t *testing.T) {
	for _, project := range []bool{false, true} {
		home := t.TempDir()
		path := filepath.Join(home, "config.toml")
		if project {
			path = filepath.Join(home, "projects", "shop", "config.toml")
		}
		writeConfig(t, path, "[defaults]\nauto_recover = false\n")
		_, err := Load(home, "shop")
		invalid, ok := err.(*InvalidError)
		if !ok || invalid.Key != "defaults.auto_recover" || invalid.File != path || !strings.Contains(invalid.Reason, "posse up --all") {
			t.Fatalf("project=%v: expected a removed defaults.auto_recover error naming its replacement, got %#v", project, err)
		}
	}
}

func TestValidationLoopDefaultsAndProjectOverrides(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "config.toml"), `[review_loop]
max_rounds = 2
guidance = ["docs/review.md"]

[gate_loop]
max_fix_rounds = 0

[ci_loop]
max_fix_rounds = 1
rerun_cancelled = 0
`)
	writeConfig(t, filepath.Join(home, "projects", "shop", "config.toml"), `[review_loop]
max_rounds = 1
auto_start = false
`)
	cfg, err := Load(home, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReviewLoop.MaxRounds != 1 || cfg.ReviewLoop.AutoStart || cfg.ReviewLoop.BlockAt != "P1" || !cfg.ReviewLoop.Enabled || !reflect.DeepEqual(cfg.ReviewLoop.Guidance, []string{"docs/review.md"}) {
		t.Fatalf("effective review loop = %#v", cfg.ReviewLoop)
	}
	if cfg.CILoop.MaxFixRounds != 1 || cfg.CILoop.RerunCancelled != 0 || !strings.Contains(strings.Join(cfg.Warnings, "\n"), "gate_loop was removed") {
		t.Fatalf("effective CI loop = %#v, warnings = %v", cfg.CILoop, cfg.Warnings)
	}

	writeConfig(t, filepath.Join(home, "config.toml"), "[review_loop]\nmax_rounds = 4\n")
	if _, err := Load(home, ""); err == nil || !strings.Contains(err.Error(), "review_loop.max_rounds") {
		t.Fatalf("max_rounds=4 config error = %v", err)
	}
}

func TestDispatchInheritanceAndDisabledKindDefaults(t *testing.T) {
	home := t.TempDir()
	cfg, err := Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.DispatchInheritance != "kind" {
		t.Fatalf("default dispatch inheritance = %q, want compatibility kind inheritance", cfg.Defaults.DispatchInheritance)
	}
	for _, kind := range []string{"claude", "codex", "pi", "opencode"} {
		if cfg.Kinds[kind].Disabled {
			t.Errorf("built-in kind %s is disabled by default", kind)
		}
	}
	writeConfig(t, filepath.Join(home, "config.toml"), "[defaults]\ndispatch_inheritance = 'lead_profile'\n[kinds.claude]\ndisabled = true\n")
	cfg, err = Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.DispatchInheritance != "lead_profile" || !cfg.Kinds["claude"].Disabled {
		t.Fatalf("explicit dispatch choices were not loaded: defaults=%#v kind=%#v", cfg.Defaults, cfg.Kinds["claude"])
	}
}

func TestKeepIdleAllowsZeroButRejectsNegativeAndBudgetsStayPositive(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "config.toml"), "[pool]\nkeep_idle = 0\n")
	cfg, err := Load(home, "")
	if err != nil || cfg.Pool.KeepIdle != 0 {
		t.Fatalf("keep_idle=0 config = %#v, %v", cfg.Pool, err)
	}
	for _, testCase := range []struct {
		key   string
		value int
	}{
		{key: "pool.keep_idle", value: -1},
		{key: "defaults.max_workers", value: 0},
		{key: "defaults.recovery_attempts", value: 0},
	} {
		t.Run(testCase.key, func(t *testing.T) {
			if err := ValidateSetting(testCase.key, testCase.value, false); err == nil {
				t.Fatalf("accepted %s=%d", testCase.key, testCase.value)
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
	writeConfig(t, path, "[identity.worker]\ndisplay_prefix = \"worker\"\n")
	cfg, err := Load(home, "shop")
	if err != nil || cfg.Identity.Worker.DisplayPrefix != "worker" {
		t.Fatalf("display_prefix config = %#v, %v", cfg.Identity.Worker, err)
	}
	writeConfig(t, path, "[identity.worker]\nname_prefix = \"worker\"\n")
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

func TestWorkerGoCacheIsUserConfigOnlyWithSafeBounds(t *testing.T) {
	home := t.TempDir()
	cfg, err := Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkerGoCache != (WorkerGoCache{MaxSize: "8GiB", MinAge: "6h", Interval: "1h"}) {
		t.Fatalf("default worker_go_cache = %#v", cfg.WorkerGoCache)
	}
	globalPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(globalPath, []byte("[worker_go_cache]\nmax_size = \"512MiB\"\nmin_age = \"3h\"\ninterval = \"30m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkerGoCache != (WorkerGoCache{MaxSize: "512MiB", MinAge: "3h", Interval: "30m"}) {
		t.Fatalf("User worker_go_cache = %#v", cfg.WorkerGoCache)
	}

	projectPath := filepath.Join(home, "projects", "shop", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte("[worker_go_cache]\nmax_size = \"1TiB\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(home, "shop"); err == nil || !strings.Contains(err.Error(), "worker_go_cache is valid only in the User config") {
		t.Fatalf("Project worker_go_cache load error = %v", err)
	}
	if issues := ValidationIssues(home, "shop"); len(issues) == 0 || !strings.Contains(issues[0].Error(), "valid only in the User config") {
		t.Fatalf("Project worker_go_cache validation issues = %v", issues)
	}
	if _, err := SetFile(projectPath, "worker_go_cache.min_age", "4h", true); err == nil || !strings.Contains(err.Error(), "valid only in the User config") {
		t.Fatalf("Project worker_go_cache edit error = %v", err)
	}

	for value, want := range map[string]string{"90m": "must be at least 2h0m0s", "soon": "must be a positive duration"} {
		if _, err := SetFile(globalPath, "worker_go_cache.min_age", value, false); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("min_age %q error = %v, want %q", value, err, want)
		}
	}
	for _, value := range []string{"0", "-1GiB", "8GB", "lots"} {
		if _, err := SetFile(globalPath, "worker_go_cache.max_size", value, false); err == nil || !strings.Contains(err.Error(), "positive size") {
			t.Fatalf("max_size %q error = %v", value, err)
		}
	}
}

func TestParseByteSize(t *testing.T) {
	for text, want := range map[string]int64{"1": 1, "2B": 2, "3KiB": 3 << 10, "4 MiB": 4 << 20, "8GiB": 8 << 30, "1TiB": 1 << 40} {
		got, err := ParseByteSize(text)
		if err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", text, got, err, want)
		}
	}
	if _, err := ParseByteSize("9999999999TiB"); err == nil {
		t.Error("overflowing size should be refused")
	}
}

func TestProfileFallbackMustNameAKnownProfileWithoutALoop(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		profiles string
		key      string
		reason   string
	}{
		{name: "chain", profiles: "[profiles.a]\nkind = \"claude\"\nfallback = \"b\"\n[profiles.b]\nkind = \"codex\"\nfallback = \"c\"\n[profiles.c]\nkind = \"pi\"\n"},
		{name: "unknown", profiles: "[profiles.a]\nkind = \"claude\"\nfallback = \"missing\"\n", key: "profiles.a.fallback", reason: "references unknown Profile missing"},
		{name: "self", profiles: "[profiles.a]\nkind = \"claude\"\nfallback = \"a\"\n", key: "profiles.a.fallback", reason: "fallback chain returns to a"},
		{name: "loop", profiles: "[profiles.a]\nkind = \"claude\"\nfallback = \"b\"\n[profiles.b]\nkind = \"codex\"\nfallback = \"a\"\n", key: "profiles.a.fallback", reason: "fallback chain returns to a"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home := t.TempDir()
			writeConfig(t, filepath.Join(home, "config.toml"), testCase.profiles)
			cfg, err := Load(home, "")
			if testCase.key == "" {
				if err != nil {
					t.Fatalf("valid fallback chain rejected: %v", err)
				}
				if cfg.Profiles["a"].Fallback != "b" || cfg.Profiles["b"].Fallback != "c" || cfg.Profiles["c"].Fallback != "" {
					t.Fatalf("fallbacks = %#v", cfg.Profiles)
				}
				return
			}
			invalid, ok := err.(*InvalidError)
			if !ok || invalid.Key != testCase.key || invalid.Reason != testCase.reason {
				t.Fatalf("fallback error = %#v, want key=%q reason=%q", err, testCase.key, testCase.reason)
			}
		})
	}
}
