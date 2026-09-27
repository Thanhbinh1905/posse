package app

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

type projectForeignKey struct {
	columns       []string
	parents       []string
	parentColumns []string
}

func TestProjectRemoveDeletesEveryProjectReference(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "stray", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := db.CreateProject(ctx, "anchor", filepath.Join(root, "anchor-repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE future_project_state (project_id INTEGER PRIMARY KEY REFERENCES projects(id), value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, anchor.ID, store.Task{Seq: 1, Type: "ship", Title: "anchor", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}

	references := projectReferenceTables(t, db)
	seeded := make(map[string]bool, len(references))
	for _, table := range []string{"tasks", "mounts"} {
		if _, ok := references[table]; !ok {
			t.Fatalf("SQLite foreign-key metadata did not identify %s as a Project reference", table)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id, n, path, state, task_id, acquired_at, released_at) VALUES(?, 1, ?, 'released', ?, 0, 0)`, anchor.ID, filepath.Join(root, "anchor-mount"), taskID); err != nil {
		t.Fatal(err)
	}
	for table, foreignKeys := range references {
		// Tasks and Mounts define whether a Project is empty, so their seed rows
		// belong to the separate anchor Project. That Task also supplies valid
		// task_id references for Project-level rows. All other references belong
		// to the Project being removed.
		if table == "tasks" || table == "mounts" {
			seeded[table] = true
			continue
		}
		seedProjectReference(t, db, table, foreignKeys, project, taskID)
		seeded[table] = true
	}
	for table := range references {
		if !seeded[table] {
			t.Errorf("no project reference row was seeded for %s", table)
		}
	}
	for _, table := range []string{"decisions", "decision_notice_cursors", "future_project_state"} {
		if !seeded[table] {
			t.Fatalf("no project reference row was seeded for %s", table)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	t.Chdir(root)
	if code, output := runCLI(t, testService(home, nil), "project", "remove", "stray", "--yes"); code != 0 || !strings.Contains(output, "removed: true") {
		t.Fatalf("project removal failed with rows in every removable project-reference table: code=%d output=%s", code, output)
	}

	observer, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	if _, err := observer.ProjectByName(ctx, "stray"); !store.IsNotFound(err) {
		t.Fatalf("removed Project still exists: %v", err)
	}
	if _, err := observer.ProjectByName(ctx, "anchor"); err != nil {
		t.Fatalf("unrelated Project was removed: %v", err)
	}
	if tasks, mounts, err := observer.ProjectContents(ctx, anchor.ID); err != nil || tasks != 1 || mounts != 1 {
		t.Fatalf("unrelated Project contents changed: tasks=%d mounts=%d err=%v", tasks, mounts, err)
	}
	rows, err := observer.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowID, parent string
		var foreignKeyID int
		if err := rows.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("project removal left an orphan row: table=%s row=%s parent=%s fk=%d", table, rowID, parent, foreignKeyID)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestProjectRemoveReportsRelatedForeignKeyFailure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "blocked", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO project_runtime(project_id, server_started_at) VALUES(?, '')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE runtime_guard (project_id INTEGER NOT NULL REFERENCES project_runtime(project_id))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO runtime_guard(project_id) VALUES(?)`, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	t.Chdir(root)
	if code, output := runCLI(t, testService(home, nil), "project", "remove", "blocked", "--yes"); code == 0 || !strings.Contains(output, "project_has_dependents") || strings.Contains(output, "internal_error") {
		t.Fatalf("related-row failure was not reported with a project error code: code=%d output=%s", code, output)
	}
}

func projectReferenceTables(t *testing.T, db *store.DB) map[string][]projectForeignKey {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}

	references := make(map[string][]projectForeignKey)
	for _, table := range tables {
		foreignKeys := tableForeignKeys(t, db, table)
		for _, foreignKey := range foreignKeys {
			if len(foreignKey.parents) == 0 || !strings.EqualFold(foreignKey.parents[0], "projects") {
				continue
			}
			references[table] = append(references[table], foreignKey)
		}
	}
	return references
}

func tableForeignKeys(t *testing.T, db *store.DB, table string) []projectForeignKey {
	t.Helper()
	rows, err := db.Query(`PRAGMA foreign_key_list(` + quoteSQLiteIdentifier(table) + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	byID := map[int]*projectForeignKey{}
	var order []int
	for rows.Next() {
		var id, sequence int
		var parent, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &sequence, &parent, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatal(err)
		}
		foreignKey := byID[id]
		if foreignKey == nil {
			foreignKey = &projectForeignKey{}
			byID[id] = foreignKey
			order = append(order, id)
		}
		for len(foreignKey.columns) <= sequence {
			foreignKey.columns = append(foreignKey.columns, "")
			foreignKey.parents = append(foreignKey.parents, "")
			foreignKey.parentColumns = append(foreignKey.parentColumns, "")
		}
		foreignKey.columns[sequence] = from
		foreignKey.parents[sequence] = parent
		foreignKey.parentColumns[sequence] = to
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	result := make([]projectForeignKey, 0, len(order))
	for _, id := range order {
		result = append(result, *byID[id])
	}
	return result
}

func seedProjectReference(t *testing.T, db *store.DB, table string, foreignKeys []projectForeignKey, project store.Project, taskID int64) {
	t.Helper()
	values := map[string]any{}
	for _, foreignKey := range foreignKeys {
		for index, column := range foreignKey.columns {
			parentColumn := foreignKey.parentColumns[index]
			if parentColumn == "" {
				parentColumn = primaryKeyColumn(t, db, foreignKey.parents[index])
			}
			var value any
			query := "SELECT " + quoteSQLiteIdentifier(parentColumn) + " FROM projects WHERE id=?"
			if err := db.QueryRow(query, project.ID).Scan(&value); err != nil {
				t.Fatalf("cannot seed %s.%s from Project.%s: %v", table, column, parentColumn, err)
			}
			values[column] = value
		}
	}
	for _, foreignKey := range tableForeignKeys(t, db, table) {
		for index, column := range foreignKey.columns {
			if _, alreadySet := values[column]; alreadySet {
				continue
			}
			parent := foreignKey.parents[index]
			if strings.EqualFold(parent, "tasks") {
				values[column] = taskID
				continue
			}
			parentColumn := foreignKey.parentColumns[index]
			if parentColumn == "" {
				parentColumn = primaryKeyColumn(t, db, parent)
			}
			var value any
			query := "SELECT " + quoteSQLiteIdentifier(parentColumn) + " FROM " + quoteSQLiteIdentifier(parent) + " LIMIT 1"
			if err := db.QueryRow(query).Scan(&value); err != nil {
				t.Fatalf("cannot seed %s.%s from %s.%s: %v", table, column, parent, parentColumn, err)
			}
			values[column] = value
		}
	}

	columns := requiredColumns(t, db, table)
	for _, column := range columns {
		if _, set := values[column.name]; set || !column.required {
			continue
		}
		values[column.name] = seedValue(column)
	}
	if action, ok := values["action"]; ok && action == "set" {
		for _, column := range columns {
			if column.name == "value" {
				values[column.name] = "test value"
			}
		}
	}

	insertColumns := make([]string, 0, len(values))
	arguments := make([]any, 0, len(values))
	for _, column := range columns {
		if value, ok := values[column.name]; ok {
			insertColumns = append(insertColumns, quoteSQLiteIdentifier(column.name))
			arguments = append(arguments, value)
		}
	}
	placeholders := make([]string, len(arguments))
	for index := range placeholders {
		placeholders[index] = "?"
	}
	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", quoteSQLiteIdentifier(table), strings.Join(insertColumns, ", "), strings.Join(placeholders, ", "))
	if _, err := db.Exec(query, arguments...); err != nil {
		t.Fatalf("seed project reference table %s: %v (query %s)", table, err, query)
	}
}

type seedColumn struct {
	name     string
	kind     string
	required bool
}

func requiredColumns(t *testing.T, db *store.DB, table string) []seedColumn {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + quoteSQLiteIdentifier(table) + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []seedColumn
	for rows.Next() {
		var index, notNull, primaryKey int
		var name, kind string
		var defaultValue sql.NullString
		if err := rows.Scan(&index, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, seedColumn{name: name, kind: kind, required: notNull != 0 && !defaultValue.Valid})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}

func seedValue(column seedColumn) any {
	if strings.Contains(strings.ToUpper(column.kind), "INT") {
		return int64(1)
	}
	if strings.Contains(strings.ToUpper(column.kind), "REAL") || strings.Contains(strings.ToUpper(column.kind), "NUM") {
		return float64(1)
	}
	if strings.Contains(strings.ToUpper(column.kind), "BLOB") {
		return []byte{}
	}
	switch column.name {
	case "action":
		return "set"
	case "user_quote":
		return "test quote"
	case "value":
		return "test value"
	default:
		return "test"
	}
}

func primaryKeyColumn(t *testing.T, db *store.DB, table string) string {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + quoteSQLiteIdentifier(table) + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var index, notNull, primaryKey int
		var name, kind string
		var defaultValue sql.NullString
		if err := rows.Scan(&index, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if primaryKey > 0 {
			return name
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("table %s has no primary key", table)
	return ""
}

func quoteSQLiteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}
