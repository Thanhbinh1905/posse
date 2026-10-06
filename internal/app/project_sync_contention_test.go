package app

import (
	"context"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
	"golang.org/x/sys/unix"
)

func TestLocalFetchContentionDoesNotInventRootBehindNotice(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	owner, err := acquireRepositoryFetchLock(context.Background(), fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	if !store.IsBusy(err) {
		t.Fatalf("fetch lock contention = %+v, %v; want retryable contention", result, err)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if countNoticeKind(notices, "root_behind") != 0 {
		t.Fatalf("local contention invented a root-behind warning: %#v", notices)
	}
}

func TestPollOnlyLookoutDefersFetchLockContentionWithoutNotice(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	project, err := fixture.db.ProjectByID(context.Background(), fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	fake := fixture.service.Herdr.(*herdr.Fake)
	paneID := "w1:p9"
	fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{
		PaneID: paneID, WorkspaceID: project.HerdrWorkspaceID, Label: lookoutTabLabel(project),
	})
	t.Setenv("HERDR_PANE_ID", paneID)

	owner, err := acquireRepositoryFetchLock(context.Background(), fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	defer unix.Flock(int(owner.Fd()), unix.LOCK_UN)

	if code, output, errOutput := fixture.run("lookout", "--poll-only", "--timeout", "1"); code != 0 {
		t.Fatalf("poll-only Lookout exited during expected fetch contention: code=%d output=%s error=%s", code, output, errOutput)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if countNoticeKind(notices, "pr_watch_failing") != 0 {
		t.Fatalf("transient fetch contention raised a Notice: %#v", notices)
	}
}

func TestRepositorySyncSkipsAnOverlappingSameTarget(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	target := repoTarget{Root: fixture.repo, DefaultBranch: fixture.project.DefaultBranch}
	key := repositorySyncKey(fixture.project, target)
	if !fixture.service.beginMaintenance(key) {
		t.Fatal("could not reserve repository sync target")
	}
	defer fixture.service.endMaintenance(key)
	result, err := fixture.service.syncRepository(context.Background(), fixture.db, fixture.project, target, time.Now())
	if err != nil || result.Status != "skipped" {
		t.Fatalf("overlapping repository sync = %+v, %v; want skipped", result, err)
	}
}

func TestRepositorySyncSkipsAnInterprocessOverlap(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	owner, err := acquireRepositorySyncLock(context.Background(), fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	defer unix.Flock(int(owner.Fd()), unix.LOCK_UN)
	target := repoTarget{Root: fixture.repo, DefaultBranch: fixture.project.DefaultBranch}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	result, err := fixture.service.syncRepository(ctx, fixture.db, fixture.project, target, time.Now())
	if err != nil || result.Status != "skipped" {
		t.Fatalf("interprocess repository overlap = %+v, %v; want skipped", result, err)
	}
}
