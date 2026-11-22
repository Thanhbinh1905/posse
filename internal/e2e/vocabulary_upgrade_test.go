//go:build e2e

package e2e

import (
	"os/exec"
	"strings"
	"testing"
)

func Test014ProjectVocabularyUpgradeRefusesPreBaselineHome(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	for _, statement := range []string{
		`DELETE FROM goose_db_version WHERE version_id > 54`,
		`DELETE FROM posse_migration_checksums WHERE version > 54`,
		`INSERT INTO goose_db_version(version_id,is_applied) VALUES(54,1)`,
	} {
		if _, err := f.db.Exec(statement); err != nil {
			t.Fatalf("prepare pre-v0.33.0 home: %s: %v", statement, err)
		}
	}
	command := exec.Command(f.binary, "list", "--all")
	command.Dir, command.Env = f.repo, f.leadEnv
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "update to posse v0.33.0 first") {
		t.Fatalf("open pre-v0.33.0 home: err=%v output=%s; want refusal directing the User to v0.33.0", err, output)
	}
}
