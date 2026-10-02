package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Regression from t152 for the shortened, connection-local busy timeout.
func TestReviewObservationTimeoutRestoredAfterContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	other, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "review", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	writer, err := other.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	started := time.Now()
	err = db.UpdateProjectObservation(writeCtx, project.ID, "w1:p1", "w1", 0)
	elapsed := time.Since(started)
	if !IsBusy(err) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("observation error=%v, want typed busy without deadline", err)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("observation waited %s, want less than parent budget", elapsed)
	}
	var restored int
	if err := db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&restored); err != nil {
		t.Fatal(err)
	}
	if restored != 5000 {
		t.Fatalf("pooled connection busy_timeout=%d, want 5000", restored)
	}
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateProjectObservation(ctx, project.ID, "w1:p1", "w1", 0); err != nil {
		t.Fatalf("observation did not recover after writer release: %v", err)
	}
	t.Logf("contended observation waited %s, restored busy_timeout=%d, subsequent write succeeded", elapsed, restored)
}
