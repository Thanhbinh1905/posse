package store

import (
	"context"
	"errors"
	"testing"
)

func TestAdoptionAdmissionRefusalRollsBackTaskAndCustody(t *testing.T) {
	for _, scenario := range []string{"head changed", "generation changed", "writer active", "Decision appeared", "operation unresolved", "command active"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			db, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			p, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
			if err != nil {
				t.Fatal(err)
			}
			occupied := func(int) (bool, error) { return false, nil }
			source, _, err := db.CreateTaskWithSequence(ctx, p.ID, p.Name, Task{Type: "change", Title: "Source", ShortName: "source"}, occupied)
			if err != nil {
				t.Fatal(err)
			}
			const url = "https://github.com/acme/shop/pull/17"
			if _, err := db.ExecContext(ctx, `UPDATE tasks SET state='done',pr_url=?,gated_sha=? WHERE id=?`, url, branchAuthorTestSHA, source); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ClaimBranchAuthor(ctx, "shop", "posse/t1", source, 1, branchAuthorTestSHA, branchAuthorTestSHA, false); err != nil {
				t.Fatal(err)
			}
			request := &TaskAdoptionRequest{SourceTaskID: source, Repository: "shop", Branch: "posse/t1", PRURL: url, ExpectedHead: branchAuthorTestSHA, ExpectedGeneration: 1, PreviousWriterStopped: true}
			switch scenario {
			case "head changed":
				request.ExpectedHead = branchAuthorOtherSHA
			case "generation changed":
				request.ExpectedGeneration = 2
			case "writer active":
				request.PreviousWriterStopped = false
			case "Decision appeared":
				if _, err := db.ExecContext(ctx, `INSERT INTO decisions(project_id,task_id,origin,question,options_json,created_at) VALUES(?,?,'custody','Keep?','[]',1)`, p.ID, source); err != nil {
					t.Fatal(err)
				}
			case "operation unresolved":
				delivery, err := db.EnsureDelivery(ctx, source, "", url, "publish")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.RecordDeliveryOperation(ctx, DeliveryOperation{DeliveryID: delivery.ID, Operation: "inspect", Actor: "lead", Outcome: "uncertain"}); err != nil {
					t.Fatal(err)
				}
			case "command active":
				if _, err := db.ExecContext(ctx, `INSERT INTO intents(project_id,task_id,command,step,process_id,updated_at,payload_json) VALUES(?,?,'relaunch','checking',1,1,'{}')`, p.ID, source); err != nil {
					t.Fatal(err)
				}
			}
			task := Task{Type: "change", Title: "Successor", ShortName: "successor", Branch: "posse/t1", Adoption: request}
			if _, _, err := db.CreateTaskWithSequenceAndIntentWithinLimit(ctx, p.ID, p.Name, task, "start", 1, 1, occupied); !errors.Is(err, ErrTaskAdoptionChanged) {
				t.Fatalf("refusal=%v", err)
			}
			var count int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("Task count=%d err=%v", count, err)
			}
			old, err := db.TaskByID(ctx, p.ID, source)
			if err != nil || old.PRURL != url || old.State != StateDone || old.GatedSHA != branchAuthorTestSHA {
				t.Fatalf("source changed: %+v %v", old, err)
			}
			claim, err := db.BranchAuthor(ctx, "shop", "posse/t1")
			if err != nil || claim.TaskID != source || claim.Generation != 1 || claim.HeadSHA != branchAuthorTestSHA {
				t.Fatalf("claim changed: %+v %v", claim, err)
			}
			lineage, err := db.TaskAdoptions(ctx, source)
			if err != nil || len(lineage) != 0 {
				t.Fatalf("refusal wrote lineage: %+v %v", lineage, err)
			}
		})
	}
}
