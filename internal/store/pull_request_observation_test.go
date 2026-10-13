package store

import (
	"context"
	"testing"
)

func TestPRObservationResetsFailureStreakOnlyWhenAccepted(t *testing.T) {
	for _, test := range []struct {
		name      string
		stale     bool
		workspace bool
	}{
		{name: "Task terminal transition"},
		{name: "stale Task observation", stale: true},
		{name: "Member terminal transition", workspace: true},
		{name: "stale Member observation", stale: true, workspace: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
			if err != nil {
				t.Fatal(err)
			}
			taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "PR watch", LandingMode: "pr"})
			if err != nil {
				t.Fatal(err)
			}
			for _, transition := range []struct {
				from, to State
				source   string
			}{{StateSpawning, StateWorking, "cli"}, {StateWorking, StateDone, "worker"}} {
				if err := db.Transition(ctx, taskID, transition.from, transition.to, transition.source, "test"); err != nil {
					t.Fatal(err)
				}
			}
			prURL := "https://github.com/acme/shop/pull/17"
			if err := db.UpdateTaskLanding(ctx, taskID, prURL, ""); err != nil {
				t.Fatal(err)
			}
			if err := db.SetTaskGatedSHA(ctx, taskID, "gated-sha"); err != nil {
				t.Fatal(err)
			}
			if err := db.Transition(ctx, taskID, StateDone, StateLanding, "cli", "land"); err != nil {
				t.Fatal(err)
			}
			memberRepo := ""
			if test.workspace {
				memberRepo = "api"
				if err := db.CreateTaskRepos(ctx, taskID, []TaskRepo{{Repo: memberRepo, LandingMode: "pr"}}); err != nil {
					t.Fatal(err)
				}
				if err := db.UpdateTaskRepo(ctx, TaskRepo{TaskID: taskID, Repo: memberRepo, LandingMode: "pr", State: TaskRepoLanding, GatedSHA: "gated-sha", PRURL: prURL}); err != nil {
					t.Fatal(err)
				}
			}
			for attempt := 1; attempt <= 2; attempt++ {
				if _, err := db.RecordPRTaskWatchFailure(ctx, project.ID, taskID, memberRepo, prURL, int64(attempt), "forge timed out", "watch failed"); err != nil {
					t.Fatal(err)
				}
			}
			task, err := db.TaskByID(ctx, project.ID, taskID)
			if err != nil {
				t.Fatal(err)
			}
			observedURL := prURL
			if test.stale {
				observedURL = "https://github.com/acme/shop/pull/18"
				if test.workspace {
					repo, err := db.TaskRepos(ctx, taskID)
					if err != nil || len(repo) != 1 {
						t.Fatalf("Task members: %#v, %v", repo, err)
					}
					repo[0].PRURL = observedURL
					if err := db.UpdateTaskRepo(ctx, repo[0]); err != nil {
						t.Fatal(err)
					}
				} else if err := db.UpdateTaskLanding(ctx, taskID, observedURL, ""); err != nil {
					t.Fatal(err)
				}
			}
			observation := PRObservation{ProjectID: project.ID, TaskID: taskID, PRURL: prURL, HeadSHA: "gated-sha", State: "MERGED", MergeCommit: "merge-sha", ObservedAt: 3}
			var accepted bool
			if test.workspace {
				accepted, err = db.RecordMemberPRObservation(ctx, memberRepo, observation, MemberPREffect{RepoState: TaskRepoLanded, LandedRef: "merge-sha"})
			} else {
				accepted, err = db.RecordPRObservation(ctx, observation, PRObservationEffect{TransitionTo: StateLanded, TransitionNote: "merged", LandedRef: "merge-sha"}, task)
			}
			if err != nil {
				t.Fatal(err)
			}
			if accepted == test.stale {
				t.Fatalf("accepted stale=%t, want accepted=%t", test.stale, !test.stale)
			}
			var failures int
			if err := db.QueryRowContext(ctx, `SELECT consecutive_failures FROM pr_task_watch_failures WHERE project_id=? AND task_id=? AND repo=?`, project.ID, taskID, memberRepo).Scan(&failures); err != nil {
				t.Fatal(err)
			}
			wantFailures := 0
			if test.stale {
				wantFailures = 2
			}
			if failures != wantFailures {
				t.Fatalf("failure streak after accepted=%t observation is %d, want %d", accepted, failures, wantFailures)
			}
			current, err := db.TaskByID(ctx, project.ID, taskID)
			if err != nil {
				t.Fatal(err)
			}
			wantState := StateLanded
			if test.stale {
				wantState = StateLanding
			}
			if current.State != wantState {
				t.Fatalf("Task state after accepted=%t observation is %q, want %q", accepted, current.State, wantState)
			}
		})
	}
}
