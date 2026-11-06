package app

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestStartAdoptionForgeAndHeadRefusalsHaveNoEffects(t *testing.T) {
	for _, scenario := range []string{"merged", "closed", "expected head", "remote head", "PR head", "live executor", "unanswered Decision"} {
		t.Run(scenario, func(t *testing.T) {
			f, brief := newTaskAdoptionFixture(t)
			ctx := context.Background()
			repository, err := gitCommonDir(ctx, f.repo)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "live executor":
				f.service.Herdr.(*herdr.Fake).SnapshotValue.Panes = append(f.service.Herdr.(*herdr.Fake).SnapshotValue.Panes, herdr.Pane{PaneID: f.task.PaneID, WorkspaceID: f.task.HerdrWorkspaceID, Label: f.task.PaneLabel, Agent: "claude", AgentStatus: "idle"})
			case "unanswered Decision":
				if _, err := f.db.ExecContext(ctx, `INSERT INTO decisions(project_id,task_id,origin,question,options_json,created_at) VALUES(?,?,'custody','Keep custody?','[]',1)`, f.project.ID, f.task.ID); err != nil {
					t.Fatal(err)
				}
			case "merged":
				t.Setenv("POSSE_TEST_GH_VIEW_STATE", "MERGED")
			case "closed":
				t.Setenv("POSSE_TEST_GH_VIEW_STATE", "CLOSED")
			case "expected head":
				if _, err := f.db.ExecContext(ctx, `UPDATE branch_authorship SET head_sha=?`, strings.Repeat("2", 40)); err != nil {
					t.Fatal(err)
				}
			case "remote head":
				gitTest(t, f.remote, "update-ref", "refs/heads/posse/t1", strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "main")))
			case "PR head":
				f.setGHHead(t, strings.Repeat("2", 40))
			}
			before, err := f.db.BranchAuthor(ctx, repository, "posse/t1")
			if err != nil {
				t.Fatal(err)
			}
			refs := gitTest(t, f.repo, "show-ref")
			code, out, stderr := f.run("start", "--brief", brief, "--profile", "maintenance")
			if code == 0 || !strings.Contains(out+stderr, "adoption_refused") {
				t.Fatalf("refusal exit=%d out=%s stderr=%s", code, out, stderr)
			}
			after, err := f.db.BranchAuthor(ctx, repository, "posse/t1")
			if err != nil || after != before {
				t.Fatalf("claim changed: %+v -> %+v, %v", before, after, err)
			}
			if got := gitTest(t, f.repo, "show-ref"); got != refs {
				t.Fatalf("refs changed: %s", got)
			}
			var count int
			if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("Task count=%d err=%v", count, err)
			}
			sandbox, err := f.db.SandboxByTask(ctx, f.task.ID)
			if err != nil || sandbox.Path != f.worktree {
				t.Fatalf("Sandbox changed: %+v %v", sandbox, err)
			}
			for _, call := range f.service.Herdr.(*herdr.Fake).Calls {
				if call.Method == "tab.new" || call.Method == "agent.start" {
					t.Fatalf("refusal created Worker: %+v", call)
				}
			}
			calls, err := os.ReadFile(f.ghLog)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if strings.Contains(string(calls), "pr edit") || strings.Contains(string(calls), "pr create") {
				t.Fatalf("refusal wrote forge: %s", calls)
			}
		})
	}
}

func TestAdoptingTaskPublishReplacesSourceToken(t *testing.T) {
	f, brief := newTaskAdoptionFixture(t)
	ctx := context.Background()
	const pr = "https://github.com/acme/shop/pull/17"
	oldToken, err := f.db.EnsurePRBodyMarker(ctx, f.task.ID, "", pr)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GH_BODY", "Human note\n\n"+managedPublishBody("## Summary\n\nOriginal", oldToken))
	if err := os.WriteFile(f.ghOpenPRs, []byte(`[{"url":"`+pr+`","headRefName":"posse/t1","headRefOid":"`+f.headSHA+`","baseRefName":"main","headRepository":{"nameWithOwner":"acme/shop"}}]`), 0600); err != nil {
		t.Fatal(err)
	}
	if code, out, stderr := f.run("start", "--brief", brief, "--profile", "maintenance"); code != 0 {
		t.Fatalf("adopt: %d %s %s", code, out, stderr)
	}
	next, err := f.db.Task(ctx, f.project.ID, "t2")
	if err != nil {
		t.Fatal(err)
	}
	original := f.task
	f.task = next
	removeFixtureTaskPane(f)
	f.task = original
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Continue again\ndone_when: verified\nadopt: t2\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, out, stderr := f.run("start", "--brief", brief, "--profile", "maintenance"); code != 0 {
		t.Fatalf("second adoption: %d %s %s", code, out, stderr)
	}
	next, err = f.db.Task(ctx, f.project.ID, "t3")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := gitCommonDir(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := f.db.BranchAuthor(ctx, repository, next.Branch)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TASK_GENERATION", strconv.FormatInt(claim.Generation, 10))
	t.Setenv("HERDR_PANE_ID", next.PaneID)
	if code, out, stderr := f.run("publish", "Continue maintenance", "--verify", "CLI adoption tests pass"); code != 0 {
		t.Fatalf("publish: %d %s %s", code, out, stderr)
	}
	marker, err := f.db.GetPRBodyMarker(ctx, next.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	body := readFileForPublishTest(t, os.Getenv("POSSE_TEST_GH_EDIT_BODY"))
	if marker.Token == oldToken || strings.Contains(body, oldToken) || !strings.Contains(body, marker.Token) || !strings.HasPrefix(body, "Human note\n\n") {
		t.Fatalf("body token transfer failed: %s", body)
	}
	oldMarker, err := f.db.GetPRBodyMarker(ctx, f.task.ID, "")
	if err != nil || oldMarker.Token != oldToken {
		t.Fatalf("source token history changed: %+v %v", oldMarker, err)
	}
}
