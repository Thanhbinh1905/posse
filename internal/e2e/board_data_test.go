//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

// TestBoardDataReachesFullJSONAndSkillSnapshots starts a Worker whose skill
// ships an asset and queues a second Task behind it, then reads both through
// the CLI and the frozen launch snapshot.
func TestBoardDataReachesFullJSONAndSkillSnapshots(t *testing.T) {
	f := newWorkerTabsFixture(t)
	skill := filepath.Join(f.root, "board-skill")
	for name, content := range map[string]string{
		"SKILL.md":          "---\nname: board\ndescription: Board\n---\nCopy assets/board.html.\n",
		"assets/board.html": "<html>",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(skill, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skill, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config := "[lead]\nkind = \"claude\"\n\n[defaults]\nmerge_mode = \"local\"\nauto_teardown = \"never\"\nmax_workers = 1\n\n[profiles.deep]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"deep\"\n\n[skills]\nchange = [\"" + skill + "\"]\n"
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	f.start(t, "t1", "Holds the slot", "holds-slot")
	brief := filepath.Join(f.root, "queued.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Queued worker\ndone_when: the test tears it down\nafter: t1:landed\n---\nWait.\n\n## Intent\nUser's words: \"one view\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "start", "--brief", brief, "--name", "queued-worker")
	f.waitState(t, "t2", store.StateQueued)

	output := runPosse(t, f.binary, f.repo, f.leadEnv, "--full", "--json")
	var home struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(output), &home); err != nil {
		t.Fatalf("decode home: %v: %s", err, output)
	}
	rows := map[string]map[string]any{}
	for _, row := range home.Tasks {
		rows[row["id"].(string)] = row
	}
	queued := rows["t2"]
	if queued["profile"] != "deep" || queued["wait_reason"] != "waiting for t1:landed" || queued["intent"] != "User's words: \"one view\"" || queued["age"] == nil || queued["created_at"] == nil {
		t.Fatalf("queued row lacks board fields: %#v", queued)
	}
	if after, _ := queued["after"].([]any); len(after) != 1 || after[0] != "t1:landed" {
		t.Fatalf("queued row after = %#v", queued["after"])
	}
	if rows["t1"]["profile"] != "deep" {
		t.Fatalf("working row lacks its Profile: %#v", rows["t1"])
	}

	frozen, err := filepath.Glob(filepath.Join(f.home, "projects", "*", "launch-skills", "*", "skills", "board", "assets", "board.html"))
	if err != nil || len(frozen) != 1 {
		t.Fatalf("frozen board assets = %v, %v", frozen, err)
	}
	if data, err := os.ReadFile(frozen[0]); err != nil || string(data) != "<html>" {
		t.Fatalf("frozen asset = %q, %v", data, err)
	}
	if !strings.Contains(runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t1", "--full"), "board/assets/board.html") {
		t.Fatal("t1 plan does not record the frozen skill asset")
	}
}
