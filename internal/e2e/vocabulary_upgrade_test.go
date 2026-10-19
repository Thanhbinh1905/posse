//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func Test014ProjectVocabularyUpgradePreservesTasksSandboxesAndOpenPR(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	ctx := context.Background()
	legacyBase := filepath.Join(f.home, "remuda", f.project.Name)
	legacyPath := filepath.Join(legacyBase, "mount-1")
	if err := os.MkdirAll(legacyBase, 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, f.repo, "worktree", "add", "-b", "posse/t1", legacyPath, "main")
	if err := os.WriteFile(filepath.Join(legacyPath, "upgrade.txt"), []byte("existing 0.14 change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, legacyPath, "add", "upgrade.txt")
	gitTest(t, f.env, legacyPath, "commit", "-m", "existing change")
	gitTest(t, f.env, legacyPath, "push", "origin", "HEAD:refs/heads/posse/t1")
	head := strings.TrimSpace(gitTest(t, f.env, legacyPath, "rev-parse", "HEAD"))
	title := `Preserve User words: "Rider ship scout"`
	id, err := f.db.CreateTask(ctx, f.project.ID, store.Task{Seq: 1, Type: "ship", Title: title, MergeMode: "pr", Branch: "posse/t1", BaseRef: "main", TargetBranch: "main", WorktreePath: legacyPath})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE tasks SET state='done',pr_url='https://github.com/acme/shop/pull/17',gated_sha=? WHERE id=?`,
	} {
		if _, err := f.db.ExecContext(ctx, statement, head, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, f.project.ID, legacyPath, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, id, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateNotice(ctx, store.Notice{ProjectID: f.project.ID, TaskID: id, Kind: "rider_resumed", Summary: "Historical notice"}); err != nil {
		t.Fatal(err)
	}
	// 0.14 has the same published schema. Remove only this release's data
	// migration receipt, then seed its former values, exactly as an old home.
	if _, err := f.db.ExecContext(ctx, `DELETE FROM goose_db_version WHERE version_id=55`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `DELETE FROM posse_migration_checksums WHERE version=55`); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(f.home, "projects", f.project.Name, "config.toml")
	configBytes, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		configBytes = []byte("[defaults]\nmerge_mode = 'pr'\nauto_teardown = 'never'\n[pool]\nclean = 'warm'\n")
	} else if err != nil {
		t.Fatal(err)
	}
	legacyConfig := strings.NewReplacer("# posse vocabulary: 1\n", "", "merge_mode", "landing_mode", "auto_teardown", "auto_unsaddle", "[pool]", "[remuda]", "\"merged\"", "\"landed\"").Replace(string(configBytes))
	if err := os.WriteFile(configPath, []byte(legacyConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(configPath + ".vocabulary"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	f.writeGraphQL(t, "pr1", "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", head)
	output := runPosse(t, f.binary, f.repo, f.leadEnv, "list", "--all")
	if !strings.Contains(output, "shop/t1") {
		t.Fatalf("upgraded list lacks the existing Task: %s", output)
	}
	task := f.mustTask(t, "t1")
	if task.ID != id || task.Type != "change" || task.PRURL != "https://github.com/acme/shop/pull/17" || task.WorktreePath != legacyPath || task.Title != title {
		t.Fatalf("upgrade lost Task/PR identity or User text: %#v", task)
	}
	if got := runPosse(t, f.binary, f.repo, f.leadEnv, "pool"); !strings.Contains(got, "sandbox-1") || !strings.Contains(got, legacyPath) {
		t.Fatalf("legacy checkout not usable as a Sandbox: %s", got)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, notice := range notices {
		if notice.Kind == "worker_resumed" {
			found = true
		}
	}
	if !found {
		t.Fatal("legacy Notice kind was not migrated")
	}
	if output := runPosse(t, f.binary, f.repo, f.leadEnv, "merge", "t1"); !strings.Contains(output, "merging") {
		t.Fatalf("existing open PR cannot prepare Merge: %s", output)
	}
	for _, pair := range [][2]string{{"ride", "start"}, {"holler", "signal"}, {"unsaddle", "teardown"}, {"lookout", "watch"}, {"remuda", "pool"}, {"roster", "list"}, {"land", "merge"}, {"sweep", "pool prune"}, {"playbook", "preferences"}} {
		command := exec.Command(f.binary, pair[0])
		command.Dir, command.Env = f.repo, f.leadEnv
		out, err := command.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "was removed; use posse "+pair[1]) {
			t.Fatalf("legacy command %s: %v %s", pair[0], err, out)
		}
	}
}
