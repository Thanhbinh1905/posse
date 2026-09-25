package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallHookEditsResolvedWritableSymlinkTarget(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "settings.json")
	linkTwo := filepath.Join(directory, "managed", "settings.json")
	linkOne := filepath.Join(directory, "settings-link.json")
	original := []byte("{\n  \"hooks\": {},\n  \"foreign\": \"preserve\"\n}\n")
	if err := os.MkdirAll(filepath.Dir(linkTwo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, original, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, linkTwo); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linkTwo, linkOne); err != nil {
		t.Fatal(err)
	}

	record, changed, err := installHook(linkOne, sessionStartHook, "posse _context", setupHookRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("installSessionStartHook reported no change")
	}
	if record.Path != target {
		t.Fatalf("manifest path = %q, want resolved target %q", record.Path, target)
	}
	for link, wantTarget := range map[string]string{linkOne: linkTwo, linkTwo: target} {
		if got, err := os.Readlink(link); err != nil || got != wantTarget {
			t.Fatalf("symlink %s = %q, %v; want %q", link, got, err, wantTarget)
		}
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(data, original) || !bytes.Contains(data, []byte("posse _context")) {
		t.Fatalf("resolved target was not edited: %s", data)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("target mode = %v, %v; want 0640", info, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".posse-") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("atomic temp file was not removed from target directory: %s", entry.Name())
		}
	}
	if _, err := removeHook(record); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, original) {
		t.Fatalf("install and uninstall did not preserve target bytes: got %q, want %q", data, original)
	}
}

func TestInstallHookRefusesReadOnlySymlinkTarget(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "settings.json")
	linkTwo := filepath.Join(directory, "managed", "settings.json")
	linkOne := filepath.Join(directory, "settings-link.json")
	original := []byte("{\"hooks\":{}}\n")
	if err := os.MkdirAll(filepath.Dir(linkTwo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, original, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, linkTwo); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linkTwo, linkOne); err != nil {
		t.Fatal(err)
	}

	_, _, err := installHook(linkOne, sessionStartHook, "posse _context", setupHookRecord{})
	if err == nil {
		t.Fatal("installSessionStartHook edited a read-only symlink target")
	}
	var blocked *setupHookSymlinkError
	if !errors.As(err, &blocked) || blocked.Path != target || blocked.Snippet != hookSnippet(sessionStartHook, "posse _context") {
		t.Fatalf("read-only symlink failure = %#v, %v", blocked, err)
	}
	for _, link := range []string{linkOne, linkTwo} {
		if _, err := os.Readlink(link); err != nil {
			t.Fatalf("symlink %s was not preserved: %v", link, err)
		}
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != string(original) {
		t.Fatalf("read-only target changed: %q, %v", data, err)
	}
}

func TestSessionStartMergePreservesForeignJSONBytes(t *testing.T) {
	fixtures := map[string]string{
		"claude": "{\n  \"foreign\": \"a && b > c\",\n  \"large\": 9007199254740993123456789,\n  \"ratio\": 1.50,\n  \"hooks\": {\n    \"SessionStart\": [\n      {\"matcher\": \"foreign\", \"hooks\": [{\"type\": \"command\", \"command\": \"foreign-claude\"}]}\n    ],\n    \"PreToolUse\": [{\"command\": \"foreign-tool\"}]\n  }\n}\n",
		"codex":  "{\n  \"foreign\": \"a && b > c\",\n  \"large\": 9007199254740993123456789,\n  \"ratio\": 1.50,\n  \"hooks\": {\n    \"SessionStart\": [\n      {\"hooks\": [{\"type\": \"command\", \"command\": \"foreign-codex\"}]}\n    ],\n    \"Stop\": [{\"command\": \"foreign-stop\"}]\n  }\n}\n",
	}
	for name, fixture := range fixtures {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hooks.json")
			if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := installHook(path, sessionStartHook, "posse _context", setupHookRecord{}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, exact := range [][]byte{[]byte(`"foreign": "a && b > c"`), []byte(`"large": 9007199254740993123456789`), []byte(`"ratio": 1.50`)} {
				if !bytes.Contains(data, exact) {
					t.Errorf("setup changed foreign bytes %q in %s", exact, data)
				}
			}
		})
	}
}

func TestSessionStartSetupUninstallRestoresOriginalBytes(t *testing.T) {
	fixtures := []string{
		"{\n  \"foreign\": \"keep && >\",\n  \"hooks\": {\n    \"SessionStart\": [\n      {\"matcher\": \"foreign\", \"hooks\": [{\"command\": \"foreign\"}]}\n    ],\n    \"PreToolUse\": []\n  }\n}\n",
		"{\n  \"hooks\": {\n    \"SessionStart\": [\n      {\"matcher\": \"foreign-empty\", \"hooks\": []}\n    ]\n  },\n  \"ratio\": 1.50\n}\n",
		"{\n  \"hooks\": {},\n  \"foreign\": 9007199254740993123456789\n}\n",
		"{\n  \"foreign\": \"untouched\"\n}\n",
	}
	for index, fixture := range fixtures {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			original := []byte(fixture)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			record, _, err := installHook(path, sessionStartHook, "posse _context", setupHookRecord{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := removeHook(record); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, original) {
				t.Fatalf("setup/uninstall changed foreign JSON bytes\n got: %s\nwant: %s", got, original)
			}
		})
	}
}

func TestSessionStartCommandIsStableAcrossSetupVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("{\"hooks\":{\"SessionStart\":[]}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := hookCommand("/opt/posse/bin/posse", sessionStartHook)
	record, changed, err := installHook(path, sessionStartHook, command, setupHookRecord{})
	if err != nil || !changed {
		t.Fatalf("initial hook install: changed=%v err=%v", changed, err)
	}
	record.Version = "1.0.0"
	if err := os.WriteFile(path, []byte("{\"hooks\":{\"SessionStart\":[]}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := installHook(path, sessionStartHook, command, setupHookRecord{}); err != nil || !changed {
		t.Fatalf("fresh version hook install: changed=%v err=%v", changed, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("POSSE_SETUP_VERSION=")) || !hookCommandExists(path, sessionStartHook, command) {
		t.Fatalf("hook command is version-stamped or missing: %s", data)
	}
}
