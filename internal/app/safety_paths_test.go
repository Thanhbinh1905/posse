package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestRideRejectsEachLooserContractBeforeCreatingTask(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		frontmatter string
		wantField   string
	}{
		{name: "Landing Mode", frontmatter: "landing_mode: local\n", wantField: "landing_mode"},
		{name: "Autonomy", frontmatter: "autonomy: { land: auto }\n", wantField: "autonomy.land"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			initRepo(t, repo)
			home := filepath.Join(root, "posse")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			globalConfig := "[defaults]\nlanding_mode = \"pr\"\nmax_workers = 4\n\n[profiles.deep]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"deep\"\n"
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(globalConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			projectConfig := filepath.Join(home, "projects", "shop", "config.toml")
			if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(projectConfig, []byte("[autonomy]\nland = \"ask\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.CreateProject(context.Background(), "shop", repo, "main"); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			brief := filepath.Join(root, "brief.md")
			briefText := fmt.Sprintf("---\ntype: ship\ntitle: Validate tightening\ndone_when: the contract is enforced\n%s---\n", testCase.frontmatter)
			if err := os.WriteFile(brief, []byte(briefText), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Chdir(repo)
			var output bytes.Buffer
			cli := testService(home, nil).CLI()
			cli.Out = &output
			if code := cli.Run([]string{"ride", "--brief", brief, "--name", "validate-tightening"}); code != 1 || !strings.Contains(output.String(), testCase.wantField) {
				t.Fatalf("looser %s was not refused: code=%d output=%s", testCase.name, code, output.String())
			}
			db, err = store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var tasks, mounts int
			if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM tasks`).Scan(&tasks); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM mounts`).Scan(&mounts); err != nil {
				t.Fatal(err)
			}
			if tasks != 0 || mounts != 0 {
				t.Fatalf("refused ride created side effects: tasks=%d mounts=%d", tasks, mounts)
			}
		})
	}
}

func TestUnsaddleDiscardCommandRequiresApprovalBeforeDestructiveEffects(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	mountPath := filepath.Join(root, "mount")
	initRepo(t, repo)
	gitTest(t, repo, "worktree", "add", "-b", "posse/t1", mountPath, "main")
	if err := os.WriteFile(filepath.Join(mountPath, "unlanded.txt"), []byte("recoverable work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, mountPath, "add", "unlanded.txt")
	gitTest(t, mountPath, "commit", "-m", "unlanded work")
	branchSHA := strings.TrimSpace(gitTest(t, repo, "rev-parse", "refs/heads/posse/t1"))
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Unlanded", LandingMode: "local", Branch: "posse/t1", BaseRef: "main", WorktreePath: mountPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, project.ID, mountPath, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, taskID, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "test Worker started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateWorking, store.StateFailed, "worker", "test Worker failed"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	var output bytes.Buffer
	cli := testService(home, nil).CLI()
	cli.Out = &output
	if code := cli.Run([]string{"unsaddle", "t1", "--discard"}); code != 1 || !strings.Contains(output.String(), "teardown_refused") {
		t.Fatalf("discard without recorded approval was not refused: code=%d output=%s", code, output.String())
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	task, err := db.Task(ctx, project.ID, "t1")
	if err != nil || task.State != store.StateFailed {
		t.Fatalf("refused discard changed Task state: %#v, %v", task, err)
	}
	mounts, err := db.Mounts(ctx, project.ID)
	if err != nil || len(mounts) != 1 || mounts[0].State != "held" || mounts[0].TaskID != taskID {
		t.Fatalf("refused discard changed Mount ownership: %#v, %v", mounts, err)
	}
	var approvals, tornDown int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE task_id=? AND action='discard'`, taskID).Scan(&approvals); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transitions WHERE task_id=? AND to_state='torn-down'`, taskID).Scan(&tornDown); err != nil {
		t.Fatal(err)
	}
	gotSHA := strings.TrimSpace(gitTest(t, repo, "rev-parse", "refs/heads/posse/t1"))
	if approvals != 0 || tornDown != 0 || gotSHA != branchSHA {
		t.Fatalf("refused discard changed recoverable state: approvals=%d tornDown=%d branch=%s want=%s", approvals, tornDown, gotSHA, branchSHA)
	}
	if got := strings.TrimSpace(gitTest(t, mountPath, "status", "--porcelain")); got != "" {
		t.Fatalf("refused discard modified Mount contents: %q", got)
	}
}
