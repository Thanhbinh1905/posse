package store

import (
	"context"
	"strings"
	"testing"
)

func TestRepoFetchFailuresCoalesceAndResetAfterSuccess(t *testing.T) {
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

	if err := db.RecordRepoFetchSuccess(ctx, project.ID, "alpha", 7); err != nil {
		t.Fatal(err)
	}
	notices, err = db.Notices(ctx, project.ID, true)
	if err != nil || countKind(notices, "root_fetch_failing") != 1 || strings.Contains(notices[0].DataJSON, `"alpha"`) || !strings.Contains(notices[0].DataJSON, `"beta"`) {
		t.Fatalf("partial recovery did not retain only the failing Member in the Notice: %#v, %v", notices, err)
	}
	if err := db.RecordRepoFetchSuccess(ctx, project.ID, "beta", 8); err != nil {
		t.Fatal(err)
	}
	notices, err = db.Notices(ctx, project.ID, true)
	if err != nil || countKind(notices, "root_fetch_failing") != 0 {
		t.Fatalf("complete recovery did not clear the active Notice: %#v, %v", notices, err)
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

func countKind(notices []Notice, kind string) int {
	count := 0
	for _, notice := range notices {
		if notice.Kind == kind {
			count++
		}
	}
	return count
}
