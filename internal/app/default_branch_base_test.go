package app

import (
	"context"
	"strings"
	"testing"
)

// A missing default branch is a repository precondition. It must refuse before
// admission instead of breaking every Sandbox and failing the Task (#699).
func TestStartRefusesMissingDefaultBranchBeforeAdmission(t *testing.T) {
	f := newFirstOutcomeFixture(t)
	gitTest(t, f.repo, "checkout", "-q", "-b", "other")
	gitTest(t, f.repo, "branch", "-D", "main")
	output := outcomeCLI(t, f.service, 1, "start", "--brief", f.brief, "--name", "no-main")
	if !strings.Contains(output, "default_branch_missing") || !strings.Contains(output, "git branch main <commit>") {
		t.Fatalf("start did not name the missing default branch and its fix: %s", output)
	}
	tasks, err := f.db.Tasks(context.Background(), f.project.ID, true)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("refused start left Tasks %#v (%v)", tasks, err)
	}
	sandboxes, err := f.db.Sandboxes(context.Background(), f.project.ID)
	if err != nil || len(sandboxes) != 0 {
		t.Fatalf("refused start touched Sandboxes %#v (%v)", sandboxes, err)
	}
}
