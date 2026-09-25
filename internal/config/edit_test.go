package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSetFilePreservesUntouchedBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "# user header\n[defaults]\n# workers comment\nmax_workers = 4 # keep inline comment\nstall_after = \"20m\" # keep this exact\n\n[identity.lead]\nname = \"Guide\"\n"
	if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := SetFile(path, "defaults.max_workers", "8", false); err != nil {
		t.Fatal(err)
	}
	want := "# user header\n[defaults]\n# workers comment\nmax_workers = 8 # keep inline comment\nstall_after = \"20m\" # keep this exact\n\n[identity.lead]\nname = \"Guide\"\n"
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("file bytes changed beyond the selected value:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("file mode = %o, want 640", info.Mode().Perm())
	}
}

func TestSetAndUnsetDynamicSchemaKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("# retained\n[lead]\nkind = \"claude\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SetFile(path, "lead.profiles.claude", `"reviewer"`, false); err != nil {
		t.Fatal(err)
	}
	if _, err := SetFile(path, "kinds.custom.resume_args", ` ["resume", "{session}"] `, false); err != nil {
		t.Fatal(err)
	}
	values, _, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lead := values["lead"].(map[string]any)
	if profiles := lead["profiles"].(map[string]any); profiles["claude"] != "reviewer" {
		t.Fatalf("Lead profile mapping = %#v", profiles)
	}
	kinds := values["kinds"].(map[string]any)
	args := kinds["custom"].(map[string]any)["resume_args"]
	if !reflect.DeepEqual(args, []any{"resume", "{session}"}) {
		t.Fatalf("resume args = %#v", args)
	}
	if err := UnsetFile(path, "lead.profiles.claude", false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "# retained\n[lead]\nkind = \"claude\"\n[lead.profiles]\n[kinds.custom]\nresume_args = [\"resume\", \"{session}\"]\n" {
		t.Fatalf("unset changed unrelated content:\n%s", got)
	}
}

func TestSetFileRejectsTOMLTableInjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "[defaults]\nmax_workers = 4\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	payload := "8\n[remuda]\nsetup = \"enabled\""
	if _, err := SetFile(path, "defaults.max_workers", payload, false); err == nil {
		t.Fatal("SetFile accepted an extra TOML table in the value")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("rejected value changed the config file:\n%s", got)
	}
}

func TestSetDispatchArrayTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "# retain\n[defaults]\nmax_workers = 4\n\n[[dispatch]]\ntype = \"scout\"\nuse = \"old\"\n\n[identity.lead]\nname = \"Guide\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SetFile(path, "dispatch", `[{type = "ship", use = "new"}]`, true); err != nil {
		t.Fatal(err)
	}
	values, _, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dispatch, ok := values["dispatch"].([]map[string]any)
	if !ok {
		if raw, arrayOK := values["dispatch"].([]any); arrayOK {
			for _, item := range raw {
				if table, tableOK := item.(map[string]any); tableOK {
					dispatch = append(dispatch, table)
				}
			}
		}
	}
	if len(dispatch) != 1 || dispatch[0]["type"] != "ship" || dispatch[0]["use"] != "new" {
		t.Fatalf("dispatch = %#v", dispatch)
	}
	if err := UnsetFile(path, "dispatch", true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "# retain\n[defaults]\nmax_workers = 4\n\n[identity.lead]\nname = \"Guide\"\n\n" {
		t.Fatalf("dispatch unset changed unrelated sections:\n%s", got)
	}
}

func TestSetDispatchRejectsFieldsItCannotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	for _, literal := range []string{`[{when = {type = "ship"}, use = "deep"}]`, `[{type = "ship", use = "deep", model = "x"}]`} {
		if _, err := SetFile(path, "dispatch", literal, false); err == nil {
			t.Errorf("SetFile accepted dispatch rule %s", literal)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("rejected dispatch rules wrote the config file: %v", err)
	}
}

func TestSchemaDrivesUserOnlyAndNewKeys(t *testing.T) {
	for _, key := range []string{"defaults.gate", "remuda.setup", "autonomy.review", "autonomy.land", "autonomy.yolo"} {
		if !IsUserOnly(key) {
			t.Errorf("%s is not User-only", key)
		}
	}
	for _, key := range []string{"lead.kind", "lead.profiles.claude", "defaults.idle_after", "kinds.codex.resume_args"} {
		if _, ok := Lookup(key); !ok {
			t.Errorf("schema does not recognize %s", key)
		}
	}
	if err := ValidateSetting("defaults.idle_after", "0s", false); err == nil {
		t.Fatal("accepted a nonpositive idle duration")
	}
}
