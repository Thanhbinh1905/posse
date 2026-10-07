//go:build e2e

package e2e

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestT229RecreatedDatabaseProjectIDCollision(t *testing.T) {
	f := newPRLifecycleFixture(t)
	ctx := context.Background()
	t.Cleanup(func() {
		if f.db != nil {
			_ = f.db.Close()
		}
	})
	originalTaskID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{Seq: 1, Type: "scout", Title: "Original shop Report", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.home, "posse.db")); err != nil {
		t.Fatal(err)
	}
	f.db, err = store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	foreignRoot := filepath.Join(f.root, "other-repo")
	if err := os.MkdirAll(foreignRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	other, err := f.db.CreateProject(ctx, "other", foreignRoot, "main")
	if err != nil {
		t.Fatal(err)
	}
	if other.ID != f.project.ID {
		t.Fatalf("fixture IDs did not collide: %d/%d", other.ID, f.project.ID)
	}
	// Exercise matching against the current database row, not just the two
	// Project snapshots produced by the fresh registration.
	if err := os.Remove(f.db.ProjectSnapshotPath(other.Name)); err != nil {
		t.Fatal(err)
	}
	snapshotPath := f.db.TaskSnapshotPath(f.project.Name, 1)
	snapshotBefore, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(f.binary, "recover", "--rebuild")
	command.Dir, command.Env = f.repo, f.env
	output, rebuildErr := command.CombinedOutput()
	if rebuildErr == nil {
		t.Fatalf("rebuild accepted conflicting Project identity and reassigned Task %d\n%s", originalTaskID, output)
	}
	if !strings.Contains(string(output), "Project identity conflict") || !strings.Contains(string(output), "shop") || !strings.Contains(string(output), "other") {
		t.Fatalf("rebuild did not report the Project identity conflict clearly: %v\n%s", rebuildErr, output)
	}
	projects, err := f.db.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].ID != other.ID || projects[0].Name != other.Name || projects[0].Root != other.Root {
		t.Fatalf("failed rebuild changed the current Project: %+v", projects)
	}
	tasks, err := f.db.Tasks(ctx, other.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("failed rebuild reassigned the old Task to the new Project: %+v", tasks)
	}
	snapshotAfter, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshotBefore) != string(snapshotAfter) {
		t.Fatal("identity conflict changed the original Task snapshot")
	}
}

func TestT229LegacyWorkspaceRebuild(t *testing.T) {
	f := newPRLifecycleFixture(t)
	ctx := context.Background()
	t.Cleanup(func() {
		if f.db != nil {
			_ = f.db.Close()
		}
	})
	members := []store.ProjectRepo{{Name: "api", Path: "api", DefaultBranch: "main", Status: store.RepoActive, OriginHost: "github.com"}}
	project, err := f.db.CreateWorkspaceProject(ctx, "stack", filepath.Join(f.root, "stack"), members)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "scout", Title: "Old Members", LandingMode: "local"}); err != nil {
		t.Fatal(err)
	}
	members = append(members, store.ProjectRepo{Name: "web", Path: "web", DefaultBranch: "main", Status: store.RepoActive, OriginHost: "git.example.com"})
	if err := f.db.SyncProjectRepos(ctx, project.ID, members); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateTask(ctx, project.ID, store.Task{Seq: 2, Type: "scout", Title: "New Members", LandingMode: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.home, "projects", "stack", "project.toml")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.home, "posse.db")); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(f.binary, "recover", "--rebuild")
	command.Dir, command.Env = f.repo, f.env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("legacy Workspace rebuild: %v\n%s", err, output)
	}
	f.db, err = store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.db.ProjectRepos(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("rebuild discarded the newer Member snapshot: %+v", got)
	}
	foundAPI, foundWeb := false, false
	for _, member := range got {
		if member.Name == "api" && member.Path == "api" && member.OriginHost == "github.com" {
			foundAPI = true
		}
		if member.Name == "web" && member.Path == "web" && member.OriginHost == "git.example.com" {
			foundWeb = true
		}
	}
	if !foundAPI || !foundWeb {
		t.Fatalf("rebuild restored the wrong Workspace Members: %+v", got)
	}
}

func TestT229SignalSnapshotFailure(t *testing.T) {
	f := newPRLifecycleFixture(t)
	ctx := context.Background()
	t.Cleanup(func() {
		if f.db != nil {
			_ = f.db.Close()
		}
	})
	brief := filepath.Join(f.root, "ship.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR lifecycle change\ndone_when: committed\n---\nFixture.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "ride", "--brief", brief, "--name", "pr-lifecycle-change")
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	workerEnv := setEnv(f.leadEnv, "HERDR_PANE_ID", task.PaneID)
	workerEnv = setEnv(workerEnv, "HERDR_WORKSPACE_ID", task.HerdrWorkspaceID)
	taskDir := filepath.Dir(f.db.TaskSnapshotPath(f.project.Name, task.Seq))
	if err := os.Chmod(taskDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(taskDir, 0o700)
	command := exec.Command(f.binary, "holler", "failed", "Snapshot write failure fixture")
	command.Dir, command.Env = task.WorktreePath, workerEnv
	output, signalErr := command.CombinedOutput()
	after, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	signals, err := f.db.TaskSignals(ctx, task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != store.StateFailed || len(signals) != 1 {
		t.Fatalf("fixture did not commit the failed Signal: state=%s signals=%+v", after.State, signals)
	}
	if signalErr != nil {
		t.Fatalf("committed Signal was rejected: %v\n%s", signalErr, output)
	}
	if !strings.Contains(string(output), "recovery snapshot refresh failed") {
		t.Fatalf("snapshot failure was not reported separately: %s", output)
	}
}

func TestT233MovedProjectRebuild(t *testing.T) {
	for _, mode := range []string{"existing", "deleted", "deleted-with-stale-sidecars", "deleted-with-valid-sidecars"} {
		t.Run(mode, func(t *testing.T) {
			f := newPRLifecycleFixture(t)
			ctx := context.Background()
			var sidecarSuffixes []string
			validSidecars := map[string][]byte{}
			t.Cleanup(func() {
				if f.db != nil {
					_ = f.db.Close()
				}
			})
			taskID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{Seq: 1, Type: "scout", Title: "Before Project move", LandingMode: "local"})
			if err != nil {
				t.Fatal(err)
			}
			moved := filepath.Join(f.root, "moved-repo")
			if err := os.Rename(f.repo, moved); err != nil {
				t.Fatal(err)
			}
			move := exec.Command(f.binary, "project", "move", "shop", moved)
			move.Dir, move.Env = moved, f.leadEnv
			output, err := move.CombinedOutput()
			if err != nil {
				t.Fatalf("project move: %v\n%s", err, output)
			}
			movedProject, err := f.db.ProjectByID(ctx, f.project.ID)
			if err != nil {
				t.Fatal(err)
			}
			taskSnapshot, err := os.ReadFile(f.db.TaskSnapshotPath(f.project.Name, 1))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(taskSnapshot), `root = "`+moved+`"`) || !strings.Contains(string(taskSnapshot), `uuid = "`+movedProject.UUID+`"`) {
				t.Fatalf("Project move did not refresh Task snapshot identity: %s", taskSnapshot)
			}
			if mode != "existing" {
				database := filepath.Join(f.home, "posse.db")
				if mode == "deleted-with-valid-sidecars" {
					for _, suffix := range []string{"-wal", "-shm"} {
						data, err := os.ReadFile(database + suffix)
						if err != nil {
							t.Fatalf("read live SQLite sidecar %s: %v", suffix, err)
						}
						validSidecars[suffix] = data
						sidecarSuffixes = append(sidecarSuffixes, suffix)
					}
				}
				if err := f.db.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(database); err != nil {
					t.Fatal(err)
				}
				if mode == "deleted-with-valid-sidecars" {
					for suffix, data := range validSidecars {
						if _, err := os.Stat(database + suffix); errors.Is(err, os.ErrNotExist) {
							if err := os.WriteFile(database+suffix, data, 0o600); err != nil {
								t.Fatal(err)
							}
						} else if err != nil {
							t.Fatal(err)
						}
					}
				}
				if mode == "deleted-with-stale-sidecars" {
					for _, suffix := range []string{"-wal", "-shm"} {
						if err := os.WriteFile(database+suffix, []byte("orphaned "+suffix+" from deleted database"), 0o600); err != nil {
							t.Fatal(err)
						}
						sidecarSuffixes = append(sidecarSuffixes, suffix)
					}
				}
			}
			command := exec.Command(f.binary, "recover", "--rebuild")
			command.Dir, command.Env = moved, f.env
			output, err = command.CombinedOutput()
			if err != nil {
				t.Fatalf("rebuild after Project move (%s): %v\n%s", mode, err, output)
			}
			if len(sidecarSuffixes) > 0 && !strings.Contains(string(output), "orphaned_sidecars") {
				t.Fatalf("rebuild did not report preserved orphaned sidecars: %s", output)
			}
			for _, suffix := range sidecarSuffixes {
				paths, err := filepath.Glob(filepath.Join(f.home, "posse.db.orphaned-*-"+strings.TrimPrefix(suffix, "-")))
				if err != nil || len(paths) != 1 {
					t.Fatalf("orphaned %s sidecar paths = %v, %v", suffix, paths, err)
				}
			}
			if mode != "existing" {
				f.db, err = store.Open(f.home)
				if err != nil {
					t.Fatal(err)
				}
			}
			project, err := f.db.ProjectByID(ctx, f.project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.TaskByID(ctx, project.ID, taskID); err != nil {
				t.Fatalf("Task was not attached to moved Project: %v", err)
			}
			if project.Root != moved {
				t.Fatalf("moved root=%q, want %q", project.Root, moved)
			}
		})
	}
}

func TestT229RebuildPreservesStoppedProject(t *testing.T) {
	for _, mode := range []string{"existing", "deleted", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			f := newPRLifecycleFixture(t)
			ctx := context.Background()
			t.Cleanup(func() {
				if f.db != nil {
					_ = f.db.Close()
				}
			})
			taskID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{Seq: 1, Type: "ship", Title: "Held", LandingMode: "local"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.AcquireMount(ctx, f.project.ID, taskID, filepath.Join(f.home, "remuda", "shop")); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.NextLeadLaunch(ctx, f.project.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.db.MarkProjectDown(ctx, f.project.ID); err != nil {
				t.Fatal(err)
			}
			before, err := f.db.ProjectByID(ctx, f.project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if before.DownAt == 0 || before.LeadLaunches == 0 {
				t.Fatalf("fixture Project is not stopped with a Lead launch: %+v", before)
			}
			if mode != "existing" {
				if err := f.db.Close(); err != nil {
					t.Fatal(err)
				}
				database := filepath.Join(f.home, "posse.db")
				if mode == "deleted" {
					if err := os.Remove(database); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(database, []byte("t229 corrupt database"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command(f.binary, "recover", "--rebuild")
			command.Dir, command.Env = f.repo, f.env
			output, rebuildErr := command.CombinedOutput()
			if rebuildErr != nil {
				t.Fatalf("recover --rebuild (%s): %v\n%s", mode, rebuildErr, output)
			}
			if mode != "existing" {
				f.db, err = store.Open(f.home)
				if err != nil {
					t.Fatal(err)
				}
			}
			after, err := f.db.ProjectByID(ctx, f.project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if before.DownAt != after.DownAt || before.LeadLaunches != after.LeadLaunches {
				t.Fatalf("rebuild changed stopped Project state: down_at %d -> %d, lead_launches %d -> %d", before.DownAt, after.DownAt, before.LeadLaunches, after.LeadLaunches)
			}
		})
	}
}
