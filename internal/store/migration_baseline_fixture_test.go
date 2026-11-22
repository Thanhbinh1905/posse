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
