package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type schemaFixture struct {
	Objects []struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Table string `json:"table"`
		SQL   string `json:"sql"`
	} `json:"objects"`
	AppliedMigrations []struct {
		Version  int64  `json:"version"`
		Checksum string `json:"checksum"`
	} `json:"applied_migrations"`
}

func TestV033HomeUpgradesToBaselineSchema(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "v033.0-schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture schemaFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	sort.Slice(fixture.Objects, func(i, j int) bool {
		order := map[string]int{"table": 0, "index": 1, "trigger": 2, "view": 3}
		if order[fixture.Objects[i].Type] != order[fixture.Objects[j].Type] {
			return order[fixture.Objects[i].Type] < order[fixture.Objects[j].Type]
		}
		return fixture.Objects[i].Name < fixture.Objects[j].Name
	})
	for _, object := range fixture.Objects {
		if _, err := legacy.Exec(object.SQL); err != nil {
			t.Fatalf("apply v0.33.0 %s %s DDL: %v", object.Type, object.Name, err)
		}
	}
	if _, err := legacy.Exec(`INSERT INTO goose_db_version(version_id,is_applied) VALUES(0,1)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range fixture.AppliedMigrations {
		if _, err := legacy.Exec(`INSERT INTO goose_db_version(version_id,is_applied) VALUES(?,1)`, migration.Version); err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.Exec(`INSERT INTO posse_migration_checksums(version,sha256) VALUES(?,?)`, migration.Version, migration.Checksum); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := OpenAt(legacyPath)
	if err != nil {
		t.Fatalf("upgrade v0.33.0 home: %v", err)
	}
	defer upgraded.Close()
	fresh, err := OpenAt(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("open fresh baseline home: %v", err)
	}
	defer fresh.Close()
	got := normalizedSchema(t, upgraded)
	want := normalizedSchema(t, fresh)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("upgraded schema differs from fresh baseline:\n%s", schemaDifference(want, got))
	}
}

func TestBaselineHomeAppliesLaterMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := embeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		if source.version <= 95 || source.version >= baselineMigrationVersion {
			continue
		}
		if _, err := db.ExecContext(context.Background(), `INSERT INTO goose_db_version(version_id,is_applied) VALUES(?,1)`, source.version); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(context.Background(), `INSERT INTO posse_migration_checksums(version,sha256) VALUES(?,?)`, source.version, source.checksum); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(context.Background(), `DELETE FROM goose_db_version WHERE version_id=20261122175108`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `DELETE FROM posse_migration_checksums WHERE version=20261122175108`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = OpenAt(path)
	if err != nil {
		t.Fatalf("open baseline home with a later migration: %v", err)
	}
	defer db.Close()
	var applied int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM goose_db_version WHERE version_id=20261122175108 AND is_applied=1`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("later migration applied rows = %d, %v; want 1", applied, err)
	}
}

func TestBaselineHomeGateSeesOnlyActiveMigrationsPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `DELETE FROM goose_db_version WHERE version_id=20261122175108`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var pending []int64
	MigrationGate = func(_ string, versions []int64, _ bool) error {
		pending = versions
		return nil
	}
	defer func() { MigrationGate = nil }()

	db, err = OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if len(pending) != 1 || pending[0] != 20261122175108 {
		t.Fatalf("gate pending = %v, want [20261122175108]", pending)
	}
}

func normalizedSchema(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT type,name,tbl_name,sql FROM sqlite_master WHERE type IN ('table','index','trigger','view') ORDER BY type,name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var schema []string
	for rows.Next() {
		var kind, name, table string
		var statement sql.NullString
		if err := rows.Scan(&kind, &name, &table, &statement); err != nil {
			t.Fatal(err)
		}
		if statement.Valid {
			schema = append(schema, fmt.Sprintf("%s|%s|%s|%s", kind, name, table, normalizeSQL(statement.String)))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(schema)
	return schema
}

func normalizeSQL(statement string) string {
	statement = strings.Join(strings.Fields(statement), " ")
	statement = strings.ReplaceAll(statement, " (", "(")
	statement = strings.ReplaceAll(statement, "( ", "(")
	statement = strings.ReplaceAll(statement, " )", ")")
	statement = strings.ReplaceAll(statement, " ,", ",")
	return statement
}

func schemaDifference(want, got []string) string {
	wantSet, gotSet := map[string]bool{}, map[string]bool{}
	for _, statement := range want {
		wantSet[statement] = true
	}
	for _, statement := range got {
		gotSet[statement] = true
	}
	var differences []string
	for statement := range wantSet {
		if !gotSet[statement] {
			differences = append(differences, "missing: "+statement)
		}
	}
	for statement := range gotSet {
		if !wantSet[statement] {
			differences = append(differences, "extra: "+statement)
		}
	}
	sort.Strings(differences)
	return strings.Join(differences, "\n")
}
