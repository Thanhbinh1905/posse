package app

import (
	"context"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
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
