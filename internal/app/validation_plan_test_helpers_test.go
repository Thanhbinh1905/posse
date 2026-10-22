package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func installValidationPlanForReview(t *testing.T, fixture *prMergeFixture, review string) {
	t.Helper()
	ctx := context.Background()
	briefPath := filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "brief.md")
	briefData, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	var userChoice *taskplan.UserChoice
	if fixture.task.MergeMode == "local" {
		briefData = []byte(strings.Replace(string(briefData), "done_when: commit exists\n", "done_when: commit exists\nreview: none\n", 1))
		if err := os.WriteFile(briefPath, briefData, 0o600); err != nil {
			t.Fatal(err)
		}
		userChoice = &taskplan.UserChoice{Actor: "user", Source: "start --user-approved", Quote: "User approved no review for this fixture"}
	}
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief:         taskplan.Select("brief", briefPath, briefData),
		Execution:     taskplan.Execution{Mode: "sandbox", Scope: "repo", Base: "main", Target: "main"},
		Launch:        taskplan.Launch{ProfileName: "claude", Profile: config.Profile{Kind: "claude"}},
		Review:        review,
		Delivery:      fixture.task.MergeMode,
		Autonomy:      config.Autonomy{Review: "ask", Merge: "ask"},
		ConfigSources: []string{"test"},
		UserChoice:    userChoice,
		Paths: taskplan.Paths{
			Scratch: filepath.Join(fixture.home, "scratch", "shop", "{task}"),
			Output:  filepath.Join(fixture.home, "projects", "shop", "tasks", "{task}"),
		},
		ValidationLoops: &taskplan.ValidationLoops{
			Review: config.ReviewLoop{Enabled: true, MaxRounds: 3, BlockAt: "P1", AutoStart: true},
			CI:     config.CILoop{MaxFixRounds: 3, RerunCancelled: 1},
		},
		MergeMethod: "squash",
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := plan.Bind("t1").Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET plan_json=? WHERE id=?`, encoded, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `INSERT INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,1,?)`, fixture.task.ID, encoded); err != nil {
		t.Fatal(err)
	}
}

func seedHistoricalValidationFixRounds(t *testing.T, fixture *prMergeFixture, count int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < count; i++ {
		head := strings.Repeat("0", 39) + string(rune('1'+i))
		if _, err := fixture.db.QueueValidationFixInstruction(ctx, fixture.task.ID, "review", head, "historical-review-"+head, "Historical Review repair"); err != nil {
			t.Fatalf("seed historical repair %d: %v", i+1, err)
		}
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE messages SET status='delivered' WHERE task_id=?`, fixture.task.ID); err != nil {
		t.Fatalf("mark historical repair messages delivered: %v", err)
	}
}
