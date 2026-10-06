//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestApprovedLeadPreferencesMove(t *testing.T) {
	const quote = " Please move these preferences exactly. "
	const content = "Preserve bytes, spaces  and line endings.\r\n"
	for _, role := range []string{"lead", "rider"} {
		for _, scope := range []string{"user", "project"} {
			t.Run(role+"/"+scope, func(t *testing.T) {
				f := newPRLifecycleFixtureWithLead(t, true)
				defer f.db.Close()
				root, cwd := f.home, f.root
				args := []string{"preferences", "move", role}
				if scope == "project" {
					cwd = f.repo
					root = filepath.Join(root, "projects", "shop")
					args = append(args, "--project", "shop")
				}
				legacy := filepath.Join(root, "playbook", role+".md")
				target := filepath.Join(root, "preferences", role+".md")
				writeMoveFixtureFile(t, legacy, []byte(content))
				args = append(args, "--user-approved", quote)
				output, err := runLeadPreferenceMove(f, cwd, args...)
				if err != nil {
					t.Fatalf("approved move failed: %v\n%s", err, output)
				}
				if !strings.Contains(string(output), legacy) || !strings.Contains(string(output), target) || !strings.Contains(string(output), "approval_recorded: true") {
					t.Fatalf("move output omitted its paths or approval: %s", output)
				}
				moved, err := os.ReadFile(target)
				if err != nil || string(moved) != content {
					t.Fatalf("moved bytes = %q, err=%v", moved, err)
				}
				if _, err := os.Lstat(legacy); !os.IsNotExist(err) {
					t.Fatalf("legacy path remains after move: %v", err)
				}
				var projectID int64
				var key, action, value, recordedQuote string
				if err := f.db.QueryRowContext(context.Background(), `SELECT project_id,key,action,value,user_quote FROM config_approvals`).Scan(&projectID, &key, &action, &value, &recordedQuote); err != nil {
					t.Fatal(err)
				}
				if projectID != f.project.ID || key != "preferences."+role || action != "move" || value != target || recordedQuote != quote {
					t.Fatalf("move approval = %d %q %q %q %q", projectID, key, action, value, recordedQuote)
				}
			})
		}
	}

	t.Run("destination_exists", func(t *testing.T) {
		f := newPRLifecycleFixtureWithLead(t, true)
		defer f.db.Close()
		root := filepath.Join(f.home, "projects", "shop")
		legacy := filepath.Join(root, "playbook", "rider.md")
		target := filepath.Join(root, "preferences", "rider.md")
		writeMoveFixtureFile(t, legacy, []byte("legacy bytes\n"))
		writeMoveFixtureFile(t, target, []byte("canonical bytes\n"))
		output, err := runLeadPreferenceMove(f, f.repo, "preferences", "move", "rider", "--project", "shop", "--user-approved", quote)
		if err == nil || !strings.Contains(string(output), "preference_destination_exists") {
			t.Fatalf("existing destination was not refused: err=%v output=%s", err, output)
		}
		assertMoveFile(t, legacy, "legacy bytes\n")
		assertMoveFile(t, target, "canonical bytes\n")
		var count int
		if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM config_approvals`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("destination refusal recorded %d approvals, err=%v", count, err)
		}
	})

	t.Run("approval_failure_rolls_back", func(t *testing.T) {
		f := newPRLifecycleFixtureWithLead(t, true)
		defer f.db.Close()
		root := filepath.Join(f.home, "projects", "shop")
		legacy := filepath.Join(root, "playbook", "lead.md")
		target := filepath.Join(root, "preferences", "lead.md")
		writeMoveFixtureFile(t, legacy, []byte(content))
		if _, err := f.db.ExecContext(context.Background(), `CREATE TRIGGER reject_preference_move BEFORE INSERT ON config_approvals WHEN NEW.action='move' BEGIN SELECT RAISE(ABORT, 'approval blocked'); END`); err != nil {
			t.Fatal(err)
		}
		output, err := runLeadPreferenceMove(f, f.repo, "preferences", "move", "lead", "--project", "shop", "--user-approved", quote)
		if err == nil || !strings.Contains(string(output), "approval blocked") {
			t.Fatalf("injected approval failure was not returned: err=%v output=%s", err, output)
		}
		assertMoveFile(t, legacy, content)
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Fatalf("destination remains after approval failure: %v", err)
		}
		var count int
		if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM config_approvals`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed approval recorded %d approvals, err=%v", count, err)
		}
	})
}

func runLeadPreferenceMove(f *prLifecycleFixture, cwd string, args ...string) ([]byte, error) {
	command := exec.Command(f.binary, args...)
	command.Dir, command.Env = cwd, f.leadEnv
	return command.CombinedOutput()
}

func writeMoveFixtureFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertMoveFile(t *testing.T, path, want string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != want {
		t.Fatalf("%s = %q, err=%v; want %q", path, contents, err, want)
	}
}
