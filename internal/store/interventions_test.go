package store

import (
	"context"
	"testing"
)

func TestNextLeadLaunchRecordsIntervention(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := db.NextLeadLaunch(ctx, project.ID); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.Interventions(ctx, project.ID, InterventionLeadLaunch)
	if err != nil || len(rows) != 2 || rows[0].TaskID != 0 || rows[0].At == 0 {
		t.Fatalf("lead launch interventions = %#v, %v", rows, err)
	}
}
