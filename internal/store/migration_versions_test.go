package store

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMigrationVersionsRejectNewSequentialNumbers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		valid bool
	}{
		{name: "20261120120000_new.sql", valid: true},
		{name: "00100_new.sql"},
		{name: "00099_new.sql"},
		{name: "20261320120000_new.sql"},
	} {
		err := validateNewMigrationVersion(tc.name)
		if (err == nil) != tc.valid {
			t.Errorf("validateNewMigrationVersion(%q) error = %v, want valid=%t", tc.name, err, tc.valid)
		}
	}
}

func validateNewMigrationVersion(name string) error {
	prefix, _, ok := strings.Cut(name, "_")
	if !ok {
		return fmt.Errorf("missing migration name separator")
	}
	if len(prefix) != 14 {
		return fmt.Errorf("new migrations must use a 14-digit goose timestamp version")
	}
	if _, err := time.Parse("20060102150405", prefix); err != nil {
		return fmt.Errorf("invalid goose timestamp version: %w", err)
	}
	return nil
}

func TestMigrationVersionsAreUnique(t *testing.T) {
	seen := map[int64]string{}
	for _, dir := range []string{"migrations", "upgrades"} {
		entries, err := fs.ReadDir(migrationFiles, dir)
		if err != nil {
			t.Fatal(err)
		}
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
}
