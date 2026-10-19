package store

import (
	"context"
	"os"
	"testing"
)

func TestVocabularyMigrationPreservesMemberNamesAndApprovalEvidence(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "upgrade", t.TempDir(), "main")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	steps := map[string]string{
		"done:gate.run:paramount.web":                      "done:gate.run:paramount.web",
		"in_progress:merge:rider.api":                      "in_progress:merge:rider.api",
		"resuming:done:push:leftover.repo":                 "resuming:done:push:leftover.repo",
		"resuming:in_progress:pr.merge:landed_ref.service": "resuming:in_progress:pr.merge:landed_ref.service",
		"done:gate.run:task.landing":                       "done:gate.run:task.landing",
		"done:mount.acquire":                               "done:sandbox.acquire",
		"resuming:resuming:in_progress:rider.start":        "resuming:resuming:in_progress:worker.start",
		"done:leftover.snapshot:mount.api":                 "done:orphan.snapshot:mount.api",
		"done:task.landing":                                "done:task.merging",
		"in_progress:task.landed":                          "in_progress:task.merged",
		"done:landed_ref.record":                           "done:merged_ref.record",
	}
	ids := map[int64]string{}
	for old, want := range steps {
		id, err := db.CreateTask(ctx, project.ID, Task{Seq: len(ids) + 1, Type: "change", Title: "Recovery"})
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		if err := db.StartIntent(ctx, project.ID, id, "land --workspace", old, `{"quote":"mount.api rider land"}`, os.Getpid()); err != nil {
			db.Close()
			t.Fatal(err)
		}
		ids[id] = want
	}
	keys := map[string]string{
		"preferences.rider": "preferences.worker", "preferences.lead": "preferences.lead",
		"remuda.setup": "pool.setup", "defaults.landing_mode": "defaults.merge_mode",
		"defaults.auto_unsaddle": "defaults.auto_teardown", "autonomy.land": "autonomy.merge",
		"repositories.myremuda.gate":         "repositories.myremuda.gate",
		"repositories.remuda.gate":           "repositories.remuda.gate",
		"repositories.landing_mode.gate":     "repositories.landing_mode.gate",
		"repositories.autonomy.land.gate":    "repositories.autonomy.land.gate",
		"repositories.myremuda.landing_mode": "repositories.myremuda.merge_mode",
	}
	approvals := map[int64]string{}
	for old, want := range keys {
		result, err := db.ExecContext(ctx, `INSERT INTO config_approvals(project_id,key,action,value,user_quote,at) VALUES(?,?,'set','"Rider must land"','User says remuda.setup',42)`, project.ID, old)
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		approvals[id] = want
	}
	for _, statement := range []string{`DELETE FROM goose_db_version WHERE version_id=55`, `DELETE FROM posse_migration_checksums WHERE version=55`} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for id, want := range ids {
		intent, err := db.IntentByTask(ctx, id)
		if err != nil || intent.Step != want || intent.PayloadJSON != `{"quote":"mount.api rider land"}` {
			t.Errorf("member name or intent changed: want=%q intent=%#v err=%v", want, intent, err)
		}
	}
	for id, want := range approvals {
		var key, action, value, quote string
		var at int64
		err := db.QueryRowContext(ctx, `SELECT key,action,value,user_quote,at FROM config_approvals WHERE id=?`, id).Scan(&key, &action, &value, &quote, &at)
		if err != nil || key != want || action != "set" || value != `"Rider must land"` || quote != "User says remuda.setup" || at != 42 {
			t.Errorf("approval %d: key=%q want=%q value=%q quote=%q at=%d err=%v", id, key, want, value, quote, at, err)
		}
	}
}
