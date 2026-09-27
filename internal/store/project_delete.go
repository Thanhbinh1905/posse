package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"modernc.org/sqlite"
)

// ErrProjectReferenced reports related rows that prevented Project removal.
var ErrProjectReferenced = errors.New("project has related records that prevent removal")

// SQLite's extended SQLITE_CONSTRAINT_FOREIGNKEY result code.
const sqliteConstraintForeignKey = 787

type projectReference struct {
	table       string
	foreignKeys []foreignKeyReference
}

type foreignKeyReference struct {
	id            int
	columns       []string
	parentColumns []string
	parentTable   string
}

type foreignKeyMetadata struct {
	id          int
	sequence    int
	parentTable string
	column      string
	parent      string
}

type queryContext interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func projectReferenceTables(ctx context.Context, queryer queryContext) ([]projectReference, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read table name: %w", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("read table names: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close table names: %w", err)
	}

	allForeignKeys := make(map[string][]foreignKeyReference, len(tables))
	for _, table := range tables {
		foreignKeys, err := tableForeignKeyReferences(ctx, queryer, table)
		if err != nil {
			return nil, err
		}
		allForeignKeys[table] = foreignKeys
	}

	var references []projectReference
	for _, table := range tables {
		if strings.EqualFold(table, "projects") {
			continue
		}
		for _, foreignKey := range allForeignKeys[table] {
			if strings.EqualFold(foreignKey.parentTable, "projects") {
				references = append(references, projectReference{table: table, foreignKeys: allForeignKeys[table]})
				break
			}
		}
	}
	return orderProjectReferences(references, allForeignKeys), nil
}

func tableForeignKeyReferences(ctx context.Context, queryer queryContext, table string) ([]foreignKeyReference, error) {
	rows, err := queryer.QueryContext(ctx, `PRAGMA foreign_key_list(`+quoteIdentifier(table)+`)`)
	if err != nil {
		return nil, fmt.Errorf("list foreign keys for %s: %w", table, err)
	}
	groups := make(map[int]*foreignKeyReference)
	var order []int
	for rows.Next() {
		var item foreignKeyMetadata
		var onUpdate, onDelete, match string
		if err := rows.Scan(&item.id, &item.sequence, &item.parentTable, &item.column, &item.parent, &onUpdate, &onDelete, &match); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read foreign key for %s: %w", table, err)
		}
		group := groups[item.id]
		if group == nil {
			group = &foreignKeyReference{id: item.id, parentTable: item.parentTable}
			groups[item.id] = group
			order = append(order, item.id)
		}
		if item.sequence >= len(group.columns) {
			count := item.sequence + 1 - len(group.columns)
			group.columns = append(group.columns, make([]string, count)...)
			group.parentColumns = append(group.parentColumns, make([]string, count)...)
		}
		group.columns[item.sequence] = item.column
		group.parentColumns[item.sequence] = item.parent
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("read foreign keys for %s: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close foreign keys for %s: %w", table, err)
	}

	foreignKeys := make([]foreignKeyReference, 0, len(order))
	for _, id := range order {
		foreignKey := *groups[id]
		for index, parentColumn := range foreignKey.parentColumns {
			if parentColumn == "" {
				foreignKey.parentColumns[index] = "id"
			}
		}
		foreignKeys = append(foreignKeys, foreignKey)
	}
	return foreignKeys, nil
}

func orderProjectReferences(references []projectReference, allForeignKeys map[string][]foreignKeyReference) []projectReference {
	byTable := make(map[string]projectReference, len(references))
	var tables []string
	for _, reference := range references {
		byTable[reference.table] = reference
		tables = append(tables, reference.table)
	}
	sort.Strings(tables)

	// Delete each dependent before the tables it references. Cycles are emitted once
	// in deterministic order; SQLite reports any cycle that contains remaining rows.
	ordered := make([]projectReference, 0, len(references))
	visited := map[string]bool{}
	visiting := map[string]bool{}
	var visit func(string)
	visit = func(table string) {
		if visited[table] || visiting[table] {
			return
		}
		visiting[table] = true
		ordered = append(ordered, byTable[table])
		for _, foreignKey := range allForeignKeys[table] {
			parent := matchingTable(foreignKey.parentTable, byTable)
			if parent != "" && parent != table {
				visit(parent)
			}
		}
		delete(visiting, table)
		visited[table] = true
	}
	for _, table := range tables {
		visit(table)
	}
	return ordered
}

func matchingTable(name string, tables map[string]projectReference) string {
	for table := range tables {
		if strings.EqualFold(table, name) {
			return table
		}
	}
	return ""
}

func deleteProjectReferences(ctx context.Context, tx *sql.Tx, projectID int64) error {
	references, err := projectReferenceTables(ctx, tx)
	if err != nil {
		return err
	}
	for _, reference := range references {
		var predicates []string
		var arguments []any
		for _, foreignKey := range reference.foreignKeys {
			if !strings.EqualFold(foreignKey.parentTable, "projects") {
				continue
			}
			var parts []string
			for index, column := range foreignKey.columns {
				parentColumn := foreignKey.parentColumns[index]
				parts = append(parts, quoteIdentifier(column)+` = (SELECT `+quoteIdentifier(parentColumn)+` FROM projects WHERE id=?)`)
				arguments = append(arguments, projectID)
			}
			predicates = append(predicates, "("+strings.Join(parts, " AND ")+")")
		}
		if len(predicates) == 0 {
			continue
		}
		query := `DELETE FROM ` + quoteIdentifier(reference.table) + ` WHERE ` + strings.Join(predicates, " OR ")
		if _, err := tx.ExecContext(ctx, query, arguments...); err != nil {
			if isForeignKeyConstraint(err) {
				return fmt.Errorf("%w: delete %s: %v", ErrProjectReferenced, reference.table, err)
			}
			return fmt.Errorf("delete %s: %w", reference.table, err)
		}
	}
	return nil
}

func quoteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func isForeignKeyConstraint(err error) bool {
	var sqliteError *sqlite.Error
	return errors.As(err, &sqliteError) && sqliteError.Code() == sqliteConstraintForeignKey
}

func projectReferenceConstraint(err error) error {
	if isForeignKeyConstraint(err) {
		return fmt.Errorf("%w: %v", ErrProjectReferenced, err)
	}
	return err
}
