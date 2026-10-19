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

func TestPRPollFailureNoticeUpdatesAndRecoversOnce(t *testing.T) {
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
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := db.RecordPRPoll(ctx, project.ID, int64(attempt), "forge timed out"); err != nil {
			t.Fatal(err)
		}
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || countKind(notices, "pr_watch_failing") != 1 {
		t.Fatalf("persistent project poll failure did not create one Notice: %#v, %v", notices, err)
	}
	failureID := notices[0].ID
	if _, err := db.ExecContext(ctx, `UPDATE notices SET acked_at=? WHERE id=?`, 4, failureID); err != nil {
		t.Fatal(err)
	}
	for attempt := 4; attempt <= 6; attempt++ {
		if _, err := db.RecordPRPoll(ctx, project.ID, int64(attempt), "connection reset"); err != nil {
			t.Fatal(err)
		}
	}
	notices, err = db.Notices(ctx, project.ID, false)
	if err != nil || countKind(notices, "pr_watch_failing") != 1 || notices[0].ID != failureID || !strings.Contains(notices[0].DataJSON, "connection reset") {
		t.Fatalf("persistent project failure did not update its Notice in place: %#v, %v", notices, err)
	}
	if err := db.RecordPRPollAttempt(ctx, project.ID, 7); err != nil {
		t.Fatal(err)
	}
	state, err := db.ProjectWatchState(ctx, project.ID)
	if err != nil || state.PRConsecutiveFailures != 6 {
		t.Fatalf("poll without a successful forge observation cleared the failure episode: %+v, %v", state, err)
	}
	if _, err := db.RecordPRPoll(ctx, project.ID, 8, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordPRPoll(ctx, project.ID, 9, ""); err != nil {
		t.Fatal(err)
	}
	notices, err = db.Notices(ctx, project.ID, false)
	if err != nil || countKind(notices, "pr_watch_recovered") != 1 || countKind(notices, "pr_watch_failing") != 1 {
		t.Fatalf("successful poll did not create exactly one recovery Notice: %#v, %v", notices, err)
	}
	for _, notice := range notices {
		if notice.ID == failureID && notice.AckedAt == 0 {
			t.Fatalf("recovered project failure Notice is still active: %#v", notice)
		}
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := db.RecordPRPoll(ctx, project.ID, int64(attempt+9), "new outage"); err != nil {
			t.Fatal(err)
		}
	}
	notices, err = db.Notices(ctx, project.ID, false)
	if err != nil || countKind(notices, "pr_watch_failing") != 2 || countKind(notices, "pr_watch_recovered") != 1 {
		t.Fatalf("new project outage did not start a distinct episode: %#v, %v", notices, err)
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "First PR", MergeMode: "pr", PRURL: prURL})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTaskRepos(ctx, taskID, []TaskRepo{{Repo: "api", MergeMode: "pr"}, {Repo: "worker", MergeMode: "pr"}}); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"api", "worker"} {
		if err := db.UpdateTaskRepo(ctx, TaskRepo{TaskID: taskID, Repo: repo, MergeMode: "pr", State: TaskRepoOpen, PRURL: prURL}); err != nil {
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

func TestPRTaskWatchFailureNoticeUpdatesInPlaceAndRecoversOnce(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "First PR", MergeMode: "pr", PRURL: prURL})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := db.RecordPRTaskWatchFailure(ctx, project.ID, taskID, "", prURL, int64(attempt), "timeout", "PR watch failed three times: timeout"); err != nil {
			t.Fatal(err)
		}
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || countKind(notices, "pr_watch_failing") != 1 {
		t.Fatalf("persistent failure did not create one Notice: %#v, %v", notices, err)
	}
	failureID := notices[0].ID
	if _, err := db.ExecContext(ctx, `UPDATE notices SET acked_at=? WHERE id=?`, 4, failureID); err != nil {
		t.Fatal(err)
	}
	for attempt := 4; attempt <= 6; attempt++ {
		if _, err := db.RecordPRTaskWatchFailure(ctx, project.ID, taskID, "", prURL, int64(attempt), "connection refused", "PR watch failed three times: connection refused"); err != nil {
			t.Fatal(err)
		}
	}
	notices, err = db.Notices(ctx, project.ID, false)
	if err != nil || countKind(notices, "pr_watch_failing") != 1 || notices[0].ID != failureID || !strings.Contains(notices[0].DataJSON, "connection refused") {
		t.Fatalf("persistent failure did not update its Notice in place: %#v, %v", notices, err)
	}

	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	observation := PRObservation{ProjectID: project.ID, TaskID: taskID, PRURL: prURL, State: "OPEN", ObservedAt: 7}
	if _, err := db.RecordPRObservation(ctx, observation, PRObservationEffect{}, task); err != nil {
		t.Fatal(err)
	}
	observation.ObservedAt++
	if _, err := db.RecordPRObservation(ctx, observation, PRObservationEffect{}, task); err != nil {
		t.Fatal(err)
	}
	notices, err = db.Notices(ctx, project.ID, false)
	if err != nil || countKind(notices, "pr_watch_recovered") != 1 || countKind(notices, "pr_watch_failing") != 1 {
		t.Fatalf("successful poll did not create exactly one recovery Notice: %#v, %v", notices, err)
	}
	for _, notice := range notices {
		if notice.ID == failureID && notice.AckedAt == 0 {
			t.Fatalf("recovered failure Notice is still active: %#v", notice)
		}
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := db.RecordPRTaskWatchFailure(ctx, project.ID, taskID, "", prURL, int64(attempt+8), "new outage", "PR watch failed three times: new outage"); err != nil {
			t.Fatal(err)
		}
	}
	notices, err = db.Notices(ctx, project.ID, false)
	if err != nil || countKind(notices, "pr_watch_failing") != 2 || countKind(notices, "pr_watch_recovered") != 1 {
		t.Fatalf("new outage did not start a distinct failure episode: %#v, %v", notices, err)
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
			taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "First PR", MergeMode: "pr", PRURL: oldURL})
			if err != nil {
				t.Fatal(err)
			}
			if test.repo != "" {
				if err := db.CreateTaskRepos(ctx, taskID, []TaskRepo{{Repo: test.repo, MergeMode: "pr"}}); err != nil {
					t.Fatal(err)
				}
				if err := db.UpdateTaskRepo(ctx, TaskRepo{TaskID: taskID, Repo: test.repo, MergeMode: "pr", State: TaskRepoOpen, PRURL: oldURL}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.RecordPRTaskWatchFailure(ctx, project.ID, taskID, test.repo, oldURL, 1, "offline", "watch failed"); err != nil {
				t.Fatal(err)
			}
			if test.repo == "" {
				if err := db.UpdateTaskMerge(ctx, taskID, newURL, ""); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := db.UpdateTaskRepo(ctx, TaskRepo{TaskID: taskID, Repo: test.repo, MergeMode: "pr", State: TaskRepoOpen, PRURL: newURL}); err != nil {
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
