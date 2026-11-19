package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestGreenhouseDeliveryRequiresCurrentPassedPipeline(t *testing.T) {
	base := `{"outcome":"ok","run_id":"run-1","run":{"id":"run-1","submitted":{"branch":"posse/t1","head":"abc"},"candidate":"abc","status":"ci-ready","pipeline":{"status":"ci-ready"}},"delivery":{"pushed_head":"abc","pr_url":"https://github.com/acme/shop/pull/1","pr_number":1,"current":true,"checks":[{"name":"test","status":"completed","conclusion":"success","head":"abc"}]}}`
	if _, err := parseGreenhouseDelivery([]byte(base), "run-1", "posse/t1", "abc"); err != nil {
		t.Fatalf("parse valid delivery: %v", err)
	}
	for name, input := range map[string]string{
		"forged run id":     strings.Replace(base, `"run_id":"run-1"`, `"run_id":"forged"`, 1),
		"moved pushed head": strings.Replace(base, `"pushed_head":"abc"`, `"pushed_head":"other"`, 1),
		"non-current":       strings.Replace(base, `"current":true`, `"current":false`, 1),
		"failed CI":         strings.Replace(base, `"conclusion":"success"`, `"conclusion":"failure"`, 1),
		"wrong branch":      strings.Replace(base, `"branch":"posse/t1"`, `"branch":"posse/t2"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseGreenhouseDelivery([]byte(input), "run-1", "posse/t1", "abc"); err == nil {
				t.Fatal("accepted invalid Greenhouse delivery")
			}
		})
	}
}

func TestInspectGreenhouseDeliveryUsesRunIDAndDefaultHome(t *testing.T) {
	dir := t.TempDir()
	output := `{"outcome":"ok","run_id":"run-1","run":{"id":"run-1","submitted":{"branch":"posse/t1","head":"abc"},"candidate":"abc","status":"ci-ready","pipeline":{"status":"ci-ready"}},"delivery":{"pushed_head":"abc","pr_url":"https://github.com/acme/shop/pull/1","pr_number":1,"current":true,"checks":[{"name":"test","status":"completed","conclusion":"success","head":"abc"}]}}`
	script := "#!/bin/sh\n[ -z \"$GREENHOUSE_HOME\" ] || exit 3\nrequest=$(cat)\ncase \"$request\" in *'\"run_id\":\"run-1\"'*) ;; *) exit 4 ;; esac\nprintf '%s\\n' '" + output + "'\n"
	binary := filepath.Join(dir, "greenhouse")
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("GREENHOUSE_HOME", filepath.Join(dir, "unexpected-home"))
	if _, err := inspectGreenhouseDelivery(context.Background(), dir, "run-1", "posse/t1", "abc"); err != nil {
		t.Fatalf("inspect Greenhouse delivery: %v", err)
	}
}

func TestGreenhouseMergeRequiresTheVerifiedDoneSignalForItsGatedHead(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Delivery", MergeMode: "greenhouse"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	task, err := db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"greenhouse_run_id": "run-1", "greenhouse_pending_head": "abc", "pr_url": "https://github.com/acme/shop/pull/1"}
	if _, err := db.RecordWorkerSignal(ctx, task, "done", "Pipeline passed", data, "task_done"); err != nil {
		t.Fatal(err)
	}
	task, err = db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if err := requireGreenhouseDeliverySignal(ctx, db, task); err != nil {
		t.Fatalf("accepted delivery Signal was rejected: %v", err)
	}
	task.GatedSHA = "forged"
	if err := requireGreenhouseDeliverySignal(ctx, db, task); err == nil {
		t.Fatal("accepted Greenhouse run bound to a different gated head")
	}
}

func TestGreenhousePullRequestRequiresExactHeadAndGreenChecks(t *testing.T) {
	base := `{"state":"OPEN","headRefOid":"abc","url":"https://github.com/acme/shop/pull/1","number":1,"statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"}]}`
	if err := validateGreenhousePullRequest([]byte(base), "abc", "https://github.com/acme/shop/pull/1", 1); err != nil {
		t.Fatalf("validate green PR: %v", err)
	}
	for name, input := range map[string]string{
		"moved head":    strings.Replace(base, `"headRefOid":"abc"`, `"headRefOid":"other"`, 1),
		"closed PR":     strings.Replace(base, `"state":"OPEN"`, `"state":"CLOSED"`, 1),
		"failed check":  strings.Replace(base, `"conclusion":"SUCCESS"`, `"conclusion":"FAILURE"`, 1),
		"pending check": strings.Replace(base, `"status":"COMPLETED"`, `"status":"IN_PROGRESS"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateGreenhousePullRequest([]byte(input), "abc", "https://github.com/acme/shop/pull/1", 1); err == nil {
				t.Fatal("accepted PR that is not green at the gated head")
			}
		})
	}
}
