package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWorkspaceGitLabMergePendingRejectsMovedLiveHead(t *testing.T) {
	f := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"pr\"\n\n[repositories.worker]\nforge = \"gitlab\"\n", []string{"worker"}, []string{"worker"})
	ctx := context.Background()
	repo := f.repos()["worker"]
	gitTest(t, repo.WorktreePath, "remote", "set-url", "origin", "https://git.example.com/group/worker.git")
	head := strings.TrimSpace(gitTest(t, repo.WorktreePath, "rev-parse", "HEAD"))
	const prURL = "https://git.example.com/group/worker/-/merge_requests/17"
	if _, err := f.db.ExecContext(ctx, `UPDATE task_repos SET landing_mode='pr',state=?,gated_sha=?,pr_url=?,target_branch='main' WHERE task_id=? AND repo='worker'`, store.TaskRepoMerge, head, prURL, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, f.task.ID, store.StateDone, store.StateMerging, "cli", "merge requested"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.RecordMemberPRObservation(ctx, "worker", store.PRObservation{
		ProjectID:  f.project.ID,
		TaskID:     f.task.ID,
		PRURL:      prURL,
		HeadSHA:    head,
		State:      "OPEN",
		Checks:     `{"state":"SUCCESS"}`,
		Review:     "APPROVED",
		Mergeable:  "MERGEABLE",
		ObservedAt: time.Now().UnixMilli(),
	}, store.MemberPREffect{}); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(f.root, "glab-state.json")
	writeMR := func(sha string) {
		t.Helper()
		mr, err := json.Marshal(map[string]any{
			"web_url": prURL, "state": "opened", "sha": sha,
			"source_branch": "posse/t1", "target_branch": "main",
			"source_project_id": 7, "target_project_id": 7,
			"detailed_merge_status": "mergeable",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statePath, mr, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(f.root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "glab"), []byte("#!/bin/sh\ncat \"$POSSE_TEST_GLAB_STATE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GLAB_STATE", statePath)
	writeMR(head)
	memberTask, err := f.db.TaskByID(ctx, f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	members, err := workspaceMembers(ctx, f.db, f.project, memberTask)
	if err != nil || len(members) != 1 {
		t.Fatalf("workspace members = %#v err=%v", members, err)
	}
	if err := f.db.StartIntent(ctx, f.project.ID, f.task.ID, "merge", "started", "{}", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	intent, err := f.db.IntentByTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.mergeMemberPullRequest(ctx, f.db, f.project, f.config(), memberTask, intent, members[0]); err != nil {
		t.Fatalf("initial member merge request: %v", err)
	}
	observation, err := f.db.LatestMemberPRObservation(ctx, f.task.ID, "worker")
	if err != nil || !decodeChecks(observation.Checks).MergeRequested {
		t.Fatalf("member merge request was not retained: observation=%+v err=%v", observation, err)
	}
	writeMR(strings.Repeat("d", 40))
	if err := f.service.mergeMemberPullRequest(ctx, f.db, f.project, f.config(), memberTask, intent, members[0]); err == nil || !strings.Contains(err.Error(), "head moved after its merge head was recorded") {
		t.Fatalf("merge with moved live member head = %v; want branch_moved", err)
	}
	current, err := f.db.TaskByID(ctx, f.project.ID, f.task.ID)
	if err != nil || current.State != store.StateDone {
		t.Fatalf("moved member head did not reopen Task: task=%+v err=%v", current, err)
	}
	observation, err = f.db.LatestMemberPRObservation(ctx, f.task.ID, "worker")
	if err != nil || observation.HeadSHA != head || !decodeChecks(observation.Checks).MergeRequested {
		t.Fatalf("retry changed the stored pending member observation: observation=%+v err=%v", observation, err)
	}
}
