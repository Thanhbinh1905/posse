//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoricalWorkflowRunHomeStillOpensAndNewConfigIsRejected(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()

	brief := filepath.Join(fixture.root, "legacy.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Legacy workflow task\ndone_when: persisted plan loads\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", brief, "--name", "legacy-run"); !strings.Contains(output, "t1") {
		t.Fatalf("start historical Task fixture: %s", output)
	}

	workflow := `{"name":"feature","run_id":"w1","stage_name":"Build","stage_index":0,"profile":{"stages":[{"name":"Build","task_type":"change"}]}}`
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET plan_json=json_set(plan_json,'$.version',9,'$.workflow',json(?)),workflow_run_id='w1' WHERE project_id=? AND seq=1`, workflow, fixture.project.ID); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1", "--full"); !strings.Contains(output, "Legacy workflow task") {
		t.Fatalf("open home with a historical staged-workflow plan: %s", output)
	}
	var taskID int64
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT id FROM tasks WHERE project_id=? AND seq=1`, fixture.project.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `INSERT INTO decisions(project_id,task_id,origin,question,options_json,answer,kind,created_at,answered_at) VALUES(?,?,'workflow-stage:legacy','Historical workflow stage','["advance"]','advance','workflow_stage',1,1)`, fixture.project.ID, taskID); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "decisions", "--all"); strings.Contains(output, "Historical workflow stage") {
		t.Fatalf("removed workflow Decision is still exposed: %s", output)
	}

	projectConfig := filepath.Join(fixture.home, "projects", fixture.project.Name, "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfig, []byte(`[workflows.feature]
stages = [{ name = "Build", frame = "implement", gate = "user", task_type = "change", workers = 1 }]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(fixture.binary, "start", "--brief", brief, "--name", "config-check")
	command.Dir, command.Env = fixture.repo, fixture.leadEnv
	outputBytes, showErr := command.CombinedOutput()
	output := string(outputBytes)
	if showErr == nil || !strings.Contains(output, "staged workflows were removed") || !strings.Contains(output, "ADR 0026") {
		t.Fatalf("staged workflow config was not refused with removal note: %s", output)
	}
}
