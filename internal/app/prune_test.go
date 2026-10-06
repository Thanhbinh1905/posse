package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestTaskScratchPruneReclaimsUnheldTerminalTasksOnly(t *testing.T) {
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
	makeTask := func(seq int, state store.State) store.Task {
		t.Helper()
		id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: seq, Type: "ship", Title: "Task", Branch: "posse/t" + strconv.Itoa(seq), BaseRef: "main"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE tasks SET state=? WHERE id=?`, string(state), id); err != nil {
			t.Fatal(err)
		}
		task, err := db.TaskByID(ctx, project.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	makeTask(1, store.StateTornDown)
	makeTask(2, store.StateFailed)
	held := makeTask(3, store.StateLanded)
	makeTask(6, store.StateLost)
	heldFailed := makeTask(7, store.StateFailed)
	activeTeardown := makeTask(5, store.StateTornDown)
	if err := db.StartIntent(ctx, project.ID, activeTeardown.ID, "unsaddle", "in_progress:scratch.remove", "{}", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	for _, task := range []store.Task{held, heldFailed} {
		if _, err := acquireMount(ctx, db, project, task, home, "warm", nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, seq := range []int{1, 2, 3, 4, 5, 6, 7} {
		path := filepath.Join(home, "scratch", project.Name, taskIDString(seq))
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "data"), []byte("abc"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	items, err := taskScratchPruneItems(ctx, db, home, project)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("scratch candidates = %#v, want terminal t1, t2, t4, and t6", items)
	}
	got := map[string]int64{}
	for _, item := range items {
		got[item.label] = item.bytes
	}
	if got["t1"] != 3 || got["t2"] != 3 || got["t4"] != 3 || got["t6"] != 3 || len(got) != 4 {
		t.Fatalf("scratch candidate sizes = %#v, want t1, t2, t4, and t6=3", got)
	}
	unsafeHome := t.TempDir()
	outsideScratch := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outsideScratch, project.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideScratch, filepath.Join(unsafeHome, "scratch")); err != nil {
		t.Fatal(err)
	}
	if _, err := taskScratchPruneItems(ctx, db, unsafeHome, project); err == nil {
		t.Fatal("scratch pruning followed a symlink outside POSSE_HOME")
	}
}

func TestTaskCreationAndScratchAcquisitionSharePruneLock(t *testing.T) {
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
	lock, err := acquireFileLock(ctx, db.Path+".mount-lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	createResult := make(chan error, 1)
	go func() {
		_, _, _, err := createTaskWithSequenceAndIntent(ctx, db, project, home, store.Task{Type: "ship", Title: "Concurrent Rider", ShortName: "concurrent-rider", Branch: "posse/concurrent-rider", BaseRef: "main"})
		createResult <- err
	}()
	select {
	case err := <-createResult:
		t.Fatalf("Task creation bypassed the prune lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if tasks, err := db.Tasks(ctx, project.ID, true); err != nil || len(tasks) != 0 {
		t.Fatalf("Task appeared before serialized creation: %#v %v", tasks, err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-createResult; err != nil {
		t.Fatal(err)
	}
	task, err := db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}

	lock, err = acquireFileLock(ctx, db.Path+".mount-lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	scratchPath := filepath.Join(home, "scratch", project.Name, "t1")
	scratchResult := make(chan error, 1)
	go func() {
		_, err := ensureTaskScratchForTask(ctx, db, home, project, task)
		scratchResult <- err
	}()
	select {
	case err := <-scratchResult:
		t.Fatalf("Task scratch acquisition bypassed the prune lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := os.Stat(scratchPath); !os.IsNotExist(err) {
		t.Fatalf("Task scratch appeared before serialized acquisition: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-scratchResult; err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(scratchPath); err != nil || !info.IsDir() {
		t.Fatalf("serialized Task scratch was not created: %v", err)
	}
}

func TestScratchPruneRevalidatesTaskOwnershipAtApply(t *testing.T) {
	for _, scenario := range []string{"active-state", "active-intent", "held-mount"} {
		t.Run(scenario, func(t *testing.T) {
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
			scratch := filepath.Join(home, "scratch", project.Name, "t1")
			if err := os.MkdirAll(scratch, 0o700); err != nil {
				t.Fatal(err)
			}
			cache := filepath.Join(scratch, "cache")
			if err := os.WriteFile(cache, []byte("active rider cache"), 0o600); err != nil {
				t.Fatal(err)
			}
			items, err := taskScratchPruneItems(ctx, db, home, project)
			if err != nil || len(items) != 1 || items[0].taskID != 0 {
				t.Fatalf("orphan scratch plan = %#v, %v", items, err)
			}
			taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Concurrent Rider", Branch: "posse/concurrent-rider", BaseRef: "main"})
			if err != nil {
				t.Fatal(err)
			}
			task, err := db.TaskByID(ctx, project.ID, taskID)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "active-state":
				if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "Rider started"); err != nil {
					t.Fatal(err)
				}
			case "active-intent":
				if _, err := db.ExecContext(ctx, `UPDATE tasks SET state=? WHERE id=?`, string(store.StateTornDown), taskID); err != nil {
					t.Fatal(err)
				}
				if err := db.StartIntent(ctx, project.ID, taskID, "relaunch", "scratch.environment", "{}", os.Getpid()); err != nil {
					t.Fatal(err)
				}
			case "held-mount":
				if _, err := db.ExecContext(ctx, `UPDATE tasks SET state=? WHERE id=?`, string(store.StateTornDown), taskID); err != nil {
					t.Fatal(err)
				}
				mount, err := acquireMount(ctx, db, project, task, home, "warm", nil)
				if err != nil || mount.State != "held" {
					t.Fatalf("acquire concurrent Rider Mount = %#v, %v", mount, err)
				}
			}
			service := testService(home, nil)
			err = service.applyPruneItem(ctx, db, home, project, items[0])
			var structured *axi.Error
			if !errors.As(err, &structured) || !structured.Retryable {
				t.Fatalf("stale scratch prune error = %v, want retryable ownership failure", err)
			}
			if contents, err := os.ReadFile(cache); err != nil || string(contents) != "active rider cache" {
				t.Fatalf("stale scratch plan removed current Task data: %q %v", contents, err)
			}
		})
	}
}

func TestExpiredTaskArtifactsAreSizedAndRemovedWithoutFollowingSymlinks(t *testing.T) {
	home := t.TempDir()
	project := store.Project{Name: "shop"}
	now := time.Now()
	root := filepath.Join(home, "projects", project.Name, "tasks", "t1")
	attachmentDir := filepath.Join(root, "attachments")
	if err := os.MkdirAll(attachmentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"task.toml":               "task metadata",
		"launch.md":               "launch brief",
		"report.md":               "report body",
		"notes.txt":               "saved note",
		"attachments/result":      "binary evidence",
		"discard/shop-tip.bundle": "captured discard tip",
	}
	for name, contents := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(attachmentDir, 0o500); err != nil {
		t.Fatal(err)
	}
	task := store.Task{Seq: 1, State: store.StateTornDown, UpdatedAt: now.Add(-48 * time.Hour).UnixMilli()}
	items, err := expiredTaskArtifactItems(home, project, []store.Task{task}, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expired artifact candidates = %#v, want one", items)
	}
	var wantBytes int64
	for _, name := range []string{"report.md", "notes.txt", "attachments/result", "discard/shop-tip.bundle"} {
		wantBytes += int64(len(files[name]))
	}
	if items[0].bytes != wantBytes {
		t.Fatalf("expired artifact size = %d, want %d", items[0].bytes, wantBytes)
	}
	if err := removeTaskArtifacts(home, project, task.Seq, items[0].files); err != nil {
		t.Fatalf("remove expired artifacts: %v", err)
	}
	for _, name := range []string{"report.md", "notes.txt", "attachments/result", "discard/shop-tip.bundle"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Errorf("expired artifact %s remains: %v", name, err)
		}
	}
	for _, name := range []string{"task.toml", "launch.md"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("Task instruction %s was removed: %v", name, err)
		}
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("do not remove"), 0o600); err != nil {
		t.Fatal(err)
	}
	unsafeRoot := filepath.Join(home, "projects", project.Name, "tasks", "t2")
	if err := os.MkdirAll(filepath.Join(unsafeRoot, "attachments"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unsafeRoot, "attachments", "evidence"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(unsafeRoot, "attachments")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(unsafeRoot, "attachments")); err != nil {
		t.Fatal(err)
	}
	if err := removeTaskArtifactFiles(unsafeRoot, []string{"attachments/evidence"}); err == nil {
		t.Fatal("artifact removal followed a symlink outside Task data")
	}
	if contents, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || strings.TrimSpace(string(contents)) != "do not remove" {
		t.Fatalf("outside file changed: %q %v", contents, err)
	}

	unsafeHome := t.TempDir()
	outsideProjects := t.TempDir()
	outsideArtifact := filepath.Join(outsideProjects, project.Name, "tasks", "t3")
	if err := os.MkdirAll(outsideArtifact, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideArtifact, "report.md"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideProjects, filepath.Join(unsafeHome, "projects")); err != nil {
		t.Fatal(err)
	}
	if _, err := expiredTaskArtifactItems(unsafeHome, project, []store.Task{{Seq: 3, State: store.StateTornDown, UpdatedAt: now.Add(-48 * time.Hour).UnixMilli()}}, time.Hour, now); err == nil {
		t.Fatal("artifact pruning followed a symlink outside POSSE_HOME")
	}
	if err := removeTaskArtifacts(unsafeHome, project, 3, []string{"report.md"}); err == nil {
		t.Fatal("artifact removal accepted a symlinked Project directory")
	}
	if contents, err := os.ReadFile(filepath.Join(outsideArtifact, "report.md")); err != nil || contents == nil {
		t.Fatalf("outside Task artifact changed: %q %v", contents, err)
	}
}

func TestPruneRemovesOnlyStaleOwnedWorktreeRegistrations(t *testing.T) {
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
	managedPath := filepath.Join(home, "remuda", "shop", "mount-1")
	if err := os.MkdirAll(filepath.Dir(managedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "worktree", "add", "--detach", managedPath, "main")
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state) VALUES(?,1,?,'idle')`, project.ID, managedPath); err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(root, "outside-worktree")
	gitTest(t, repo, "worktree", "add", "--detach", outsidePath, "main")
	if err := os.RemoveAll(managedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(managedPath)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(outsidePath); err != nil {
		t.Fatal(err)
	}
	mounts, err := db.Mounts(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	items, err := staleOwnedWorktreeItems(ctx, db, project, home, mounts)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].path != managedPath || items[0].bytes != 0 {
		t.Fatalf("stale worktree candidates = %#v, want only Posse-owned registration %s", items, managedPath)
	}
	service := testService(home, nil)
	if err := service.applyPruneItem(ctx, db, home, project, items[0]); err != nil {
		t.Fatalf("remove stale Posse worktree registration: %v", err)
	}
	listing, err := gitOutput(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listing, managedPath) || !strings.Contains(listing, outsidePath) {
		t.Fatalf("prune changed unexpected worktree registrations: %s", listing)
	}
	mounts, err = db.Mounts(ctx, project.ID)
	if err != nil || len(mounts) != 0 {
		t.Fatalf("Mounts after pruning stale registration = %#v, %v", mounts, err)
	}
}

func TestPruneBranchRechecksTaskOwnershipAndLiveWorktreesAtApply(t *testing.T) {
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
	makeLandedTask := func(seq int) (store.Task, string) {
		t.Helper()
		branch := "posse/t" + strconv.Itoa(seq)
		gitTest(t, repo, "switch", "-c", branch)
		file := "landed-" + strconv.Itoa(seq) + ".txt"
		if err := os.WriteFile(filepath.Join(repo, file), []byte("landed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, repo, "add", file)
		gitTest(t, repo, "commit", "-m", branch)
		sha := strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD"))
		gitTest(t, repo, "switch", "main")
		gitTest(t, repo, "merge", "--ff-only", branch)
		id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: seq, Type: "ship", Title: "Landed Task", Branch: branch, BaseRef: "main", LandingMode: "local"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE tasks SET state='landed' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		task, err := db.TaskByID(ctx, project.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		return task, sha
	}
	service := testService(home, nil)
	assertProtectedAtApply := func(task store.Task, expectedSHA string, afterPlan func()) {
		t.Helper()
		items, err := landedOrDiscardedBranchItems(ctx, db, home, project, []store.Task{task})
		if err != nil || len(items) != 1 {
			t.Fatalf("branch plan for %s = %#v, %v; want one initially eligible branch", task.Branch, items, err)
		}
		afterPlan()
		if err := service.applyPruneItem(ctx, db, home, project, items[0]); err == nil {
			t.Errorf("prune applied stale branch plan for %s after its ownership changed", task.Branch)
		}
		if got := strings.TrimSpace(gitTest(t, repo, "rev-parse", "refs/heads/"+task.Branch)); got != expectedSHA {
			t.Errorf("stale prune plan changed %s from %s to %s", task.Branch, expectedSHA, got)
		}
	}

	intentTask, intentSHA := makeLandedTask(1)
	assertProtectedAtApply(intentTask, intentSHA, func() {
		if err := db.StartIntent(ctx, project.ID, intentTask.ID, "unsaddle", "in_progress:branch.remove", "{}", os.Getpid()); err != nil {
			t.Fatal(err)
		}
	})
	if err := db.FinishIntent(ctx, mustTaskIntent(t, db, intentTask.ID), os.Getpid()); err != nil {
		t.Fatal(err)
	}

	heldTask, heldSHA := makeLandedTask(2)
	assertProtectedAtApply(heldTask, heldSHA, func() {
		mount, err := db.AcquireMount(ctx, project.ID, heldTask.ID, filepath.Join(home, "remuda", project.Name))
		if err != nil {
			t.Fatal(err)
		}
		gitTest(t, repo, "worktree", "add", mount.Path, heldTask.Branch)
	})

	checkedOutTask, checkedOutSHA := makeLandedTask(3)
	assertProtectedAtApply(checkedOutTask, checkedOutSHA, func() {
		path := filepath.Join(root, "external-worktree")
		gitTest(t, repo, "worktree", "add", path, checkedOutTask.Branch)
	})
}

func mustTaskIntent(t *testing.T, db *store.DB, taskID int64) int64 {
	t.Helper()
	intent, err := db.IntentByTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	return intent.ID
}

func TestPruneSelectsOnlyLandedAndCapturedDiscardBranches(t *testing.T) {
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
	makeBranch := func(name, file string) string {
		t.Helper()
		gitTest(t, repo, "switch", "-c", name)
		if err := os.WriteFile(filepath.Join(repo, file), []byte(name+" work\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, repo, "add", file)
		gitTest(t, repo, "commit", "-m", name)
		sha := strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD"))
		gitTest(t, repo, "switch", "main")
		return sha
	}
	landedSHA := makeBranch("posse/t1", "landed.txt")
	gitTest(t, repo, "merge", "--ff-only", "posse/t1")
	discardSHA := makeBranch("posse/t3", "discard.txt")
	unlandedSHA := makeBranch("posse/t2", "unlanded.txt")
	prMergedSHA := makeBranch("posse/t4", "pr-merged.txt")
	noMistakesMergedSHA := makeBranch("posse/t6", "no-mistakes-merged.txt")
	noMistakesHeadSHA := makeBranch("posse/t7", "no-mistakes-head.txt")
	gitTest(t, repo, "switch", "posse/t7")
	if err := os.WriteFile(filepath.Join(repo, "no-mistakes-follow-up.txt"), []byte("unmerged follow-up\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", "no-mistakes-follow-up.txt")
	gitTest(t, repo, "commit", "-m", "follow-up after merged no-mistakes PR")
	noMistakesFollowUpSHA := strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD"))
	gitTest(t, repo, "switch", "main")
	gitTest(t, repo, "switch", "-c", "posse/t5")
	if err := os.WriteFile(filepath.Join(repo, "pr-head.txt"), []byte("merged head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", "pr-head.txt")
	gitTest(t, repo, "commit", "-m", "PR head")
	prHeadSHA := strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(repo, "pr-follow-up.txt"), []byte("unmerged follow-up\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", "pr-follow-up.txt")
	gitTest(t, repo, "commit", "-m", "follow-up after PR head")
	prFollowUpSHA := strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD"))
	gitTest(t, repo, "switch", "main")
	makeTask := func(seq int, branch string, state store.State, landingMode string) store.Task {
		t.Helper()
		id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: seq, Type: "ship", Title: "Task", Branch: branch, BaseRef: "main", LandingMode: landingMode})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE tasks SET state=? WHERE id=?`, string(state), id); err != nil {
			t.Fatal(err)
		}
		task, err := db.TaskByID(ctx, project.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	landed := makeTask(1, "posse/t1", store.StateLanded, "local")
	unlanded := makeTask(2, "posse/t2", store.StateWorking, "local")
	discarded := makeTask(3, "posse/t3", store.StateTornDown, "local")
	mergedPR := makeTask(4, "posse/t4", store.StateLanded, "pr")
	advancedPR := makeTask(5, "posse/t5", store.StateLanded, "pr")
	mergedNoMistakes := makeTask(6, "posse/t6", store.StateLanded, "no-mistakes")
	advancedNoMistakes := makeTask(7, "posse/t7", store.StateLanded, "no-mistakes")
	for _, entry := range []struct {
		task    store.Task
		headSHA string
	}{
		{task: mergedPR, headSHA: prMergedSHA},
		{task: advancedPR, headSHA: prHeadSHA},
		{task: mergedNoMistakes, headSHA: noMistakesMergedSHA},
		{task: advancedNoMistakes, headSHA: noMistakesHeadSHA},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO pr_observations(project_id,task_id,pr_url,head_sha,state,observed_at) VALUES(?,?,?,?,?,?)`, project.ID, entry.task.ID, "https://example.test/pull/1", entry.headSHA, "MERGED", time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordApprovalWithBranch(ctx, discarded.ID, "discard", "approved discard", discardSHA); err != nil {
		t.Fatal(err)
	}
	if err := testService(home, nil).captureDiscardTips(ctx, db, home, project, discarded); err != nil {
		t.Fatalf("capture approved discard for prune: %v", err)
	}
	items, err := landedOrDiscardedBranchItems(ctx, db, home, project, []store.Task{landed, unlanded, discarded, mergedPR, advancedPR, mergedNoMistakes, advancedNoMistakes})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 || items[0].label != "posse/t1" || items[1].label != "posse/t3" || items[2].label != "posse/t4" || items[3].label != "posse/t6" {
		t.Fatalf("prunable branch items = %#v, want landed, captured discard, merged PR, and merged no-mistakes branches", items)
	}
	if items[0].sha != landedSHA || items[1].sha != discardSHA || items[1].bytes <= 0 || items[2].sha != prMergedSHA || items[3].sha != noMistakesMergedSHA || prFollowUpSHA == prHeadSHA {
		t.Fatalf("branch candidates lost their captured tips or sizes: %#v", items)
	}
	service := testService(home, nil)
	for _, item := range items {
		if err := service.applyPruneItem(ctx, db, home, project, item); err != nil {
			t.Fatalf("remove safe branch %s: %v", item.label, err)
		}
	}
	for _, branch := range []string{"posse/t1", "posse/t3", "posse/t4", "posse/t6"} {
		command := exec.Command("git", "-C", repo, "show-ref", "--verify", "refs/heads/"+branch)
		if output, err := command.CombinedOutput(); err == nil {
			t.Errorf("pruned branch %s still exists: %s", branch, output)
		}
	}
	if got := strings.TrimSpace(gitTest(t, repo, "rev-parse", "refs/heads/posse/t2")); got != unlandedSHA {
		t.Fatalf("prune changed unlanded branch tip to %s, want %s", got, unlandedSHA)
	}
	if got := strings.TrimSpace(gitTest(t, repo, "rev-parse", "refs/heads/posse/t5")); got != prFollowUpSHA {
		t.Fatalf("prune changed follow-up PR branch tip to %s, want %s", got, prFollowUpSHA)
	}
	if got := strings.TrimSpace(gitTest(t, repo, "rev-parse", "refs/heads/posse/t7")); got != noMistakesFollowUpSHA {
		t.Fatalf("prune changed follow-up no-mistakes branch tip to %s, want %s", got, noMistakesFollowUpSHA)
	}
}

func TestSafeManagedMountPathRejectsExternalAndSymlinkedMounts(t *testing.T) {
	home := t.TempDir()
	managedProject := filepath.Join(home, "remuda", "shop")
	managedMount := filepath.Join(managedProject, "mount-1")
	if err := os.MkdirAll(managedMount, 0o700); err != nil {
		t.Fatal(err)
	}
	if !safeManagedMountPath(home, "shop", managedMount) {
		t.Fatal("safe Posse-owned Mount was rejected")
	}
	outside := t.TempDir()
	if safeManagedMountPath(home, "shop", outside) {
		t.Fatal("external path was accepted as a Posse-owned Mount")
	}
	if err := os.RemoveAll(managedProject); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, managedProject); err != nil {
		t.Fatal(err)
	}
	if safeManagedMountPath(home, "shop", managedMount) {
		t.Fatal("Mount through a symlinked Project directory was accepted")
	}
}
