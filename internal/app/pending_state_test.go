package app

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// pendingFixture adds t1 waiting in needs-decision, t2 working with a pending
// worker_question Decision and t3 working with nothing pending.
func pendingFixture(t *testing.T) (string, store.Project, store.Decision) {
	t.Helper()
	ctx := context.Background()
	home, db, project := restoreFixture(t)
	ids := map[int]int64{}
	for seq := 1; seq <= 3; seq++ {
		id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: seq, Type: "change", Title: "Work", MergeMode: "local"})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Transition(ctx, id, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
			t.Fatal(err)
		}
		ids[seq] = id
	}
	if err := db.Transition(ctx, ids[1], store.StateWorking, store.StateNeedsDecision, "worker", "Which schema wins?"); err != nil {
		t.Fatal(err)
	}
	decision, err := db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: project.ID, TaskID: ids[2], Origin: "test:2", Kind: "worker_question", Question: "Spec or code?", Options: []string{"spec", "code"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: ids[3], Kind: "task_progress", Summary: "t3 progressed"}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project.Root)
	t.Setenv("HERDR_PANE_ID", "")
	return home, project, decision
}

func runJSON(t *testing.T, home string, args ...string) map[string]any {
	t.Helper()
	var output bytes.Buffer
	cli := testService(home, nil).CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run(append([]string{"--json"}, args...)); code != 0 {
		t.Fatalf("%v = %d: %s", args, code, output.String())
	}
	var result map[string]any
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("%v: %v: %s", args, err, output.String())
	}
	return result
}

func TestNeedsYouListsPendingDecisionsAndNeedsDecisionTasks(t *testing.T) {
	home, _, decision := pendingFixture(t)
	result := runJSON(t, home)
	rows, _ := result["needs_you"].([]any)
	if len(rows) != 2 {
		t.Fatalf("needs_you = %v, want t1 and t2", result["needs_you"])
	}
	first, second := rows[0].(map[string]any), rows[1].(map[string]any)
	if first["id"] != "t1" || first["state"] != "needs-decision" || first["reason"] != "Which schema wins?" {
		t.Fatalf("needs-decision Task row = %v", first)
	}
	decisions, _ := second["decisions"].([]any)
	if second["id"] != "t2" || len(decisions) != 1 || decisions[0] != float64(decision.ID) {
		t.Fatalf("pending Decision row = %v, want t2 with Decision %d", second, decision.ID)
	}
	for _, row := range result["tasks"].([]any) {
		if id := row.(map[string]any)["id"]; id != "t3" {
			t.Fatalf("tasks repeats needs_you Task %v", id)
		}
	}

	var output bytes.Buffer
	cli := testService(home, nil).CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run(nil); code != 0 || !strings.Contains(output.String(), "needs_you[2]") {
		t.Fatalf("dashboard = %d: %s", code, output.String())
	}
}

func TestLeadPrintsPendingBlock(t *testing.T) {
	home, project, decision := pendingFixture(t)
	t.Setenv("HERDR_PANE_ID", project.LeadPaneID)
	result := runJSON(t, home, "lead")
	pending, ok := result["pending"].(map[string]any)
	if !ok {
		t.Fatalf("lead has no pending block: %v", result)
	}
	decisions, _ := pending["decisions"].([]any)
	tasks, _ := pending["needs_decision"].([]any)
	if len(decisions) != 1 || decisions[0] != float64(decision.ID) || len(tasks) != 1 || tasks[0] != "t1" || pending["unacked_notices"] != float64(1) {
		t.Fatalf("pending ids and counts = %v", pending)
	}
	if want := filepath.Join(home, "projects", project.Name, "drafts"); pending["drafts"] != want {
		t.Fatalf("pending drafts = %v, want %s", pending["drafts"], want)
	}
	if help, _ := pending["catch_up"].(string); !strings.Contains(help, "posse-catch-up") {
		t.Fatalf("pending block has no posse-catch-up pointer: %v", pending)
	}
	if text, _ := json.Marshal(pending); strings.Contains(string(text), "Spec or code?") || strings.Contains(string(text), "Which schema wins?") {
		t.Fatalf("pending block carries bodies: %s", text)
	}
	var output bytes.Buffer
	cli := testService(home, nil).CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"lead"}); code != 0 || !strings.Contains(output.String(), "needs_decision[1]: t1") {
		t.Fatalf("lead TOON pending block = %d: %s", code, output.String())
	}
}

func TestUpAllFromLeadPaneNamesCauseAndFix(t *testing.T) {
	startWait, startDelay := agentStartBusyWait, agentStartRetryDelay
	agentStartBusyWait, agentStartRetryDelay = 0, 0
	t.Cleanup(func() { agentStartBusyWait, agentStartRetryDelay = startWait, startDelay })
	home, _, project := restoreFixture(t)
	fake := herdr.NewFake()
	fake.SnapshotValue = restartedSnapshot(project)
	fake.Errors["agent.start"] = paneBusy()
	t.Setenv("HERDR_PANE_ID", project.LeadPaneID)
	output := &bytes.Buffer{}
	cli := testService(home, fake).CLI()
	cli.Out, cli.ErrOut = output, output
	cli.Run([]string{"up", "--all"})
	text := output.String()
	if !strings.Contains(text, "runs in the Lead pane "+project.LeadPaneID) || !strings.Contains(text, "Run `posse up` in the Lead pane of shop") || strings.Contains(text, "Fix the failed Projects") {
		t.Fatalf("up --all from the Lead pane did not name the cause and fix: %s", text)
	}
}

func TestUpAllFromLiveLeadRestoresProject(t *testing.T) {
	home, _, project := restoreFixture(t)
	fake := herdr.NewFake()
	fake.SnapshotValue = claudeLeadSnapshot(project)
	t.Setenv("HERDR_PANE_ID", project.LeadPaneID)
	output := &bytes.Buffer{}
	cli := testService(home, fake).CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"up", "--all"}); code != 0 || !strings.Contains(output.String(), "state: restored") || strings.Contains(output.String(), "runs in the Lead pane") {
		t.Fatalf("up --all from a running Lead = %d: %s", code, output)
	}
}
