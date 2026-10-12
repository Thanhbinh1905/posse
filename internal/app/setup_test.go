package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	setupassets "github.com/thanhbinh1905/posse/internal/setup"
)

func TestDevinConfigRootUsesXDGDefaultAndOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	got, err := integrationConfigRoot("devin")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "devin"); got != want {
		t.Fatalf("default config root = %q, want %q", got, want)
	}

	configHome := filepath.Join(home, "custom-config")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	got, err = integrationConfigRoot("devin")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(configHome, "devin"); got != want {
		t.Fatalf("overridden config root = %q, want %q", got, want)
	}
}

func TestSetupPlanPreflightsHookJSONAndSkillPath(t *testing.T) {
	root := t.TempDir()
	claudeDir := filepath.Join(root, "claude")
	codexDir := filepath.Join(root, "codex")
	agentsDir := filepath.Join(root, "agents", "skills")
	if err := os.MkdirAll(filepath.Join(claudeDir, "skills", "posse"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte("{invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := &Service{}
	plan := service.setupPlan(root, "/usr/bin/posse", "test", setupDirs{claude: claudeDir, codex: codexDir, agents: agentsDir}, setupManifest{}, false, setupInspection{GlobalSkillsMode: "install"})
	var hookAction, skillAction string
	for _, row := range plan {
		switch row["target"] {
		case filepath.Join(claudeDir, "settings.json"):
			hookAction, _ = row["action"].(string)
		case filepath.Join(agentsDir, "posse"):
			skillAction, _ = row["action"].(string)
		}
	}
	if hookAction != "invalid" {
		t.Fatalf("hook JSON action = %q, want invalid", hookAction)
	}
	if skillAction != "conflict" {
		t.Fatalf("real Claude skill directory action = %q, want conflict", skillAction)
	}
}

func TestSetupPlanReportsReadOnlySymlinkSnippetAndDoctorRecognizesIt(t *testing.T) {
	root := t.TempDir()
	claudeDir, codexDir := filepath.Join(root, "claude"), filepath.Join(root, "codex")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "managed-settings.json")
	if err := os.WriteFile(target, []byte("{\"hooks\":{}}"), 0o444); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(claudeDir, "settings.json")
	if err := os.Symlink(target, configured); err != nil {
		t.Fatal(err)
	}
	command := hookCommand("/opt/posse/bin/posse", sessionStartHook)
	plan := (&Service{}).setupPlan(root, "/opt/posse/bin/posse", "1", setupDirs{claude: claudeDir, codex: codexDir, agents: filepath.Join(root, "agents")}, setupManifest{}, false, setupInspection{})
	var hookRow map[string]any
	for _, row := range plan {
		if row["target"] == configured {
			hookRow = row
			break
		}
	}
	if hookRow == nil || hookRow["action"] != "setup_hook_symlink" || hookRow["resolved_target"] != target || hookRow["snippet"] != hookSnippet(sessionStartHook, command) {
		t.Fatalf("read-only symlink plan row = %#v", hookRow)
	}
	_, _, err := installHook(configured, sessionStartHook, command, setupHookRecord{})
	var blocked *setupHookSymlinkError
	if !errors.As(err, &blocked) || blocked.Snippet != hookSnippet(sessionStartHook, command) {
		t.Fatalf("apply symlink report = %#v, %v", blocked, err)
	}
	manual := filepath.Join(root, "manual-settings.json")
	if err := os.WriteFile(manual, []byte(hookSnippet(sessionStartHook, command)), 0o600); err != nil {
		t.Fatal(err)
	}
	if !hookCommandExists(manual, sessionStartHook, command) {
		t.Fatal("doctor hook recognition did not accept the user-added snippet")
	}
	readOnly := filepath.Join(root, "read-only-settings.json")
	original := []byte("{\"hooks\":{}}")
	if err := os.WriteFile(readOnly, original, 0o444); err != nil {
		t.Fatal(err)
	}
	_, _, err = installHook(readOnly, sessionStartHook, command, setupHookRecord{})
	if !errors.As(err, &blocked) || blocked.Snippet != hookSnippet(sessionStartHook, command) {
		t.Fatalf("apply regular read-only file report = %#v, %v", blocked, err)
	}
	if data, err := os.ReadFile(readOnly); err != nil || !bytes.Equal(data, original) {
		t.Fatalf("regular read-only file changed: data=%q err=%v", data, err)
	}
}

func TestHookWritePathRefusesStoreTargets(t *testing.T) {
	_, err := hookWritePath("/nix/store/posse-settings.json")
	var blocked *setupHookSymlinkError
	if !errors.As(err, &blocked) || blocked.Path != "/nix/store/posse-settings.json" {
		t.Fatalf("store target error = %#v, %v", blocked, err)
	}
}

func TestSetupUninstallLeavesPreexistingUserHookUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	command := hookCommand("/opt/posse/bin/posse", sessionStartHook)
	original := []byte("{\n  \"hooks\": {\n    \"SessionStart\": [\n      {\"matcher\":\"startup|resume|compact\",\"hooks\":[{\"type\":\"command\",\"command\":" + mustJSONString(t, command) + ",\"timeout\":5}]}\n    ]\n  }\n}\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	record, changed, err := installHook(path, sessionStartHook, command, setupHookRecord{})
	if err != nil || changed {
		t.Fatalf("install existing hook = changed %v record %#v err %v", changed, record, err)
	}
	if changed, err := removeHook(record); err != nil || changed {
		t.Fatalf("uninstall preexisting hook = changed %v err %v", changed, err)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, original) {
		t.Fatalf("preexisting hook changed: data=%q err=%v", data, err)
	}
}

func TestSetupManifestOwnsOnlyHooksItInserted(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("# existing config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeDir, codexDir := filepath.Join(home, "claude"), filepath.Join(home, "codex")
	existing := map[string]any{}
	for _, spec := range setupHooks {
		existing[spec.Event] = []any{setupHookGroup(spec, hookCommand("/opt/posse/bin/posse", spec))}
	}
	encoded, err := json.MarshalIndent(map[string]any{"hooks": existing}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	original := append(encoded, '\n')
	paths := []string{filepath.Join(claudeDir, "settings.json"), filepath.Join(codexDir, "hooks.json")}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifestPath := filepath.Join(home, setupManifestName)
	adapter := herdr.NewFake()
	adapter.RunOut["plugin link "+filepath.Join(home, "plugin", "herdr-plugin.toml")] = []byte("{}")
	adapter.RunOut["plugin uninstall "+possePluginID] = []byte("{}")
	service := &Service{Herdr: adapter}
	if _, err := service.applySetup(context.Background(), home, manifestPath, setupManifest{}, "/opt/posse/bin/posse", "test", setupDirs{claude: claudeDir, codex: codexDir, agents: filepath.Join(home, "agents", "skills")}, setupInspection{}); err != nil {
		t.Fatal(err)
	}
	manifest, found, err := readSetupManifest(manifestPath)
	if err != nil || !found || len(manifest.Hooks) != 0 {
		t.Fatalf("manifest recorded preexisting hooks: found=%v hooks=%#v err=%v", found, manifest.Hooks, err)
	}
	if err := service.uninstallSetup(&axi.Context{Context: context.Background(), Out: &bytes.Buffer{}}, home, manifestPath, manifest, true); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, original) {
			t.Fatalf("preexisting hook %s changed: data=%q err=%v", path, data, err)
		}
	}
}

func TestSetupHookInsertionMatchesIndentedJSON(t *testing.T) {
	command := hookCommand("/opt/posse/bin/posse", sessionStartHook)
	original := []byte("{\n  \"hooks\": {\n    \"SessionStart\": [\n      {\n        \"matcher\": \"UserPromptSubmit\",\n        \"hooks\": [\n          {\"type\": \"command\", \"command\": \"echo user\", \"timeout\": 5}\n        ]\n      }\n    ]\n  }\n}\n")
	want := "{\n  \"hooks\": {\n    \"SessionStart\": [\n      {\n        \"matcher\": \"UserPromptSubmit\",\n        \"hooks\": [\n          {\"type\": \"command\", \"command\": \"echo user\", \"timeout\": 5}\n        ]\n      },\n      {\n        \"matcher\": \"startup|resume|compact\",\n        \"hooks\": [\n          {\n            \"type\": \"command\",\n            \"command\": " + mustJSONString(t, command) + ",\n            \"timeout\": 5\n          }\n        ]\n      }\n    ]\n  }\n}\n"
	updated, _, _, _, changed, err := installHookBytes(original, sessionStartHook, command, "")
	if err != nil || !changed {
		t.Fatalf("install hook changed=%v err=%v", changed, err)
	}
	if !json.Valid(updated) {
		t.Fatalf("installed hook JSON is invalid: %s", updated)
	}
	if got := string(updated); got != want {
		t.Fatalf("installed hook formatting mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
	crlfOriginal := bytes.ReplaceAll(original, []byte("\n"), []byte("\r\n"))
	crlfUpdated, _, _, _, _, err := installHookBytes(crlfOriginal, sessionStartHook, command, "")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(crlfUpdated, []byte("\n")) && !bytes.Contains(crlfUpdated, []byte("\r\n")) {
		t.Fatalf("CRLF input lost its line ending style: %q", crlfUpdated)
	}
	if expected := bytes.ReplaceAll([]byte(want), []byte("\n"), []byte("\r\n")); !bytes.Equal(crlfUpdated, expected) {
		t.Fatalf("inserted hook did not retain CRLF formatting: %q", crlfUpdated)
	}
}

func TestSetupHookEventInsertionMatchesIndentedJSON(t *testing.T) {
	command := hookCommand("/opt/posse/bin/posse", guardHook)
	original := []byte("{\n  \"hooks\": {\n    \"Stop\": [\n      {\n        \"hooks\": []\n      }\n    ]\n  }\n}\n")
	want := "{\n  \"hooks\": {\n    \"Stop\": [\n      {\n        \"hooks\": []\n      }\n    ],\n    \"PreToolUse\": [\n      {\n        \"hooks\": [\n          {\n            \"type\": \"command\",\n            \"command\": " + mustJSONString(t, command) + ",\n            \"timeout\": 5\n          }\n        ]\n      }\n    ]\n  }\n}\n"
	updated, _, createdKey, createdGroup, changed, err := installHookBytes(original, guardHook, command, "")
	if err != nil || !changed || !createdKey || !createdGroup {
		t.Fatalf("install guard hook changed=%v key=%v group=%v err=%v", changed, createdKey, createdGroup, err)
	}
	if got := string(updated); got != want {
		t.Fatalf("inserted hook event formatting mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func mustJSONString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestSetupAcceptsBinaryOverrideFlag(t *testing.T) {
	service := &Service{}
	output, errorsOut := &bytes.Buffer{}, &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	if code := cli.Run([]string{"setup", "--binary", "/opt/posse/bin/posse"}); code == 0 || strings.Contains(output.String()+errorsOut.String(), "unknown setup flag --binary") {
		t.Fatalf("setup did not accept --binary: code=%d output=%s stderr=%s", code, output, errorsOut)
	}
}

func TestSetupUnsafeBinaryKeepsStructuredCodeAndHelp(t *testing.T) {
	home := t.TempDir()
	service := testService(home, herdr.NewFake())
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output, errorsOut := &bytes.Buffer{}, &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	if code := cli.Run([]string{"setup", "--check", "--binary", executable, "--json"}); code == 0 {
		t.Fatalf("unsafe temporary binary was accepted: %s", output)
	}
	var failure axi.Error
	if err := json.Unmarshal(output.Bytes(), &failure); err != nil {
		t.Fatalf("setup error is not structured JSON: %s stderr=%s err=%v", output, errorsOut, err)
	}
	if failure.Code != "setup_binary_unsafe" || len(failure.Help) != 1 || failure.Help[0] != "Install to `~/.local/bin/posse` or pass `--binary <path>` with a stable executable" {
		t.Fatalf("unsafe binary error lost its domain code or action: %#v", failure)
	}
}

func TestSetupCheckUsesUniformPlanAndConfiguredAvailableKinds(t *testing.T) {
	service, home, adapter, binDir := setupOutputFixture(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	dotfile := filepath.Join(home, "dotfiles", "home", ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(dotfile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dotfile, []byte(`{"hooks":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dotfile, settings); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	adapter.RunOut["integration status"] = []byte("claude: installed\ncodex: installed\ncursor: not installed\nopencode: not installed\n")
	output, errorsOut := &bytes.Buffer{}, &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	if code := cli.Run([]string{"setup", "--check", "--binary", "/usr/bin/true", "--json"}); code != 0 {
		t.Fatalf("setup check failed: output=%s stderr=%s", output, errorsOut)
	}
	var result struct {
		Plan                []map[string]string `json:"plan"`
		Prerequisites       []map[string]string `json:"prerequisites"`
		AvailableAgentKinds []string            `json:"available_agent_kinds"`
		Help                []string            `json:"help"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("setup check did not return JSON: %s: %v", output, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["needs_apply"]; exists {
		t.Fatalf("setup check repeated its plan in needs_apply: %s", output)
	}
	if len(result.Plan) == 0 || len(result.Prerequisites) == 0 {
		t.Fatalf("setup output omitted plan or prerequisites: %s", output)
	}
	needsApply, hasNoop := false, false
	for _, row := range result.Plan {
		if len(row) != 4 || row["step"] == "" || row["target"] == "" || row["action"] == "" {
			t.Fatalf("plan row does not match step,target,action,note: %#v", row)
		}
		if row["action"] == "keep" {
			hasNoop = true
		} else {
			needsApply = true
		}
		if row["target"] == dotfile && row["note"] != "edited through link "+settings {
			t.Fatalf("symlinked hook target lacks its resolved path and note: %#v", row)
		}
		if row["target"] == settings {
			t.Fatalf("plan points at the link instead of final dotfile: %#v", row)
		}
		if (row["target"] == "cursor" || row["target"] == "opencode") && row["step"] == "integration" {
			t.Fatalf("unconfigured detected kind was planned for install: %#v", row)
		}
	}
	for _, row := range result.Prerequisites {
		if len(row) != 3 || row["tool"] == "" || row["status"] == "" {
			t.Fatalf("prerequisite row does not match tool,path,status: %#v", row)
		}
	}
	if !strings.Contains(strings.Join(result.AvailableAgentKinds, ","), "cursor") || !strings.Contains(strings.Join(result.AvailableAgentKinds, ","), "opencode") {
		t.Fatalf("unconfigured detected kinds were not listed as available: %#v PATH=%s", result.AvailableAgentKinds, binDir)
	}
	if !strings.Contains(strings.Join(result.Help, "\n"), "Codex will ask once to trust the new hook") {
		t.Fatalf("setup check omitted the Codex hook trust note: %#v", result.Help)
	}
	if !needsApply || !hasNoop {
		t.Fatalf("plan action column did not distinguish changes from no-ops: %#v", result.Plan)
	}
	output.Reset()
	cli = service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	if code := cli.Run([]string{"setup", "--check", "--binary", "/usr/bin/true"}); code != 0 {
		t.Fatalf("setup text output failed: %s stderr=%s", output, errorsOut)
	}
	for _, header := range []string{"{step,target,action,note}:", "{tool,path,status}:"} {
		if !strings.Contains(output.String(), header) {
			t.Errorf("setup text output omitted ordered table header %q: %s", header, output)
		}
	}
	if strings.Contains(output.String(), "needs_apply") {
		t.Errorf("setup text output repeated the plan as needs_apply: %s", output)
	}
}

func TestUninstallSkillDoesNotFollowReplacementDirectorySymlink(t *testing.T) {
	_, home, _, _ := setupOutputFixture(t)
	dirs := setupDirs{claude: filepath.Join(home, ".claude"), agents: filepath.Join(home, ".agents", "skills")}
	record, _, err := installSkill("posse", dirs.agents, dirs.claude, "1", setupSkillRecord{})
	if err != nil {
		t.Fatal(err)
	}
	foreignDir := filepath.Join(home, "foreign-skill")
	if err := os.MkdirAll(foreignDir, 0o700); err != nil {
		t.Fatal(err)
	}
	contents, err := setupassets.Skill("posse")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreignDir, "SKILL.md"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreignDir, ".posse-version"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dirs.agents, "posse")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreignDir, filepath.Join(dirs.agents, "posse")); err != nil {
		t.Fatal(err)
	}
	unchangedFile, modifiedFile := ownedSkillFileStatus(filepath.Join(dirs.agents, "posse", "SKILL.md"), record.FileHash)
	if unchangedFile || !modifiedFile || !skillRecordHasModifiedOwnedParts(record) {
		t.Fatal("ownership detection followed the replacement skill-directory symlink")
	}
	if err := uninstallSkill(record); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"SKILL.md", ".posse-version"} {
		if _, err := os.Stat(filepath.Join(foreignDir, name)); err != nil {
			t.Fatalf("uninstall removed foreign file %s: %v", name, err)
		}
	}
	if target, err := os.Readlink(filepath.Join(dirs.agents, "posse")); err != nil || target != foreignDir {
		t.Fatalf("uninstall changed replacement directory symlink: target=%q err=%v", target, err)
	}
}

func TestDoctorOffersRemovalOfUnchangedGlobalSkills(t *testing.T) {
	service, home, _, _ := setupOutputFixture(t)
	dirs := setupDirs{claude: filepath.Join(home, ".claude"), agents: filepath.Join(home, ".agents", "skills")}
	manifest := setupManifest{Version: "1"}
	for _, name := range []string{"posse", "posse-setup"} {
		record, _, err := installSkill(name, dirs.agents, dirs.claude, "1", setupSkillRecord{})
		if err != nil {
			t.Fatal(err)
		}
		manifest.Skills = append(manifest.Skills, record)
	}
	if err := writeSetupManifest(filepath.Join(home, setupManifestName), manifest); err != nil {
		t.Fatal(err)
	}
	result, err := service.collectDoctorChecks(&axi.Context{Context: context.Background()})
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range result.Checks {
		if check.Name == "global skills" {
			if check.Status != "warn" || !strings.Contains(check.Detail, "posse, posse-setup") || check.Action != "Run `posse setup --remove-global-skills` to remove unchanged Posse-owned files" {
				t.Fatalf("doctor global skills check = %#v", check)
			}
			return
		}
	}
	t.Fatalf("doctor omitted owned global skills: %#v", result.Checks)
}

func TestDoctorIsCompactAndOnlyChecksConfiguredAgentKinds(t *testing.T) {
	service, _, adapter, _ := setupOutputFixture(t)
	adapter.RunOut["integration status"] = []byte("claude: installed\ncodex: installed\ncursor: not installed\nopencode: not installed\n")
	adapter.RunOut["plugin log list --plugin posse.herdr"] = []byte("[]")
	output, errorsOut := &bytes.Buffer{}, &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	if code := cli.Run([]string{"doctor", "--json"}); code != 0 {
		t.Fatalf("doctor failed: output=%s stderr=%s", output, errorsOut)
	}
	var compact struct {
		Checks         []map[string]string `json:"checks"`
		Help           []map[string]string `json:"help"`
		AgentManifests any                 `json:"agent_manifests"`
	}
	if err := json.Unmarshal(output.Bytes(), &compact); err != nil {
		t.Fatalf("doctor did not return JSON: %s: %v", output, err)
	}
	if len(compact.Checks) == 0 || compact.AgentManifests != nil {
		t.Fatalf("default doctor output is not compact: %s", output)
	}
	var sawAvailable bool
	for _, row := range compact.Checks {
		if len(row) != 3 || row["check"] == "" || !oneOfString(row["status"], "ok", "info", "warn", "fail") {
			t.Fatalf("doctor check row does not match check,status,detail: %#v", row)
		}
		if strings.HasPrefix(row["check"], "agent integration cursor") || strings.HasPrefix(row["check"], "agent integration opencode") {
			t.Fatalf("doctor reported unconfigured agent integration as missing: %#v", row)
		}
		if row["check"] == "Herdr Agents sidebar layout" {
			t.Fatal("doctor still requests a retired sidebar layout")
		}
		if row["check"] == "other agent kinds" && row["status"] == "ok" && strings.Contains(row["detail"], "cursor") && strings.Contains(row["detail"], "opencode") {
			sawAvailable = true
		}
	}
	if !sawAvailable {
		t.Fatalf("doctor did not report available kinds: %s", output)
	}
	for _, row := range compact.Help {
		if row["check"] == "" || row["action"] == "" {
			t.Fatalf("doctor help omitted an action for a non-ok check: %#v", row)
		}
	}

	output.Reset()
	cli = service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	if code := cli.Run([]string{"doctor"}); code != 0 {
		t.Fatalf("doctor text output failed: %s stderr=%s", output, errorsOut)
	}
	if !strings.Contains(output.String(), "{check,status,detail}:") {
		t.Fatalf("doctor text output omitted the ordered compact checks table: %s", output)
	}

	output.Reset()
	cli = service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	if code := cli.Run([]string{"doctor", "--full", "--json"}); code != 0 {
		t.Fatalf("doctor --full failed: output=%s stderr=%s", output, errorsOut)
	}
	var full map[string]any
	if err := json.Unmarshal(output.Bytes(), &full); err != nil || full["agent_manifests"] == nil {
		t.Fatalf("doctor --full omitted agent manifests: output=%s err=%v", output, err)
	}
}

func setupOutputFixture(t *testing.T) (*Service, string, *herdr.Fake, string) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "projects", "shop"), 0o700); err != nil {
		t.Fatal(err)
	}
	global := "[lead]\nkind = \"claude\"\n\n[lead.profiles]\ncodex = \"fast\"\n\n[profiles.fast]\nkind = \"codex\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "projects", "shop", "config.toml"), []byte("[profiles.project]\nkind = \"claude\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude", "codex", "cursor-agent", "opencode", "herdr"} {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("POSSE_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	adapter := herdr.NewFake()
	adapter.Results["server.agent_manifests"] = []byte(`{"agents":[{"kind":"claude"},{"kind":"codex"},{"kind":"cursor"},{"kind":"opencode"}]}`)
	adapter.Results["plugin.list"] = []byte(`[{"id":"posse.herdr","path":"/tmp/posse-plugin.toml"}]`)
	adapter.RunOut["plugin list --json"] = []byte(`{"id":"cli:plugin","result":{"type":"plugin_list","plugins":[{"plugin_id":"posse.herdr","manifest_path":"/tmp/posse-plugin.toml"}]}}`)
	return testService(home, adapter), home, adapter, binDir
}

func TestSetupBinaryPathSafetyAndInstalledPreference(t *testing.T) {
	for _, path := range []string{"/tmp/go-build123/posse", "/nix/store/abc-posse/bin/posse"} {
		if !isUnsafeSetupBinaryPath(path) {
			t.Errorf("temporary/store path %q was accepted", path)
		}
	}
	if isUnsafeSetupBinaryPath("/opt/posse/bin/posse") {
		t.Fatal("stable installed path was rejected")
	}

	if !isUnsafeSetupBinaryPath(filepath.Join("/tmp", "posse-fixture", ".local", "bin", "posse")) {
		t.Fatal("path under /tmp was not treated as unsafe")
	}
	installedHome, err := os.MkdirTemp("/var/tmp", "posse-installed-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(installedHome) })
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	installedPath := filepath.Join(installedHome, ".local", "bin", "posse")
	if err := os.MkdirAll(filepath.Dir(installedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, installedPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", installedHome)
	got, err := resolveSetupBinary("")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(installedPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resolved executable = %q, want installed path %q", got, want)
	}
}

func TestApplySetupRelinksChangedPluginManifest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("# existing config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pluginPath := filepath.Join(home, "plugin", "herdr-plugin.toml")
	oldPlugin, err := setupassets.PluginManifest("/opt/posse/bin/posse", "old")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(pluginPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginPath, oldPlugin, 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := herdr.NewFake()
	adapter.RunOut["plugin link "+pluginPath] = []byte("{}")
	service := &Service{Herdr: adapter}
	claudeDir, codexDir := filepath.Join(home, "claude"), filepath.Join(home, "codex")
	_, err = service.applySetup(context.Background(), home, filepath.Join(home, setupManifestName), setupManifest{
		PluginPath: pluginPath, PluginHash: fileHash(oldPlugin), PluginFileMade: true, PluginLinked: true,
	}, "/opt/posse/bin/posse", "new", setupDirs{claude: claudeDir, codex: codexDir, agents: filepath.Join(home, ".agents", "skills")}, setupInspection{
		PluginPath: pluginPath, PluginPresent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := adapter.RunCount("plugin link " + pluginPath); got != 1 {
		t.Fatalf("plugin link runs after manifest update = %d, want 1", got)
	}
}

func TestPluginPathComparisonResolvesSymlinks(t *testing.T) {
	directory := t.TempDir()
	materialized := filepath.Join(directory, "plugin.toml")
	if err := os.WriteFile(materialized, []byte("plugin"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "plugin-alias.toml")
	if err := os.Symlink(materialized, alias); err != nil {
		t.Fatal(err)
	}
	if !samePathAfterEval(alias, materialized) {
		t.Fatalf("path alias %q was not resolved to %q", alias, materialized)
	}
}

func TestSetupRepairsPreexistingIntegrationWithoutOwningIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("# existing config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := herdr.NewFake()
	adapter.RunOut["integration install claude"] = []byte("repaired")
	adapter.RunOut["integration status"] = []byte("claude: needs repair\n")
	adapter.RunOut["plugin link "+filepath.Join(home, "plugin", "herdr-plugin.toml")] = []byte("{}")
	adapter.RunOut["plugin uninstall "+possePluginID] = []byte("{}")
	status, err := integrationStatus(adapter, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Herdr: adapter}
	claudeDir, codexDir := filepath.Join(home, "claude"), filepath.Join(home, "codex")
	manifestPath := filepath.Join(home, setupManifestName)
	if _, err := service.applySetup(context.Background(), home, manifestPath, setupManifest{}, "/usr/bin/true", "1", setupDirs{claude: claudeDir, codex: codexDir, agents: filepath.Join(home, ".agents", "skills")}, setupInspection{
		Kinds: []string{"claude"}, Integrations: status,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, found, err := readSetupManifest(manifestPath)
	if err != nil || !found {
		t.Fatalf("setup manifest = %#v, found=%v err=%v", manifest, found, err)
	}
	if len(manifest.Integrations) != 0 {
		t.Fatalf("preexisting integration was recorded as installed: %v", manifest.Integrations)
	}
	adapter.RunErrors["integration uninstall claude"] = errors.New("integration uninstall must not run for a preexisting integration")
	if err := service.uninstallSetup(&axi.Context{Context: context.Background(), Out: &bytes.Buffer{}}, home, manifestPath, manifest, true); err != nil {
		t.Fatalf("uninstall attempted to remove the preexisting integration: %v", err)
	}
}

func TestSetupUninstallPersistsCompletedItemsBeforeFailure(t *testing.T) {
	home := t.TempDir()
	manifestPath := filepath.Join(home, setupManifestName)
	manifest := setupManifest{Version: "1", Binary: "/opt/posse/bin/posse", PluginPath: filepath.Join(home, "plugin", "herdr-plugin.toml"), PluginLinked: true, Integrations: []string{"claude"}}
	if err := writeSetupManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	adapter := herdr.NewFake()
	adapter.RunErrors["integration uninstall claude"] = errors.New("simulated integration uninstall failure")
	adapter.RunOut["integration uninstall claude"] = []byte("removed")
	adapter.RunOut["plugin uninstall "+possePluginID] = []byte("{}")
	service := &Service{Herdr: adapter}
	ctx := &axi.Context{Context: context.Background(), Out: &bytes.Buffer{}}
	if err := service.uninstallSetup(ctx, home, manifestPath, manifest, true); err == nil {
		t.Fatal("uninstall ignored the integration failure")
	}
	partial, found, err := readSetupManifest(manifestPath)
	if err != nil || !found {
		t.Fatalf("partial manifest = %#v, found=%v err=%v", partial, found, err)
	}
	if partial.PluginLinked || len(partial.Integrations) != 1 || partial.Integrations[0] != "claude" {
		t.Fatalf("manifest did not record completed plugin unlink: %#v", partial)
	}
	delete(adapter.RunErrors, "integration uninstall claude")
	if err := service.uninstallSetup(ctx, home, manifestPath, partial, true); err != nil {
		t.Fatalf("uninstall did not resume after the completed item: %v", err)
	}
	if _, found, err := readSetupManifest(manifestPath); err != nil || found {
		t.Fatalf("completed uninstall retained manifest: found=%v err=%v", found, err)
	}
}

func TestSetupInstallsAndRemovesThePiWorkerGuard(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	piDir := filepath.Join(home, "pi")
	dirs := setupDirs{claude: filepath.Join(home, "claude"), codex: filepath.Join(home, "codex"), agents: filepath.Join(home, ".agents", "skills"), pi: piDir}
	adapter := herdr.NewFake()
	adapter.RunOut["plugin link "+filepath.Join(home, "plugin", "herdr-plugin.toml")] = []byte("{}")
	adapter.RunOut["plugin uninstall "+possePluginID] = []byte("{}")
	service := &Service{Herdr: adapter}
	manifestPath := filepath.Join(home, setupManifestName)
	if _, err := service.applySetup(context.Background(), home, manifestPath, setupManifest{}, "/opt/posse/bin/posse", "1", dirs, setupInspection{}); err != nil {
		t.Fatal(err)
	}
	guard := filepath.Join(piDir, "extensions", "posse-worker-guard.ts")
	if _, err := os.Stat(guard); !os.IsNotExist(err) {
		t.Fatalf("pi guard was installed without a pi agent directory: %v", err)
	}
	if err := os.MkdirAll(piDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, row := range service.setupPlan(home, "/opt/posse/bin/posse", "1", dirs, setupManifest{}, false, setupInspection{}) {
		if row["step"] == "pi_guard" && row["action"] != "install" {
			t.Fatalf("pi guard plan = %#v, want install", row)
		}
	}
	manifest, _, err := readSetupManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := service.applySetup(context.Background(), home, manifestPath, manifest, "/opt/posse/bin/posse", "1", dirs, setupInspection{})
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(guard)
	if err != nil || !bytes.Contains(contents, []byte(`"/opt/posse/bin/posse"`)) || !bytes.Contains(contents, []byte(`"_guard"`)) || !containsValue(changed, "pi_guard") {
		t.Fatalf("pi guard not installed: changed=%v contents=%s err=%v", changed, contents, err)
	}
	manifest, _, err = readSetupManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range service.setupPlan(home, "/opt/posse/bin/posse", "1", dirs, manifest, true, setupInspection{}) {
		if row["step"] == "pi_guard" && row["action"] != "keep" {
			t.Fatalf("pi guard plan after install = %#v, want keep", row)
		}
	}
	if err := service.uninstallSetup(&axi.Context{Context: context.Background(), Out: &bytes.Buffer{}}, home, manifestPath, manifest, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(guard); !os.IsNotExist(err) {
		t.Fatalf("uninstall kept the pi guard: %v", err)
	}
	if _, err := os.Stat(filepath.Join(piDir, "extensions")); !os.IsNotExist(err) {
		t.Fatalf("uninstall kept the extensions directory it created: %v", err)
	}
}

func containsValue(values []any, want any) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
