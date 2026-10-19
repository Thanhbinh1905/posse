package store

import (
	"context"
	"strings"
	"testing"
)

func TestRepoFetchFailuresCoalesceAndRecoverOnce(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateWorkspaceProject(ctx, "stack", t.TempDir(), []ProjectRepo{
		{Name: "alpha", Path: "alpha", DefaultBranch: "main", Status: RepoActive},
		{Name: "beta", Path: "beta", DefaultBranch: "main", Status: RepoActive},
	})
	if err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if failures, err := db.RecordRepoFetchFailure(ctx, project.ID, "alpha", int64(attempt), "fetch timed out"); err != nil || failures != attempt {
			t.Fatalf("alpha failure %d = %d, %v", attempt, failures, err)
		}
	}
	if notices, err := db.Notices(ctx, project.ID, true); err != nil || countKind(notices, "root_fetch_failing") != 0 {
		t.Fatalf("fetch Notice appeared before the threshold: %#v, %v", notices, err)
	}
	if _, err := db.RecordRepoFetchFailure(ctx, project.ID, "alpha", 3, "alpha timed out"); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := db.RecordRepoFetchFailure(ctx, project.ID, "beta", int64(attempt+3), "beta timed out"); err != nil {
			t.Fatal(err)
		}
	}
	notices, err := db.Notices(ctx, project.ID, true)
	if err != nil || countKind(notices, "root_fetch_failing") != 1 || countKind(notices, "root_behind") != 0 {
		t.Fatalf("persistent failures did not produce one distinct coalesced Notice: %#v, %v", notices, err)
	}
	if !strings.Contains(notices[0].DataJSON, `"alpha"`) || !strings.Contains(notices[0].DataJSON, `"beta"`) {
		t.Fatalf("coalesced Notice does not include both Members: %#v", notices[0])
	}
	failureNoticeID := notices[0].ID

	if err := db.RecordRepoFetchSuccess(ctx, project.ID, "alpha", 7); err != nil {
		t.Fatal(err)
	}
	notices, err = db.Notices(ctx, project.ID, true)
	if err != nil || countKind(notices, "root_fetch_failing") != 1 || notices[0].ID != failureNoticeID || strings.Contains(notices[0].DataJSON, `"alpha"`) || !strings.Contains(notices[0].DataJSON, `"beta"`) {
		t.Fatalf("partial recovery did not update the existing Notice for the failing Member: %#v, %v", notices, err)
	}
	if err := db.RecordRepoFetchSuccess(ctx, project.ID, "beta", 8); err != nil {
		t.Fatal(err)
	}
	notices, err = db.Notices(ctx, project.ID, true)
	if err != nil || countKind(notices, "root_fetch_failing") != 0 {
		t.Fatalf("complete recovery did not clear the active Notice: %#v, %v", notices, err)
	}
	allNotices, err := db.Notices(ctx, project.ID, false)
	if err != nil || countKind(allNotices, "root_fetch_recovered") != 1 {
		t.Fatalf("complete recovery did not create exactly one recovery Notice: %#v, %v", allNotices, err)
	}

	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := db.RecordRepoFetchFailure(ctx, project.ID, "alpha", int64(attempt+8), "fetch timed out again"); err != nil {
			t.Fatal(err)
		}
	}
	notices, err = db.Notices(ctx, project.ID, true)
	if err != nil || countKind(notices, "root_fetch_failing") != 1 {
		t.Fatalf("a new failure streak did not create a new active Notice: %#v, %v", notices, err)
	}
	allNotices, err = db.Notices(ctx, project.ID, false)
	if err != nil || countKind(allNotices, "root_fetch_recovered") != 1 {
		t.Fatalf("a later failure created an extra recovery Notice: %#v, %v", allNotices, err)
	}
	var alphaFailures, betaFailures int
	if err := db.QueryRowContext(ctx, `SELECT consecutive_failures FROM repo_fetch_failures WHERE project_id=? AND repo='alpha'`, project.ID).Scan(&alphaFailures); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT consecutive_failures FROM repo_fetch_failures WHERE project_id=? AND repo='beta'`, project.ID).Scan(&betaFailures); err != nil {
		t.Fatal(err)
	}
	if alphaFailures != 3 || betaFailures != 0 {
		t.Fatalf("failure streaks after recovery and recurrence = alpha:%d beta:%d", alphaFailures, betaFailures)
	}
}

func TestRepoFetchAcknowledgementDoesNotRestartOutage(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateWorkspaceProject(ctx, "stack", t.TempDir(), []ProjectRepo{
		{Name: "alpha", Path: "alpha", DefaultBranch: "main", Status: RepoActive},
		{Name: "beta", Path: "beta", DefaultBranch: "main", Status: RepoActive},
	})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := db.RecordRepoFetchFailure(ctx, project.ID, "alpha", int64(attempt), "timeout"); err != nil {
			t.Fatal(err)
		}
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || countKind(notices, "root_fetch_failing") != 1 {
		t.Fatalf("persistent fetch failure did not create one Notice: %#v, %v", notices, err)
	}
	failureID := notices[0].ID
	if _, err := db.ExecContext(ctx, `UPDATE notices SET acked_at=? WHERE id=?`, 4, failureID); err != nil {
		t.Fatal(err)
	}
	for attempt := 4; attempt <= 6; attempt++ {
		if _, err := db.RecordRepoFetchFailure(ctx, project.ID, "alpha", int64(attempt), "connection reset"); err != nil {
			t.Fatal(err)
		}
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := db.RecordRepoFetchFailure(ctx, project.ID, "beta", int64(attempt+6), "connection reset"); err != nil {
			t.Fatal(err)
		}
	}
	notices, err = db.Notices(ctx, project.ID, false)
	if err != nil || countKind(notices, "root_fetch_failing") != 1 || notices[0].ID != failureID || !strings.Contains(notices[0].DataJSON, `"beta"`) {
		t.Fatalf("acknowledged outage was re-raised instead of updated in place: %#v, %v", notices, err)
	}
	if err := db.RecordRepoFetchSuccess(ctx, project.ID, "alpha", 10); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRepoFetchSuccess(ctx, project.ID, "beta", 11); err != nil {
		t.Fatal(err)
	}
	notices, err = db.Notices(ctx, project.ID, false)
	if err != nil || countKind(notices, "root_fetch_failing") != 1 || countKind(notices, "root_fetch_recovered") != 1 {
		t.Fatalf("recovery after an acknowledged outage was not recorded once: %#v, %v", notices, err)
	}
}

func countKind(notices []Notice, kind string) int {
	count := 0
	for _, notice := range notices {
		if notice.Kind == kind {
			count++
		}
	}
	return count
}
