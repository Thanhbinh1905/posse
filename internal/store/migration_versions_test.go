package store

import (
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

func TestMigrationVersionsAreUnique(t *testing.T) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]string{}
	for _, entry := range entries {
		name := entry.Name()
		prefix, _, _ := strings.Cut(name, "_")
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Errorf("migration %s has no numeric version prefix", name)
			continue
		}
		if other, ok := seen[version]; ok {
			t.Errorf("migrations %s and %s share version %d", other, name, version)
			continue
		}
		seen[version] = name
	}
}
