package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestMergedTicketRaisesSpecReadyToDeliverOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		subIssues string
		want      int
	}{
		{name: "every sub-issue closed", subIssues: `[[{"number":11,"state":"closed"},{"number":12,"state":"open"}]]`, want: 1},
		{name: "another sub-issue open", subIssues: `[[{"number":11,"state":"open"},{"number":12,"state":"open"}]]`, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPRMergeFixture(t, "pr", store.StateDone)
			ctx := context.Background()
			brief := "---\ntype: change\ntitle: Ticketed change\ndone_when: commit exists\nticket: 12\n---\nIntent\n"
			if err := os.WriteFile(filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "brief.md"), []byte(brief), 0o600); err != nil {
				t.Fatal(err)
			}
			subIssues := filepath.Join(fixture.root, "sub-issues.json")
			if err := os.WriteFile(subIssues, []byte(tc.subIssues), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("POSSE_TEST_GH_SUB_ISSUES", subIssues)
			if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
				t.Fatal(err)
			}
			fixture.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", strings.Repeat("9", 40), fixture.headSHA)
			cfg, err := config.Load(fixture.home, fixture.project.Name)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
					t.Fatal(err)
				}
			}
			notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := countNoticeKind(notices, "spec_ready_to_deliver"); got != tc.want {
				t.Fatalf("spec_ready_to_deliver Notices = %d, want %d: %#v", got, tc.want, notices)
			}
		})
	}
}

func TestFailedSpecReadyCheckRetriesOnLaterPolls(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	brief := "---\ntype: change\ntitle: Ticketed change\ndone_when: commit exists\nticket: 12\n---\nIntent\n"
	if err := os.WriteFile(filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "brief.md"), []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	subIssues := filepath.Join(fixture.root, "sub-issues.json")
	if err := os.WriteFile(subIssues, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GH_SUB_ISSUES", subIssues)
	if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", strings.Repeat("9", 40), fixture.headSHA)
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	poll := func() {
		t.Helper()
		if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	poll()
	if err := os.WriteFile(subIssues, []byte(`[[{"number":11,"state":"closed"},{"number":12,"state":"open"}]]`), 0o600); err != nil {
		t.Fatal(err)
	}
	poll()
	poll()
	notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := countNoticeKind(notices, "spec_ready_to_deliver"); got != 1 {
		t.Fatalf("spec_ready_to_deliver Notices = %d, want 1 after the failed check was retried: %#v", got, notices)
	}
}

func TestSpecReadyRetrySkipsMergedTaskWithoutPullRequest(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	if err := fixture.db.Transition(ctx, fixture.task.ID, store.StateDone, store.StateMerged, "cli", "merged locally"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	checked, err := fixture.db.SpecReadyChecked(ctx, fixture.task.ID)
	if err != nil || checked {
		t.Fatalf("spec-ready check ran for a merged Task without a pull request: checked=%v err=%v", checked, err)
	}
}
