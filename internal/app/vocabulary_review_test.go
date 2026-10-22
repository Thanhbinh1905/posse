package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLegacyWatchRejectsNonAdapterInvocations(t *testing.T) {
	service := testService(t.TempDir(), herdr.NewFake())
	for _, args := range [][]string{
		{}, {"--json"}, {"--json", "--ack", "all"}, {"--poll-only"}, {"--json", "--poll-only"}, {"--json", "--repair"},
		{"--handoff", "--destination", "pi:old"}, {"--json", "--handoff", "--destination", "claude:old"},
		{"--json", "--handoff", "--destination", "pi:old", "--repair"},
		{"--json", "--receipt", "old", "--receipt-outcome", "accepted", "--ack", "all"},
		{"--json", "--handoff", "--destination", "pi:old", "--help"},
	} {
		code, output := runCLI(t, service, append([]string{"lookout"}, args...)...)
		if code != 2 || !strings.Contains(output, "was removed; use posse watch") {
			t.Fatalf("legacy User invocation %v escaped tombstone: %d %s", args, code, output)
		}
	}
}

func TestLegacyWatchRetainsCanonicalWorkerGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv(workerHomeEnv, home)
	service := testService(home, herdr.NewFake())
	code, output := runCLI(t, service, "lookout", "--json", "--handoff", "--destination", "pi:old")
	if code == 0 || !strings.Contains(output, "worker_forbidden") || !strings.Contains(output, "posse watch") {
		t.Fatalf("legacy protocol bypassed canonical Worker guard: %d %s", code, output)
	}
}

type pruneSnapshotFailureAdapter struct {
	*herdr.Fake
	removedPath string
	fail        bool
}

func (a *pruneSnapshotFailureAdapter) Snapshot(ctx context.Context) (herdr.Snapshot, error) {
	if _, err := os.Stat(a.removedPath); a.fail && os.IsNotExist(err) {
		return herdr.Snapshot{}, errors.New("Herdr unavailable after removal")
	}
	return a.Fake.Snapshot(ctx)
}

func TestPoolPruneReportsAppliedRowsWhenOrphanCleanupFails(t *testing.T) {
	for _, mode := range []string{"snapshot", "lingering-pane", "close-failure"} {
		t.Run(mode, func(t *testing.T) { testPartialPoolPrune(t, mode) })
	}
}

func testPartialPoolPrune(t *testing.T, mode string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(home, "scratch", "shop", "t99")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "payload"), []byte("old scratch"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"}}}
	expected := "Herdr unavailable after removal"
	if mode != "snapshot" {
		fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{PaneID: "w2:p1", Label: "posse:shop:t99", Agent: "claude"})
		fake.SnapshotValue.Agents = []herdr.Agent{{Name: "posse-shop-t99-1", PaneID: "w2:p1"}}
		expected = "orphan pane w2:p1 remains open"
	}
	if mode == "close-failure" {
		fake.Errors["pane.close"] = errors.New("orphan close failed")
		expected = "orphan close failed"
	}
	service := testService(home, &pruneSnapshotFailureAdapter{Fake: fake, removedPath: scratch, fail: mode == "snapshot"})
	t.Chdir(repo)
	code, output := runCLI(t, service, "pool", "prune", "--yes", "--json")
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("fixture did not reach partial deletion: %v; %s", err, output)
	}
	var result struct {
		Prune []struct {
			Item    string `json:"item"`
			Applied bool   `json:"applied"`
		} `json:"prune"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		ClosedPanes []string `json:"closed_panes"`
		OrphanPanes []any    `json:"orphan_panes"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("partial prune must return one structured result: %v %s", err, output)
	}
	if code == 0 || len(result.Prune) != 1 || result.Prune[0].Item != "t99" || !result.Prune[0].Applied || !strings.Contains(result.Error.Message, expected) {
		t.Fatalf("deleted items were hidden by orphan error: %d %s", code, output)
	}
	if mode != "snapshot" && len(result.OrphanPanes) != 1 {
		t.Fatalf("partial pane plan was hidden: %s", output)
	}
	if mode == "lingering-pane" && (len(result.ClosedPanes) != 1 || result.ClosedPanes[0] != "w2:p1") {
		t.Fatalf("partial pane effects were hidden: %s", output)
	}
}

func TestHistoricalOrphanSnapshotReusesOrphanRef(t *testing.T) {
	for _, uncommitted := range []bool{false, true} {
		name := "committed"
		if uncommitted {
			name = "uncommitted"
		}
		t.Run(name, func(t *testing.T) {
			f := newPRMergeFixture(t, "pr", store.StateDone)
			defer f.db.Close()
			ctx := context.Background()
			if uncommitted {
				if err := os.WriteFile(filepath.Join(f.worktree, "follow-up.txt"), []byte("saved follow-up\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// Model 0.14 crashing after the snapshot ref write but before RaiseDecision.
			if err := persistOrphanSnapshot(ctx, f.db, f.project, f.task, ""); err != nil {
				t.Fatal(err)
			}
			legacy := f.task.Branch + "-leftover"
			gitTest(t, f.repo, "branch", "-m", f.task.Branch+"-orphan", legacy)
			if _, err := f.db.ExecContext(ctx, `DELETE FROM decisions WHERE task_id=?`, f.task.ID); err != nil {
				t.Fatal(err)
			}
			before := gitTest(t, f.repo, "rev-parse", "refs/heads/"+legacy)
			for i := 0; i < 2; i++ {
				var err error
				if uncommitted {
					err = persistOrphanSnapshot(ctx, f.db, f.project, f.task, "")
				} else {
					err = snapshotUnmergedMemberWork(ctx, f.db, f.project, f.task, "")
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := gitOutput(ctx, f.repo, "rev-parse", "--verify", "refs/heads/"+f.task.Branch+"-orphan"); err == nil {
				t.Fatal("retry created a duplicate -orphan branch beside historical -leftover")
			}
			if after := gitTest(t, f.repo, "rev-parse", "refs/heads/"+legacy); after != before {
				t.Fatal("retry changed saved historical work")
			}
			decisions, err := f.db.Decisions(ctx, f.project.ID, true)
			if err != nil || len(decisions) != 1 || decisions[0].Origin != "orphan:"+legacy {
				t.Fatalf("retry did not reuse one historical Orphan: %#v %v", decisions, err)
			}
			if err := os.WriteFile(filepath.Join(f.worktree, "another.txt"), []byte("different work"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := persistOrphanSnapshot(ctx, f.db, f.project, f.task, ""); err == nil || !strings.Contains(err.Error(), "different content") {
				t.Fatalf("retry overwrote historical work instead of failing closed: %v", err)
			}
			if after := gitTest(t, f.repo, "rev-parse", "refs/heads/"+legacy); after != before {
				t.Fatal("conflicting retry changed the historical snapshot")
			}
		})
	}
}
