//go:build e2e && linux

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
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

type seededStatsTask struct {
	seq        int
	profile    string
	state      store.State
	ageDays    int
	doneAfter  time.Duration
	landed     bool
	relaunches int
	tokens     int // input tokens of the recorded Claude session; 0 records no session
}

// seedStatsHistory inserts finished Tasks with backdated creation times and
// transitions, plus a recorded Claude session for those with tokens.
func seedStatsHistory(t *testing.T, f *workerTabsFixture, claudeDir string, tasks []seededStatsTask) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, seed := range tasks {
		worktree := filepath.Join(f.root, "seeded", fmt.Sprintf("t%d", seed.seq))
		sessionID := fmt.Sprintf("session-t%d", seed.seq)
		task := store.Task{Seq: seed.seq, Type: "change", Title: fmt.Sprintf("Seeded %d", seed.seq), Profile: seed.profile, DispatchRule: "when=hard", MergeMode: "local", Branch: fmt.Sprintf("posse/t%d", seed.seq), BaseRef: "main", WorktreePath: worktree}
		if seed.tokens > 0 {
			task.AgentSession, task.AgentSessionKind = sessionID, "claude"
			directory := filepath.Join(claudeDir, "projects", strings.ReplaceAll(worktree, string(os.PathSeparator), "-"))
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			record := fmt.Sprintf(`{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":%d,"output_tokens":0}}}`+"\n", seed.tokens)
			if err := os.WriteFile(filepath.Join(directory, sessionID+".jsonl"), []byte(record), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		id, err := db.CreateTask(ctx, f.projectID, task)
		if err != nil {
			t.Fatal(err)
		}
		created := time.Now().AddDate(0, 0, -seed.ageDays)
		landedRef := ""
		if seed.landed {
			landedRef = "abc123"
		}
		if _, err := db.ExecContext(ctx, `UPDATE tasks SET state=?, created_at=?, landed_ref=?, launches=? WHERE id=?`, string(seed.state), created.UnixMilli(), landedRef, seed.relaunches+1, id); err != nil {
			t.Fatal(err)
		}
		insert := func(from, to store.State, note string, at time.Time) {
			if _, err := db.ExecContext(ctx, `INSERT INTO transitions(task_id,from_state,to_state,source,note,at) VALUES(?,?,?,?,?,?)`, id, string(from), string(to), "cli", note, at.UnixMilli()); err != nil {
				t.Fatal(err)
			}
		}
		insert(store.StateSpawning, store.StateWorking, "seeded start", created)
		if seed.doneAfter > 0 {
			insert(store.StateWorking, store.StateDone, "seeded done", created.Add(seed.doneAfter))
		}
		if seed.landed {
			insert(store.StateDone, store.StateMerged, "seeded merge", created.Add(seed.doneAfter+time.Minute))
		}
	}
}

func jsonProfileStats(t *testing.T, output string) map[string]map[string]any {
	t.Helper()
	var result struct {
		Profiles []map[string]any `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode stats: %v\n%s", err, output)
	}
	byName := map[string]map[string]any{}
	for _, row := range result.Profiles {
		byName[row["profile"].(string)] = row
	}
	return byName
}

// TestStatsReportProfileHistoryAndDispatchShowsItWithoutChangingRules seeds
// Task history and checks the stats, their use in dispatch, and that dispatch
// resolution and its existing fields do not depend on them.
func TestStatsReportProfileHistoryAndDispatchShowsItWithoutChangingRules(t *testing.T) {
	f := newWorkerTabsFixture(t)
	config := "[lead]\nkind = \"claude\"\n\n[defaults]\nmerge_mode = \"local\"\nauto_teardown = \"never\"\nmax_workers = 4\n\n[profiles.deep]\nkind = \"claude\"\n\n[profiles.fast]\nkind = \"claude\"\n\n[[dispatch]]\nwhen = \"hard\"\nuse = \"deep\"\n\n[[dispatch]]\nwhen = \"easy\"\nuse = \"fast\"\n"
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeDir := filepath.Join(f.root, "seeded-claude")
	env := setEnv(f.leadEnv, "CLAUDE_CONFIG_DIR", claudeDir)
	brief := filepath.Join(f.root, "stats.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Stats\ndone_when: dispatch shows stats\n---\nPreview.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	empty := jsonProfileStats(t, runPosse(t, f.binary, f.repo, env, "stats", "--json"))
	if len(empty) != 0 {
		t.Fatalf("stats without history invented rows: %#v", empty)
	}
	before, _ := runPosseAllowFailure(f, f.repo, env, "dispatch", "--brief", brief, "--json")

	seedStatsHistory(t, f, claudeDir, []seededStatsTask{
		{seq: 11, profile: "deep", state: store.StateMerged, ageDays: 5, doneAfter: 10 * time.Minute, landed: true, tokens: 1000},
		{seq: 12, profile: "deep", state: store.StateMerged, ageDays: 6, doneAfter: 20 * time.Minute, landed: true, tokens: 3000, relaunches: 1},
		{seq: 13, profile: "deep", state: store.StateFailed, ageDays: 7, relaunches: 1},
		{seq: 14, profile: "deep", state: store.StateMerged, ageDays: 45, doneAfter: time.Minute, landed: true, tokens: 900000},
		{seq: 15, profile: "fast", state: store.StateMerged, ageDays: 2, doneAfter: 5 * time.Minute, landed: true},
	})

	stats := jsonProfileStats(t, runPosse(t, f.binary, f.repo, env, "stats", "--json"))
	deep, fast := stats["deep"], stats["fast"]
	if deep == nil || fast == nil || len(stats) != 2 {
		t.Fatalf("stats profiles: %#v", stats)
	}
	for key, want := range map[string]float64{"tasks": 3, "finished": 3, "landed": 2, "landed_rate": 0.667, "median_done_seconds": 900, "tokens_per_landed": 2000, "relaunches": 2} {
		if got, ok := deep[key].(float64); !ok || got != want {
			t.Errorf("deep %s = %v, want %v (the 45 day old Task must be outside the window)", key, deep[key], want)
		}
	}
	for key, want := range map[string]float64{"tasks": 1, "finished": 1, "landed": 1, "landed_rate": 1, "median_done_seconds": 300, "relaunches": 0} {
		if got, ok := fast[key].(float64); !ok || got != want {
			t.Errorf("fast %s = %v, want %v", key, fast[key], want)
		}
	}
	if fast["tokens_per_landed"] != nil {
		t.Errorf("fast has no recorded usage, tokens_per_landed = %v", fast["tokens_per_landed"])
	}

	// An ambiguous dispatch lists the candidates with their stats.
	output, _ := runPosseAllowFailure(f, f.repo, env, "dispatch", "--brief", brief)
	for _, want := range []string{"profile_required", "stats deep: landed 67% (2/3)", "median to done 15m0s", "tokens per landed 2000", "relaunches 2", "stats fast: landed 100% (1/1)", "tokens per landed n/a"} {
		if !strings.Contains(output, want) {
			t.Errorf("ambiguous dispatch omits %q:\n%s", want, output)
		}
	}
	if !strings.Contains(before, "profile_required") || !strings.Contains(before, "stats deep: no Tasks in the last 30 days") {
		t.Errorf("dispatch without history must still list candidates, marking empty stats: %s", before)
	}
	if !strings.Contains(output, "profile_required") {
		t.Errorf("stats resolved an ambiguous rule: %s", output)
	}

	// A resolved dispatch gains profile_stats and keeps every existing field.
	resolved := runPosse(t, f.binary, f.repo, env, "dispatch", "--brief", brief, "--profile", "deep", "--json")
	var fields map[string]any
	if err := json.Unmarshal([]byte(resolved), &fields); err != nil {
		t.Fatalf("decode dispatch: %v\n%s", err, resolved)
	}
	for _, key := range []string{"task_type", "name", "profile", "dispatch_rule", "help", "model", "effort", "model_known"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("dispatch lost existing field %q: %s", key, resolved)
		}
	}
	if fields["profile"] != "deep" || fields["dispatch_rule"] != "--profile deep" {
		t.Errorf("stats changed the dispatch resolution: %s", resolved)
	}
	profileStats, ok := fields["profile_stats"].(map[string]any)
	if !ok || profileStats["profile"] != "deep" || profileStats["landed"] != float64(2) {
		t.Errorf("dispatch profile_stats: %s", resolved)
	}
}

func runPosseAllowFailure(f *workerTabsFixture, dir string, env []string, args ...string) (string, error) {
	command := exec.Command(f.binary, args...)
	command.Dir, command.Env = dir, env
	output, err := command.CombinedOutput()
	return string(output), err
}
