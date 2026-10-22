package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSeededNewStoreIsCurrentWithoutReplayingMigrations(t *testing.T) {
	newStoreTemplate.Lock()
	saved := newStoreTemplate.enabled
	newStoreTemplate.enabled, newStoreTemplate.image = true, nil
	newStoreTemplate.Unlock()
	t.Cleanup(func() {
		newStoreTemplate.Lock()
		newStoreTemplate.enabled, newStoreTemplate.image = saved, nil
		newStoreTemplate.Unlock()
	})
	ctx := context.Background()
	source, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if newStoreTemplate.image == nil {
		t.Fatal("the first migrated store did not supply the template")
	}
	if _, err := source.CreateProject(ctx, "source-only", t.TempDir(), "main"); err != nil {
		t.Fatal(err)
	}

	seeded, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer seeded.Close()
	sources, err := embeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if !seeded.schemaCurrent(ctx, sources) {
		t.Fatal("seeded store is not recognized as current, so every open would take the migration lock")
	}
	var projects int
	if err := seeded.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects`).Scan(&projects); err != nil || projects != 0 {
		t.Fatalf("seeded store projects = %d, %v; want an empty store", projects, err)
	}
	var schema, seededSchema string
	query := `SELECT group_concat(sql, ';') FROM (SELECT sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY name)`
	if err := source.QueryRowContext(ctx, query).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if err := seeded.QueryRowContext(ctx, query).Scan(&seededSchema); err != nil {
		t.Fatal(err)
	}
	if schema != seededSchema {
		t.Fatal("seeded store schema differs from the migrated store")
	}
	if _, err := seeded.CreateProject(ctx, "seeded", t.TempDir(), "main"); err != nil {
		t.Fatalf("write to seeded store: %v", err)
	}
}
