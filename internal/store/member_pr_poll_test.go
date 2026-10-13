package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMemberPRPollClaimsAreIndependentAndDurable(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	good, err := db.ClaimMemberPRPoll(ctx, project.ID, "good", now, time.Minute, false)
	if err != nil || good == "" {
		t.Fatalf("claim good Member: token=%q err=%v", good, err)
	}
	offline, err := db.ClaimMemberPRPoll(ctx, project.ID, "offline", now, time.Minute, false)
	if err != nil || offline == "" {
		t.Fatalf("claim offline Member while good is active: token=%q err=%v", offline, err)
	}
	if duplicate, err := db.ClaimMemberPRPoll(ctx, project.ID, "good", now, time.Minute, false); err != nil || duplicate != "" {
		t.Fatalf("duplicate good Member claim: token=%q err=%v", duplicate, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, repo := range []string{"good", "offline"} {
		if duplicate, err := db.ClaimMemberPRPoll(ctx, project.ID, repo, now.Add(time.Second), time.Minute, false); err != nil || duplicate != "" {
			t.Fatalf("Member %s claim was not durable across reopen: token=%q err=%v", repo, duplicate, err)
		}
	}
	if err := db.FinishMemberPRPoll(ctx, project.ID, "good", good, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if due, err := db.ClaimMemberPRPoll(ctx, project.ID, "good", now.Add(30*time.Second), time.Minute, false); err != nil || due != "" {
		t.Fatalf("good Member poll ignored its interval: token=%q err=%v", due, err)
	}
	next, err := db.ClaimMemberPRPoll(ctx, project.ID, "good", now.Add(time.Minute+time.Millisecond), time.Minute, false)
	if err != nil || next == "" {
		t.Fatalf("good Member was not independently due: token=%q err=%v", next, err)
	}
	if err := db.FinishMemberPRPoll(ctx, project.ID, "good", good, now.Add(time.Minute).UnixMilli()); err != ErrStateRace {
		t.Fatalf("stale Member poll owner finished successor's claim: %v", err)
	}
	if err := db.FinishMemberPRPoll(ctx, project.ID, "good", next, now.Add(time.Minute+time.Millisecond).UnixMilli()); err != nil {
		t.Fatal(err)
	}
}

func TestPRTaskWatchFailuresRaiseAfterThree(t *testing.T) {
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
	prURL := "https://github.com/acme/shop/pull/1"
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "First PR", LandingMode: "pr", PRURL: prURL})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTaskRepos(ctx, taskID, []TaskRepo{{Repo: "api", LandingMode: "pr"}, {Repo: "worker", LandingMode: "pr"}}); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"api", "worker"} {
		if err := db.UpdateTaskRepo(ctx, TaskRepo{TaskID: taskID, Repo: repo, LandingMode: "pr", State: TaskRepoOpen, PRURL: prURL}); err != nil {
			t.Fatal(err)
		}
	}
	summary := "First PR pull request watch failed three times: forge CLI timed out"
	for attempt := 1; attempt <= 3; attempt++ {
		failures, err := db.RecordPRTaskWatchFailure(ctx, project.ID, taskID, "", prURL, int64(attempt), "forge CLI timed out", summary)
		if err != nil || failures != attempt {
			t.Fatalf("PR failure %d: count=%d err=%v", attempt, failures, err)
		}
		notices, err := db.Notices(ctx, project.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		wantNotices := 0
		if attempt == 3 {
			wantNotices = 1
		}
		if len(notices) != wantNotices {
			t.Fatalf("PR failure %d created %d Notices, want %d: %#v", attempt, len(notices), wantNotices, notices)
		}
	}
	var data string
	if err := db.QueryRowContext(ctx, `SELECT data_json FROM notices WHERE project_id=? AND task_id=? AND kind='pr_watch_failing'`, project.ID, taskID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data, `"consecutive_failures":3`) {
		t.Fatalf("threshold Notice omitted failure count: %s", data)
	}
	for _, repo := range []string{"api", "worker"} {
		if failures, err := db.RecordPRTaskWatchFailure(ctx, project.ID, taskID, repo, prURL, 6, "forge CLI timed out", "Member PR watch failed three times"); err != nil || failures != 1 {
			t.Fatalf("independent %s Member failure count=%d err=%v, want 1", repo, failures, err)
		}
	}
}

func TestPRTaskWatchFailureIgnoresStalePRURL(t *testing.T) {
	for _, test := range []struct {
		name string
		repo string
	}{
		{name: "Task"},
		{name: "Member", repo: "api"},
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
			oldURL := "https://github.com/acme/shop/pull/1"
			newURL := "https://github.com/acme/shop/pull/2"
			taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "First PR", LandingMode: "pr", PRURL: oldURL})
			if err != nil {
				t.Fatal(err)
			}
			if test.repo != "" {
				if err := db.CreateTaskRepos(ctx, taskID, []TaskRepo{{Repo: test.repo, LandingMode: "pr"}}); err != nil {
					t.Fatal(err)
				}
				if err := db.UpdateTaskRepo(ctx, TaskRepo{TaskID: taskID, Repo: test.repo, LandingMode: "pr", State: TaskRepoOpen, PRURL: oldURL}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.RecordPRTaskWatchFailure(ctx, project.ID, taskID, test.repo, oldURL, 1, "offline", "watch failed"); err != nil {
				t.Fatal(err)
			}
			if test.repo == "" {
				if err := db.UpdateTaskLanding(ctx, taskID, newURL, ""); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := db.UpdateTaskRepo(ctx, TaskRepo{TaskID: taskID, Repo: test.repo, LandingMode: "pr", State: TaskRepoOpen, PRURL: newURL}); err != nil {
					t.Fatal(err)
				}
			}
			if failures, err := db.RecordPRTaskWatchFailure(ctx, project.ID, taskID, test.repo, oldURL, 2, "stale response", "watch failed"); err != nil || failures != 0 {
				t.Fatalf("stale failure write returned count=%d err=%v, want no write", failures, err)
			}
			var failures int
			if err := db.QueryRowContext(ctx, `SELECT consecutive_failures FROM pr_task_watch_failures WHERE project_id=? AND task_id=? AND repo=?`, project.ID, taskID, test.repo).Scan(&failures); err != nil || failures != 1 {
				t.Fatalf("stale response changed failure count to %d: %v", failures, err)
			}
			notices, err := db.Notices(ctx, project.ID, false)
			if err != nil || len(notices) != 0 {
				t.Fatalf("stale response created Notices: %#v, %v", notices, err)
			}
		})
	}
}

func TestMemberPRPollReleaseClearsOnlyTheMatchingClaim(t *testing.T) {
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
	now := time.Now()
	first, err := db.ClaimMemberPRPoll(ctx, project.ID, "api", now, time.Minute, false)
	if err != nil || first == "" {
		t.Fatalf("claim api: token=%q err=%v", first, err)
	}
	if err := db.ReleaseMemberPRPoll(ctx, project.ID, "api", first); err != nil {
		t.Fatal(err)
	}
	second, err := db.ClaimMemberPRPoll(ctx, project.ID, "api", now, time.Minute, false)
	if err != nil || second == "" {
		t.Fatalf("reclaim api after release: token=%q err=%v", second, err)
	}
	if err := db.ReleaseMemberPRPoll(ctx, project.ID, "api", first); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := db.ClaimMemberPRPoll(ctx, project.ID, "api", now, time.Minute, false); err != nil || duplicate != "" {
		t.Fatalf("stale release cleared successor claim: token=%q err=%v", duplicate, err)
	}
	if err := db.ReleaseMemberPRPoll(ctx, project.ID, "api", second); err != nil {
		t.Fatal(err)
	}
}
