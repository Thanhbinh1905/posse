package app

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestGitLabMergePendingRejectsMovedLiveHead(t *testing.T) {
	f := gitlabFixture(t, store.StateDone)
	const prURL = "https://git.example.com/group/sub/shop/-/merge_requests/17"
	ctx := context.Background()
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET pr_url=?,gated_sha=? WHERE id=?`, prURL, f.headSHA, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, f.task.ID, store.StateDone, store.StateMerging, "cli", "merge requested"); err != nil {
		t.Fatal(err)
	}
	task, err := f.db.TaskByID(ctx, f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.RecordPRObservation(ctx, store.PRObservation{
		ProjectID:  f.project.ID,
		TaskID:     f.task.ID,
		PRURL:      prURL,
		HeadSHA:    f.headSHA,
		State:      "OPEN",
		Checks:     `{"state":"SUCCESS"}`,
		Review:     "APPROVED",
		Mergeable:  "MERGEABLE",
		ObservedAt: time.Now().UnixMilli(),
	}, store.PRObservationEffect{}, task); err != nil {
		t.Fatal(err)
	}
	observation, err := f.db.LatestPRObservation(ctx, f.task.ID)
	if err != nil || observation.HeadSHA != f.headSHA || decodeChecks(observation.Checks).MergeRequested {
		t.Fatalf("pre-merge observation = %+v err=%v", observation, err)
	}
	if err := f.db.StartIntent(ctx, f.project.ID, f.task.ID, "merge", "started", "{}", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	intent, err := f.db.IntentByTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.home, "shop")
	if err != nil {
		t.Fatal(err)
	}
	f.setGitLabState(t, "opened", "mergeable", "success", f.headSHA)
	var output bytes.Buffer
	out := &axi.Context{Context: ctx, Out: &output}
	if err := f.service.mergePullRequest(out, f.db, f.project, cfg, task, "User approved MR", intent); err != nil {
		t.Fatalf("initial merge request: %v", err)
	}
	observation, err = f.db.LatestPRObservation(ctx, f.task.ID)
	if err != nil || !decodeChecks(observation.Checks).MergeRequested {
		t.Fatalf("merge request was not retained: observation=%+v err=%v", observation, err)
	}
	movedHead := strings.Repeat("d", 40)
	f.setGitLabState(t, "opened", "mergeable", "success", movedHead)
	if err := f.service.mergePullRequest(out, f.db, f.project, cfg, task, "User approved MR", intent); err == nil || !strings.Contains(err.Error(), "head moved after its merge head was recorded") {
		t.Fatalf("merge with moved live head = %v; want branch_moved", err)
	}
	task, err = f.db.Task(ctx, f.project.ID, "t1")
	if err != nil || task.State != store.StateDone || task.GatedSHA != "" {
		t.Fatalf("moved head did not reopen Task for gating: task=%+v err=%v", task, err)
	}
	observation, err = f.db.LatestPRObservation(ctx, f.task.ID)
	if err != nil || observation.HeadSHA != f.headSHA || !decodeChecks(observation.Checks).MergeRequested {
		t.Fatalf("retry changed the stored pending observation: observation=%+v err=%v", observation, err)
	}
}
