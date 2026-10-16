package execution

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestCanonicalPathResolvesSymlinkAliasesAndMissingSuffix(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "existing"), 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}

	got, err := CanonicalPath(filepath.Join(alias, "existing", "not-yet-created", "child"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(real, "existing", "not-yet-created", "child")
	if got != want {
		t.Fatalf("CanonicalPath() = %q, want %q", got, want)
	}
}

func TestPathLeasesRefuseEqualityAndAncestorOverlapAcrossProjects(t *testing.T) {
	for _, test := range []struct {
		name   string
		first  string
		second string
	}{
		{name: "equal", first: "tree/member", second: "tree/member"},
		{name: "ancestor", first: "tree", second: "tree/member"},
		{name: "descendant", first: "tree/member", second: "tree"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, projectA, projectB := resourceDB(t)
			manager := NewManager(db)
			root := t.TempDir()
			first, err := manager.AcquirePath(ctx, filepath.Join(root, filepath.FromSlash(test.first)), store.ResourceLeaseRequest{
				OwnerProjectID: projectA.ID, Owner: "writer A", SourceKey: "writer-a",
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = manager.AcquirePath(ctx, filepath.Join(root, filepath.FromSlash(test.second)), store.ResourceLeaseRequest{
				OwnerProjectID: projectB.ID, Owner: "writer B", SourceKey: "writer-b",
			})
			var conflict *store.ResourceLeaseConflict
			if !errors.As(err, &conflict) || conflict.Lease.Owner != "writer A" {
				t.Fatalf("overlapping claim error = %v, want conflict naming writer A", err)
			}
			if err := manager.Release(ctx, first); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWorkspaceMemberAndSeparatelyRegisteredProjectSharePathScopes(t *testing.T) {
	ctx := context.Background()
	db, _, _ := resourceDB(t)
	memberRoot := filepath.Join(t.TempDir(), "member-repo")
	if err := os.MkdirAll(memberRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, err := db.CreateWorkspaceProject(ctx, "workspace", filepath.Dir(memberRoot), []store.ProjectRepo{{
		Name: "backend", Path: filepath.Base(memberRoot), DefaultBranch: "main", Status: store.RepoActive,
	}})
	if err != nil {
		t.Fatal(err)
	}
	registeredMember, err := db.CreateProject(ctx, "backend", memberRoot, "main")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(db)
	_, err = manager.AcquirePath(ctx, memberRoot, store.ResourceLeaseRequest{
		OwnerProjectID: workspace.ID, Owner: "workspace writer", SourceKey: "workspace-member-writer",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.AcquirePath(ctx, filepath.Join(memberRoot, "nested"), store.ResourceLeaseRequest{
		OwnerProjectID: registeredMember.ID, Owner: "separately registered member writer", SourceKey: "registered-member-writer",
	})
	var conflict *store.ResourceLeaseConflict
	if !errors.As(err, &conflict) || conflict.Lease.Owner != "workspace writer" {
		t.Fatalf("workspace/member overlap error = %v, want conflict across registrations", err)
	}
}

func TestPathLeaseCanonicalizesSymlinkAliases(t *testing.T) {
	ctx := context.Background()
	db, projectA, projectB := resourceDB(t)
	manager := NewManager(db)
	root := t.TempDir()
	target := filepath.Join(root, "project")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	_, err := manager.AcquirePath(ctx, target, store.ResourceLeaseRequest{OwnerProjectID: projectA.ID, Owner: "registered Project", SourceKey: "real"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.AcquirePath(ctx, alias, store.ResourceLeaseRequest{OwnerProjectID: projectB.ID, Owner: "alias Project", SourceKey: "alias"})
	var conflict *store.ResourceLeaseConflict
	if !errors.As(err, &conflict) || conflict.Lease.ScopePath != target {
		t.Fatalf("symlink alias claim error = %v, want canonical path conflict", err)
	}
}

func TestPathLeaseIsAtomicAcrossConcurrentProjects(t *testing.T) {
	ctx := context.Background()
	db, projectA, projectB := resourceDB(t)
	manager := NewManager(db)
	path := filepath.Join(t.TempDir(), "shared")
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var successes, conflicts int
	var unexpected error
	for index, project := range []store.Project{projectA, projectB} {
		wg.Add(1)
		go func(index int, project store.Project) {
			defer wg.Done()
			<-start
			_, err := manager.AcquirePath(ctx, path, store.ResourceLeaseRequest{
				OwnerProjectID: project.ID, Owner: "writer", SourceKey: []string{"writer-a", "writer-b"}[index],
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes++
			} else {
				var conflict *store.ResourceLeaseConflict
				if errors.As(err, &conflict) {
					conflicts++
				} else {
					unexpected = errors.Join(unexpected, err)
				}
			}
		}(index, project)
	}
	close(start)
	wg.Wait()
	if unexpected != nil || successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent path claims: successes=%d conflicts=%d unexpected=%v", successes, conflicts, unexpected)
	}
}

func TestLegacyMountOwnershipBecomesAPathLease(t *testing.T) {
	ctx := context.Background()
	db, projectA, projectB := resourceDB(t)
	taskID, err := db.CreateTask(ctx, projectA.ID, store.Task{Seq: 1, Type: "ship", Title: "Mounted writer"})
	if err != nil {
		t.Fatal(err)
	}
	mount, err := db.AcquireMount(ctx, projectA.ID, taskID, filepath.Join(t.TempDir(), "mounts"))
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(db)
	_, err = manager.AcquirePath(ctx, mount.Path, store.ResourceLeaseRequest{OwnerProjectID: projectB.ID, Owner: "overlapping writer", SourceKey: "overlapping-writer"})
	var conflict *store.ResourceLeaseConflict
	if !errors.As(err, &conflict) || conflict.Lease.MountID != mount.ID {
		t.Fatalf("path claim overlapping a legacy Mount = %v, want Mount lease conflict", err)
	}
	leases, err := db.ResourceLeases(ctx, projectA.ID)
	if err != nil || len(leases) != 1 || leases[0].MountID != mount.ID {
		t.Fatalf("persisted Mount leases = %#v, %v", leases, err)
	}
}

func TestGitRefLeasesShareCommonDirAndAllowIndependentBranches(t *testing.T) {
	ctx := context.Background()
	db, projectA, projectB := resourceDB(t)
	manager := NewManager(db)
	repo := initResourceGitRepo(t)
	alias := filepath.Join(filepath.Dir(repo), "repo-alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	mainLease, err := manager.AcquireGitRef(ctx, repo, "refs/heads/main", store.ResourceLeaseRequest{
		OwnerProjectID: projectA.ID, Owner: "main writer", SourceKey: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.AcquireGitRef(ctx, alias, "refs/heads/main", store.ResourceLeaseRequest{
		OwnerProjectID: projectB.ID, Owner: "same-ref writer", SourceKey: "same-main",
	})
	var conflict *store.ResourceLeaseConflict
	if !errors.As(err, &conflict) || conflict.Lease.Owner != "main writer" {
		t.Fatalf("same-ref alias claim error = %v, want conflict", err)
	}
	independent, err := manager.AcquireGitRef(ctx, alias, "refs/heads/feature", store.ResourceLeaseRequest{
		OwnerProjectID: projectB.ID, Owner: "feature writer", SourceKey: "feature",
	})
	if err != nil {
		t.Fatalf("independent Git ref was blocked: %v", err)
	}
	if err := manager.Release(ctx, mainLease); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(ctx, independent); err != nil {
		t.Fatal(err)
	}
}

func TestIntentCannotFinishUntilItsResourceCustodyIsReconciled(t *testing.T) {
	ctx := context.Background()
	db, project, _ := resourceDB(t)
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Land with a lease"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.StartIntent(ctx, project.ID, taskID, "land", "merge", "{}", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	intent, err := db.IntentByTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewManager(db).AcquirePath(ctx, filepath.Join(t.TempDir(), "root"), store.ResourceLeaseRequest{
		OwnerProjectID: project.ID, OwnerTaskID: taskID, Owner: "Land t1", SourceKey: "land-intent-path", IntentID: intent.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishIntent(ctx, intent.ID, os.Getpid()); !errors.Is(err, store.ErrResourceLeaseHeld) {
		t.Fatalf("FinishIntent() = %v, want unresolved resource custody", err)
	}
	if err := db.ReconcileResourceLeasesForIntent(ctx, intent.ID, "re-read the target ref after the interrupted effect"); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishIntent(ctx, intent.ID, os.Getpid()); err != nil {
		t.Fatalf("finish reconciled intent: %v", err)
	}
}

func TestLeaseSurvivesExecutorCrashAndRebuildUntilReconciliation(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "project-a", filepath.Join(home, "repo-a"), "main")
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateProject(ctx, "project-b", filepath.Join(home, "repo-b"), "main")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(db)
	path := filepath.Join(home, "source")
	lease, err := manager.AcquirePath(ctx, path, store.ResourceLeaseRequest{OwnerProjectID: project.ID, Owner: "interrupted writer", SourceKey: "interrupted"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := NewManager(db).AcquirePath(ctx, filepath.Join(path, "child"), store.ResourceLeaseRequest{OwnerProjectID: other.ID, Owner: "new writer", SourceKey: "new"}); err == nil {
		t.Fatal("new writer stole a lease left by a crashed executor")
	}
	if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
		t.Fatalf("rebuild resource custody: %v", err)
	}
	if _, err := NewManager(db).AcquirePath(ctx, path, store.ResourceLeaseRequest{OwnerProjectID: other.ID, Owner: "new writer", SourceKey: "after-rebuild"}); err == nil {
		t.Fatal("rebuild discarded the unresolved resource lease")
	}
	if err := db.ReconcileResourceLease(ctx, lease.ID, lease.CustodyToken, "verified the interrupted attempt and preserved its output"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(db).AcquirePath(ctx, path, store.ResourceLeaseRequest{OwnerProjectID: other.ID, Owner: "new writer", SourceKey: "after-reconcile"}); err != nil {
		t.Fatalf("claim after explicit custody reconciliation: %v", err)
	}
}

func resourceDB(t *testing.T) (*store.DB, store.Project, store.Project) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	projectA, err := db.CreateProject(ctx, "project-a", filepath.Join(t.TempDir(), "repo-a"), "main")
	if err != nil {
		t.Fatal(err)
	}
	projectB, err := db.CreateProject(ctx, "project-b", filepath.Join(t.TempDir(), "repo-b"), "main")
	if err != nil {
		t.Fatal(err)
	}
	return db, projectA, projectB
}

func initResourceGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main", root}, {"-C", root, "config", "user.name", "Test"}, {"-C", root, "config", "user.email", "test@example.test"}, {"-C", root, "commit", "--allow-empty", "--quiet", "-m", "initial"}} {
		command := exec.Command("git", args...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	return root
}
