package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestAskAndDecideThroughCLIWithUserOnlyBoundary(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "home")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 1, Type: "ship", Title: "Fix things", LandingMode: "local", AutonomyLand: "ask", PaneID: "w1:p2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), id, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	service := testService(home, herdr.NewFake())
	cli := service.CLI()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cli.Out, cli.ErrOut = out, errOut
	t.Chdir(repo)
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	run := func(want int, args ...string) string {
		t.Helper()
		out.Reset()
		errOut.Reset()
		code := cli.Run(args)
		result := out.String() + errOut.String()
		if code != want {
			t.Fatalf("%v: code=%d want=%d output=%s", args, code, want, result)
		}
		return result
	}
	if got := run(0, "ask", "t1", "Should we retry?", "--option", "retry", "--option=wait"); !strings.Contains(got, "Should we retry?") {
		t.Fatal(got)
	}
	items, err := db.Decisions(context.Background(), project.ID, true)
	if err != nil || len(items) != 1 {
		t.Fatalf("Decisions: %#v %v", items, err)
	}
	decisionID := fmt.Sprint(items[0].ID)
	if got := run(1, "decide", decisionID, "retry"); !strings.Contains(got, "user_only") {
		t.Fatal(got)
	}
	if got := run(1, "decide", decisionID, "wrong", "--user-approved", "User says retry"); !strings.Contains(got, "decision_refused") {
		t.Fatal(got)
	}
	if got := run(0, "decide", decisionID, "retry", "--user-approved", "User says retry"); !strings.Contains(got, "User says retry") {
		t.Fatal(got)
	}
	if got := run(1, "decide", decisionID, "wait", "--user-approved", "User changed mind"); !strings.Contains(got, "already answered") {
		t.Fatal(got)
	}
	if got := run(0, "decisions", "--all"); !strings.Contains(got, "User says retry") {
		t.Fatal(got)
	}
	if got := run(0, "decisions"); !strings.Contains(got, "decisions: []") {
		t.Fatal(got)
	}
	t.Setenv("HERDR_PANE_ID", "w1:p2")
	if got := run(1, "decide", decisionID, "wait"); !strings.Contains(got, "worker_forbidden") {
		t.Fatal(got)
	}
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	if got := run(0, "ask", "t1", "One more?", "--option", "yes", "--option", "no"); !strings.Contains(got, "One more?") {
		t.Fatal(got)
	}
	t.Setenv("HERDR_PANE_ID", "")
	items, err = db.Decisions(context.Background(), project.ID, true)
	if err != nil || len(items) != 1 {
		t.Fatalf("pending: %#v %v", items, err)
	}
	if got := run(0, "decide", fmt.Sprint(items[0].ID), "yes"); !strings.Contains(got, "yes") {
		t.Fatal(got)
	}
}

func TestNoticeSourcesRaiseDecisionsOnce(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "home")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	create := func(seq int, kind string, state store.State, reviewOf int64) store.Task {
		t.Helper()
		id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: seq, Type: kind, ReviewsTaskID: reviewOf, Title: fmt.Sprintf("Task %d", seq), LandingMode: "local", AutonomyLand: "ask", AutonomyReview: "ask"})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Transition(ctx, id, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
			t.Fatal(err)
		}
		if state == store.StateLost {
			if err := db.Transition(ctx, id, store.StateWorking, state, "cli", "lost"); err != nil {
				t.Fatal(err)
			}
		} else if state == store.StateFailed {
			if err := db.Transition(ctx, id, store.StateWorking, state, "worker", "failed"); err != nil {
				t.Fatal(err)
			}
		} else if state == store.StateLanding {
			if err := db.Transition(ctx, id, store.StateWorking, store.StateDone, "worker", "done"); err != nil {
				t.Fatal(err)
			}
			if err := db.Transition(ctx, id, store.StateDone, state, "cli", "landing"); err != nil {
				t.Fatal(err)
			}
		} else if state == store.StateReported {
			if err := db.Transition(ctx, id, store.StateWorking, store.StateDone, "worker", "done"); err != nil {
				t.Fatal(err)
			}
			if err := db.Transition(ctx, id, store.StateDone, state, "cli", "reported"); err != nil {
				t.Fatal(err)
			}
		}
		task, err := db.TaskByID(ctx, project.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	ship := create(1, "ship", store.StateLanding, 0)
	lost := create(2, "ship", store.StateLost, 0)
	failed := create(3, "ship", store.StateFailed, 0)
	review := create(4, "review", store.StateReported, ship.ID)
	findings := create(5, "ship", store.StateWorking, 0)
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET landing_mode='no-mistakes' WHERE id=?`, findings.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, findings.ID, store.StateWorking, store.StateNeedsDecision, "worker", "Review findings"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "projects", project.Name, "tasks", "t5", "findings.toon")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("findings"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		task store.Task
		kind string
	}{{ship, "land_ready"}, {lost, "task_lost"}, {failed, "task_failed"}, {review, "task_done"}, {findings, "needs_decision"}} {
		if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: item.task.ID, Kind: item.kind, Summary: item.task.Title}); err != nil {
			t.Fatal(err)
		}
	}
	// This ship task_done Notice produces no Decision, including after ack.
	ignoredID, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: ship.ID, Kind: "task_done", Summary: "Ship Task done"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AckNotices(ctx, project.ID, []string{fmt.Sprint(ignoredID)}); err != nil {
		t.Fatal(err)
	}
	service := testService(home, herdr.NewFake())
	for i := 0; i < 2; i++ {
		if err := service.raiseNoticeDecisions(ctx, db, project); err != nil {
			t.Fatal(err)
		}
	}
	items, err := db.Decisions(ctx, project.ID, true)
	if err != nil || len(items) != 5 {
		t.Fatalf("raised Decisions: %#v %v", items, err)
	}
	want := [][]string{{"land", "wait"}, {"relaunch", "discard"}, {"relaunch", "discard"}, {"accept", "request-changes", "ignore"}, {"accept", "request-changes", "ignore"}}
	wantKinds := []string{"land_ready", "recovery", "recovery", "review", "review"}
	for i, d := range items {
		if d.Kind != wantKinds[i] || !strings.HasPrefix(d.Origin, "notice:") {
			t.Fatalf("Decision origin or kind not recorded explicitly: %#v", d)
		}
		if strings.Join(d.Options, ",") != strings.Join(want[i], ",") {
			t.Fatalf("options for %s: %#v", d.Origin, d.Options)
		}
	}
	// All five Notices were evaluated, including the non-Decision kinds. A
	// second pass must not fetch historical Notices or load their Tasks.
	unseen, err := db.DecisionSourceNotices(ctx, project.ID)
	if err != nil || len(unseen) != 0 {
		t.Fatalf("rescanned evaluated Notices: %#v %v", unseen, err)
	}
	if err := db.Transition(ctx, ship.ID, store.StateLanding, store.StateLanded, "cli", "merged"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, lost.ID, store.StateLost, store.StateWorking, "cli", "relaunched"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, failed.ID, store.StateFailed, store.StateTornDown, "user", "discarded"); err == nil {
		t.Fatal("discard without approval unexpectedly succeeded")
	}
	if err := service.raiseNoticeDecisions(ctx, db, project); err != nil {
		t.Fatal(err)
	}
	pending, err := db.Decisions(ctx, project.ID, true)
	if err != nil || len(pending) != 3 {
		t.Fatalf("resolved Decisions stayed pending: %#v %v", pending, err)
	}
	all, err := db.Decisions(ctx, project.ID, false)
	if err != nil || all[0].ObsoleteReason != "Task left landing" || all[1].ObsoleteReason != "Task no longer failed or lost" || all[0].Answer != "" {
		t.Fatalf("resolved Decisions were not marked obsolete: %#v %v", all, err)
	}
	// Repeated failure Notices still produce at most one pending recovery
	// Decision for this Task.
	for i := 0; i < 2; i++ {
		if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: failed.ID, Kind: "task_failed", Summary: "Repeated failure"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.raiseNoticeDecisions(ctx, db, project); err != nil {
		t.Fatal(err)
	}
	pending, err = db.Decisions(ctx, project.ID, true)
	if err != nil || len(pending) != 3 {
		t.Fatalf("repeated failure duplicated pending recovery Decision: %#v %v", pending, err)
	}
	if err := db.TransitionWithApproval(ctx, failed.ID, store.StateFailed, store.StateTornDown, "user", "discarded", "discard", "User requested discard"); err != nil {
		t.Fatal(err)
	}
	if err := service.raiseNoticeDecisions(ctx, db, project); err != nil {
		t.Fatal(err)
	}
	pending, err = db.Decisions(ctx, project.ID, true)
	if err != nil || len(pending) != 2 {
		t.Fatalf("torn-down Task still has pending Decision: %#v %v", pending, err)
	}
}
