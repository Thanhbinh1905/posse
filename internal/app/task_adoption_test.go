package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestStartAdoptionInvalidSourceHasNoEffects(t *testing.T) {
	for _, scenario := range []string{"missing", "research", "without PR", "live executor", "unanswered Decision"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFirstOutcomeFixture(t)
			wantTasks := 0
			if scenario != "missing" {
				taskType := "change"
				if scenario == "research" {
					taskType = "research"
				}
				if _, err := f.db.CreateTask(context.Background(), f.project.ID, store.Task{Seq: 12, Type: taskType, Title: "Prior Task", State: store.StateSpawning}); err != nil {
					t.Fatal(err)
				}
				wantTasks = 1
				if scenario == "unanswered Decision" {
					if _, err := f.db.ExecContext(context.Background(), `UPDATE tasks SET pr_url='https://github.com/acme/shop/pull/17' WHERE seq=12`); err != nil {
						t.Fatal(err)
					}
					if _, err := f.db.ExecContext(context.Background(), `INSERT INTO decisions(project_id,task_id,origin,question,options_json,created_at) SELECT project_id,id,'adoption-test','Keep custody?','[]',1 FROM tasks WHERE seq=12`); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "live executor" {
					if _, err := f.db.ExecContext(context.Background(), `UPDATE tasks SET pr_url='https://github.com/acme/shop/pull/17',pane_id='w1:p2',pane_label='posse:shop:t12' WHERE seq=12`); err != nil {
						t.Fatal(err)
					}
					f.fake.SnapshotValue.Panes = append(f.fake.SnapshotValue.Panes, herdr.Pane{PaneID: "w1:p2", WorkspaceID: "w1", Label: "posse:shop:t12", Agent: "pi", AgentStatus: "idle"})
				}
			}
			if err := os.WriteFile(f.brief, []byte("---\ntype: change\ntitle: Continue prior PR\ndone_when: verified\nadopt: t12\n---\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HERDR_PANE_ID", "w1:p1")
			output := outcomeCLI(t, f.service, 1, "start", "--brief", f.brief)
			if !strings.Contains(output, "adoption_refused") {
				t.Fatal(output)
			}
			var tasks int
			if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM tasks`).Scan(&tasks); err != nil || tasks != wantTasks {
				t.Fatalf("tasks=%d err=%v", tasks, err)
			}
			for _, call := range f.fake.Calls {
				if call.Method == "tab.new" || call.Method == "agent.start" {
					t.Fatalf("refusal allocated a Worker: %#v", call)
				}
			}
		})
	}
}
