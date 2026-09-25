package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestAckAutoUnsaddlesReportedTaskOnlyForFinishedPolicy(t *testing.T) {
	for _, policy := range []string{"finished", "landed", "never"} {
		t.Run(policy, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			initRepo(t, repo)
			home := filepath.Join(root, "posse")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nauto_unsaddle = \""+policy+"\"\n"), 0o600); err != nil {
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
			taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "scout", Title: "Report", LandingMode: "local"})
			if err != nil {
				t.Fatal(err)
			}
			for _, transition := range []struct {
				from, to store.State
				source   string
			}{{store.StateSpawning, store.StateWorking, "cli"}, {store.StateWorking, store.StateDone, "worker"}, {store.StateDone, store.StateReported, "cli"}} {
				if err := db.Transition(ctx, taskID, transition.from, transition.to, transition.source, "state fixture"); err != nil {
					t.Fatal(err)
				}
			}
			noticeID, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: taskID, Kind: "task_done", Summary: "Report finished"})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			t.Chdir(repo)
			var output bytes.Buffer
			cli := testService(home, nil).CLI()
			cli.Out = &output
			if code := cli.Run([]string{"ack", stringID(noticeID)}); code != 0 {
				t.Fatalf("ack failed: %s", output.String())
			}
			observer, err := store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			defer observer.Close()
			task, err := observer.Task(ctx, project.ID, "t1")
			if err != nil {
				t.Fatal(err)
			}
			if policy == "finished" {
				if task.State != store.StateTornDown || !strings.Contains(output.String(), "Report") {
					t.Fatalf("finished policy did not auto-unsaddle the reported Task: state=%s output=%s", task.State, output.String())
				}
			} else if task.State != store.StateReported {
				t.Fatalf("%s policy auto-unsaddled a reported Task: state=%s", policy, task.State)
			}
		})
	}
}

func stringID(id int64) string { return strconv.FormatInt(id, 10) }
