package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestFirstOutcomeWorkspaceReadinessUsesEffectiveMemberPolicies(t *testing.T) {
	f := newFirstOutcomeFixture(t)
	workspace := filepath.Join(f.root, "stack")
	for _, member := range []string{"backend", "worker"} {
		initRepo(t, filepath.Join(workspace, member))
	}
	project, err := f.db.CreateWorkspaceProject(context.Background(), "stack", workspace, []store.ProjectRepo{
		{Name: "backend", Path: "backend", DefaultBranch: "main", Status: store.RepoActive},
		{Name: "worker", Path: "worker", DefaultBranch: "main", Status: store.RepoActive},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)
	// Members without an origin Land locally, not through the Project's default pr mode.
	outcomeCLI(t, f.service, 0, "config", "set", "defaults.gate", "['true']", "--project", "stack", "--user-approved", "Use the fixture Gate")
	outcomeCLI(t, f.service, 0, "config", "set", "repositories.worker.gate", "[]", "--project", "stack", "--user-approved", "No Gate for worker")
	check := func(wantGate, wantForge bool) {
		t.Helper()
		for _, args := range [][]string{{"--json"}} {
			output := outcomeCLI(t, f.service, 0, args...)
			var result struct {
				Readiness []readinessGap `json:"readiness"`
			}
			if err := json.Unmarshal([]byte(output), &result); err != nil {
				t.Fatal(err)
			}
			gates, forges := 0, 0
			for _, gap := range result.Readiness {
				switch gap.Code {
				case "gate_empty":
					gates++
					if gap.Member != "worker" || gap.Key != "repositories.worker.gate" || !gap.UserOnly {
						t.Fatalf("wrong effective Gate: %#v", gap)
					}
				case "forge_auth":
					forges++
					if gap.Member != "backend" || gap.Key != "repositories.backend.forge" {
						t.Fatalf("wrong Member forge: %#v", gap)
					}
				}
			}
			if (gates == 1) != wantGate || gates > 1 || (forges == 1) != wantForge || forges > 1 {
				t.Fatalf("%v incorrect Member readiness: %s", args, output)
			}
		}
	}
	check(true, false)
	outcomeCLI(t, f.service, 0, "config", "set", "repositories.backend.landing_mode", "pr", "--project", "stack")
	check(true, true)
	outcomeCLI(t, f.service, 0, "config", "unset", "repositories.worker.gate", "--project", "stack", "--user-approved", "Inherit the Project Gate")
	check(false, true)
	// Removed/missing Members do not produce a stale gap.
	if _, err := f.db.ExecContext(context.Background(), `UPDATE project_repos SET status='missing' WHERE project_id=? AND name='backend'`, project.ID); err != nil {
		t.Fatal(err)
	}
	check(false, false)
}
