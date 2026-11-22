package store

import (
	"context"
	"errors"
	"testing"
)

func openAndExec(t *testing.T, home string, statements ...string) {
	t.Helper()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range statements {
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
}

func requireSchemaError(t *testing.T, err error, code string) {
	t.Helper()
	var schema *SchemaError
	if !errors.As(err, &schema) || schema.Code != code || schema.Help == "" {
		t.Fatalf("Open error = %v, want SchemaError %s", err, code)
	}
}

func TestOpenRecordsMigrationChecksums(t *testing.T) {
	home := t.TempDir()
	openAndExec(t, home)
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM posse_migration_checksums`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("checksums recorded = %d, want one baseline checksum (%v)", count, err)
	}
}

func TestOpenRefusesASchemaThisBuildDoesNotKnow(t *testing.T) {
	home := t.TempDir()
	openAndExec(t, home, `INSERT INTO goose_db_version(version_id, is_applied) VALUES(9999, 1)`)
	_, err := Open(home)
	requireSchemaError(t, err, "schema_unknown")
	t.Setenv(ForceMigrationEnv, "1")
	db, err := Open(home)
	if err != nil {
		t.Fatalf("forced open failed: %v", err)
	}
	_ = db.Close()
}

func TestOpenRefusesADivergedSchema(t *testing.T) {
	home := t.TempDir()
	openAndExec(t, home, `UPDATE posse_migration_checksums SET sha256='from-another-branch' WHERE version=20261120180000`)
	_, err := Open(home)
	requireSchemaError(t, err, "schema_diverged")
}

func TestMigrationGateDecidesPendingMigrations(t *testing.T) {
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	prepareSupportedHomeAtVersion(t, db, 95)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	previous := MigrationGate
	t.Cleanup(func() { MigrationGate = previous })
	var gotPending []int64
	var gotForced bool
	MigrationGate = func(path string, pending []int64, forced bool) error {
		gotPending, gotForced = pending, forced
		return errors.New("not the installed posse")
	}
	_, err = Open(home)
	requireSchemaError(t, err, "migration_refused")
	if len(gotPending) != 9 || gotForced {
		t.Fatalf("gate saw pending=%v forced=%v, want migrations 96-101 and the baseline", gotPending, gotForced)
	}
	calls := 0
	MigrationGate = func(string, []int64, bool) error { calls++; return nil }
	if db, err := Open(t.TempDir()); err != nil || calls != 0 {
		t.Fatalf("a new database consulted the gate: calls=%d err=%v", calls, err)
	} else {
		_ = db.Close()
	}
}
