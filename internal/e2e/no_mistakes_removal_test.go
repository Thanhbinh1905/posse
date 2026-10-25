//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// noMistakesRemovalFixture is a plain User shell, outside any Herdr pane, with
// a freshly built posse installed outside /tmp and one registered Project.
type noMistakesRemovalFixture struct {
	binary, home, repo string
	env                []string
}

func newNoMistakesRemovalFixture(t *testing.T) noMistakesRemovalFixture {
	t.Helper()
	root := newFixtureRootAt(t, "/var/tmp", fixturePrefix("no-mistakes-removal-"))
	binary := filepath.Join(root, "installed", "posse")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = moduleRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse: %v\n%s", err, output)
	}
	home := filepath.Join(root, "posse")
	env := []string{}
	for _, entry := range isolatedE2EEnv(t, root) {
		// Model a User shell even when the suite itself runs inside a Worker.
		if !strings.HasPrefix(entry, "POSSE_WORKER_HOME=") && !strings.HasPrefix(entry, "POSSE_TASK_") {
			env = append(env, entry)
		}
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(herdrBinary, filepath.Join(binDir, "herdr")); err != nil {
		t.Fatal(err)
	}
	env = setEnv(env, "POSSE_HOME", home)
	env = setEnv(env, "PATH", binDir+":/usr/bin:/bin")
	// A successful update runs setup, which needs the isolated Herdr config.
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"home", "claude", "codex"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	repo := filepath.Join(root, "shop")
	initRepository(t, repo, filepath.Join(root, "origin.git"), env)
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.CreateProject(context.Background(), "shop", repo, "main"); err != nil {
		t.Fatal(err)
	}
	return noMistakesRemovalFixture{binary: binary, home: home, repo: repo, env: env}
}

// createLegacyNoMistakesTask records a Task admitted by a release that still
// supported no-mistakes delivery.
func (f noMistakesRemovalFixture) createLegacyNoMistakesTask(t *testing.T, state store.State) {
	t.Helper()
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.ProjectByName(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Admitted under no-mistakes", State: store.StateSpawning, MergeMode: "no-mistakes", Branch: "posse/t1", BaseRef: "main", AutonomyMerge: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET state=? WHERE id=?`, string(state), id); err != nil {
		t.Fatal(err)
	}
	if err := db.PersistTask(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func (f noMistakesRemovalFixture) run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	return runReleaseCLI(t, f.binary, f.repo, f.env, args...)
}

// The removal error names the file, the key and the replacement V14 already
// recommended; applying the printed command repairs config.
func TestNoMistakesConfigNamesFileKeyAndReplacement(t *testing.T) {
	f := newNoMistakesRemovalFixture(t)
	path := filepath.Join(f.home, "config.toml")
	for _, tc := range []struct {
		config, key, replacement string
		lighter                  []string
	}{
		{"[defaults]\nmerge_mode = 'no-mistakes'\n", "defaults.merge_mode", "pr", nil},
		{"[defaults]\nreview = 'no-mistakes'\n", "defaults.review", "full-pipeline", []string{"quick", "none"}},
		{"[repositories.api]\nreview = 'no-mistakes'\n", "repositories.api.review", "full-pipeline", []string{"quick", "none"}},
	} {
		t.Run(tc.key, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.config), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"config", "show", "--effective"}, {"list"}} {
				code, output := f.run(t, args...)
				if code != 1 || !strings.Contains(output, "config_migration_required") ||
					!strings.Contains(output, path+": "+tc.key+": no-mistakes was removed; use "+tc.replacement) ||
					!strings.Contains(output, "posse config set "+tc.key+" "+tc.replacement) {
					t.Fatalf("posse %s did not name the file, key and replacement: %d %s", strings.Join(args, " "), code, output)
				}
				for _, lighter := range tc.lighter {
					if !strings.Contains(output, "posse config set "+tc.key+" "+lighter) {
						t.Fatalf("posse %s omitted the lighter %s choice: %s", strings.Join(args, " "), lighter, output)
					}
				}
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != tc.config {
				t.Fatalf("refusal changed config: %q %v", data, err)
			}
			if code, output := f.run(t, "config", "set", tc.key, "no-mistakes"); code != 1 || !strings.Contains(output, "config_migration_required") {
				t.Fatalf("config set accepted no-mistakes: %d %s", code, output)
			}
			runPosse(t, f.binary, f.repo, f.env, "config", "set", tc.key, tc.replacement)
			if code, output := f.run(t, "config", "show", "--effective"); code != 0 {
				t.Fatalf("printed replacement did not repair config: %d %s", code, output)
			}
		})
	}
}

// The downloaded release refuses its own installation while one live Task has
// a frozen no-mistakes plan, even with --force; finished history does not block.
func TestUpdateRefusesWhileALiveTaskHasANoMistakesPlan(t *testing.T) {
	f := newNoMistakesRemovalFixture(t)
	f.createLegacyNoMistakesTask(t, store.StateWorking)
	release := makeUpdateTestRelease(t, f.binary, "v99.0.0")
	proxyURL, certPath := startUpdateReleaseProxy(t, release)
	for key, value := range map[string]string{"HTTPS_PROXY": proxyURL, "https_proxy": proxyURL, "NO_PROXY": "localhost,127.0.0.1,::1", "no_proxy": "localhost,127.0.0.1,::1", "SSL_CERT_FILE": certPath} {
		f.env = setEnv(f.env, key, value)
	}
	installed, err := os.ReadFile(f.binary)
	if err != nil {
		t.Fatal(err)
	}
	database, err := os.ReadFile(filepath.Join(f.home, "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	backups := func() string {
		entries, _ := filepath.Glob(filepath.Join(f.home, "backup", "*"))
		return strings.Join(entries, "\n")
	}
	backupsBefore := backups()
	for _, args := range [][]string{{"update", "--version", release.tag}, {"update", "--version", release.tag, "--force"}} {
		code, output := f.run(t, args...)
		if code != 1 || !strings.Contains(output, "no_mistakes_tasks_live") || !strings.Contains(output, "shop t1") || !strings.Contains(output, "posse teardown <task> --discard") {
			t.Fatalf("posse %s was not refused for the live no-mistakes Task: %d %s", strings.Join(args, " "), code, output)
		}
		if after, err := os.ReadFile(f.binary); err != nil || !bytes.Equal(after, installed) {
			t.Fatalf("refused update changed the installed binary: %v", err)
		}
		if after, err := os.ReadFile(filepath.Join(f.home, "posse.db")); err != nil || !bytes.Equal(after, database) {
			t.Fatalf("refused update changed the database: %v", err)
		}
		if after := backups(); after != backupsBefore {
			t.Fatalf("refused update wrote a database backup: %s", after)
		}
	}

	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE tasks SET state='merged'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if code, output := f.run(t, "update", "--version", release.tag); code != 0 || !strings.Contains(output, "installed: "+release.tag) {
		t.Fatalf("merged no-mistakes history blocked the update: %d %s", code, output)
	}
}

// Receipts recorded by earlier releases stay visible in show, including after
// the database is rebuilt from Task snapshots.
func TestStoredNoMistakesReceiptsSurviveSnapshotRebuild(t *testing.T) {
	f := newNoMistakesRemovalFixture(t)
	f.createLegacyNoMistakesTask(t, store.StateMerged)
	receiptSHA := strings.Repeat("d", 64)
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO review_receipts(task_id,repository,run_id,head_sha,review_status,reviewer_profile,reviewer_model,report_sha256,receipt_sha256,observed_at) SELECT id,'github.com/acme/shop','01RUN',?,'passed','nm-reviewer','model-r',?,?,10 FROM tasks WHERE seq=1`,
		strings.Repeat("a", 40), strings.Repeat("ab", 32), receiptSHA); err != nil {
		t.Fatal(err)
	}
	var taskID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM tasks WHERE seq=1`).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.PersistTask(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	requireReceipt := func(stage string) {
		t.Helper()
		code, output := f.run(t, "show", "t1", "--json")
		if code != 0 || !strings.Contains(output, "review_receipts") || !strings.Contains(output, receiptSHA) || !strings.Contains(output, "nm-reviewer") {
			t.Fatalf("%s: show omits the stored receipt: %d %s", stage, code, output)
		}
	}
	requireReceipt("stored database")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(f.home, "posse.db"+suffix)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if code, output := f.run(t, "recover", "--rebuild"); code != 0 || !strings.Contains(output, "rebuilt_tasks") {
		t.Fatalf("rebuild from snapshots: %d %s", code, output)
	}
	requireReceipt("rebuilt database")
}
