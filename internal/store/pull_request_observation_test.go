package store

import (
	"context"
	"encoding/json"
	"testing"
)

func TestWorkspaceMergeReadyNoticeRepeatsOnlyForNewCompletion(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Workspace PR", MergeMode: "pr"})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		from, to State
		source   string
	}{{StateSpawning, StateWorking, "cli"}, {StateWorking, StateDone, "worker"}} {
		if err := db.Transition(ctx, taskID, transition.from, transition.to, transition.source, "initial completion"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateTaskRepos(ctx, taskID, []TaskRepo{{Repo: "api", MergeMode: "pr"}}); err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/acme/api/pull/17"
	if err := db.UpdateTaskRepo(ctx, TaskRepo{TaskID: taskID, Repo: "api", MergeMode: "pr", State: TaskRepoMerge, GatedSHA: "head", PRURL: prURL}); err != nil {
		t.Fatal(err)
	}
	observation := PRObservation{ProjectID: project.ID, TaskID: taskID, PRURL: prURL, HeadSHA: "head", State: "OPEN", Checks: `{"state":"SUCCESS","failures":[]}`, Review: "APPROVED", Mergeable: "MERGEABLE", ObservedAt: 1}
	notice := Notice{ProjectID: project.ID, TaskID: taskID, Kind: "merge_ready", Summary: "ready", DataJSON: `{"repo":"api","url":"https://github.com/acme/api/pull/17","head_sha":"head"}`}
	record := func() (bool, error) {
		return db.RecordMemberPRObservation(ctx, "api", observation, MemberPREffect{Notices: []Notice{notice}})
	}
	if accepted, err := record(); err != nil || !accepted {
		t.Fatalf("first ready observation accepted=%t err=%v, want accepted", accepted, err)
	}
	if err := db.Transition(ctx, taskID, StateDone, StateWorking, "lead", "follow-up started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, StateWorking, StateDone, "worker", "follow-up completed"); err != nil {
		t.Fatal(err)
	}
	if accepted, err := record(); err != nil || !accepted {
		t.Fatalf("same ready head after a new completion accepted=%t err=%v, want a new Notice", accepted, err)
	}
	if accepted, err := record(); err != nil || accepted {
		t.Fatalf("same ready head and completion accepted=%t err=%v, want duplicate suppressed", accepted, err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var completions []int64
	for _, got := range notices {
		if got.Kind != "merge_ready" {
			continue
		}
		var data struct {
			Completion int64 `json:"completion_transition"`
		}
		if err := json.Unmarshal([]byte(got.DataJSON), &data); err != nil {
			t.Fatal(err)
		}
		completions = append(completions, data.Completion)
	}
	if len(completions) != 2 || completions[0] == completions[1] {
		t.Fatalf("merge_ready completion transitions = %v, want two distinct completions", completions)
	}
}

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
			taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "PR watch", MergeMode: "pr"})
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
			if err := db.UpdateTaskMerge(ctx, taskID, prURL, ""); err != nil {
				t.Fatal(err)
			}
			if err := db.SetTaskGatedSHA(ctx, taskID, "gated-sha"); err != nil {
				t.Fatal(err)
			}
			if err := db.Transition(ctx, taskID, StateDone, StateMerging, "cli", "merge"); err != nil {
				t.Fatal(err)
			}
			memberRepo := ""
			if test.workspace {
				memberRepo = "api"
				if err := db.CreateTaskRepos(ctx, taskID, []TaskRepo{{Repo: memberRepo, MergeMode: "pr"}}); err != nil {
					t.Fatal(err)
				}
				if err := db.UpdateTaskRepo(ctx, TaskRepo{TaskID: taskID, Repo: memberRepo, MergeMode: "pr", State: TaskRepoMerge, GatedSHA: "gated-sha", PRURL: prURL}); err != nil {
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
				} else if err := db.UpdateTaskMerge(ctx, taskID, observedURL, ""); err != nil {
					t.Fatal(err)
				}
			}
			observation := PRObservation{ProjectID: project.ID, TaskID: taskID, PRURL: prURL, HeadSHA: "gated-sha", State: "MERGED", MergeCommit: "merge-sha", ObservedAt: 3}
			var accepted bool
			if test.workspace {
				accepted, err = db.RecordMemberPRObservation(ctx, memberRepo, observation, MemberPREffect{RepoState: TaskRepoMerged, MergedRef: "merge-sha"})
			} else {
				accepted, err = db.RecordPRObservation(ctx, observation, PRObservationEffect{TransitionTo: StateMerged, TransitionNote: "merged", MergedRef: "merge-sha"}, task)
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
			wantState := StateMerged
			if test.stale {
				wantState = StateMerging
			}
			if current.State != wantState {
				t.Fatalf("Task state after accepted=%t observation is %q, want %q", accepted, current.State, wantState)
			}
		})
	}
}
