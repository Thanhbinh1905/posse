package taskplan_test

import (
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func TestWorkspaceScopePreservesScopedPreferencesAndPinnedBase(t *testing.T) {
	r := request("ship")
	r.Delivery, r.Review = "pr", "always"
	r.Brief = taskplan.Select("brief", "scoped-workspace.md", []byte("---\ntype: ship\ntitle: Scoped workspace\ndone_when: checked\nrepos: [api]\nroot_context: read-only\nlanding_mode: local\nreview: never\nbase: release/next\ntarget: release/next\n---\n"))
	commit := strings.Repeat("a", 40)
	r.Execution = taskplan.Execution{Scope: "workspace", Members: []taskplan.Member{{
		Name: "api", Path: "api", Base: "refs/remotes/origin/release/next", BaseCommit: commit, Target: "release/next", Delivery: "local",
	}}}
	if _, err := taskplan.Resolve(r); err == nil {
		t.Fatal("workspace scope bypassed tightening-only preferences")
	}
	r.UserChoice = &taskplan.UserChoice{Quote: "  Use local delivery without review for this release Task.  ", Actor: "lead", Source: "ride --user-approved"}
	plan, err := taskplan.Resolve(r)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	plan, err = taskplan.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Execution.RootContext != taskplan.RootContextReadOnly || len(plan.Execution.Members) != 1 || plan.Execution.Members[0].BaseCommit != commit || plan.Execution.Members[0].Target != "release/next" {
		t.Fatalf("workspace context or pinned member selection lost: %#v", plan.Execution)
	}
	if plan.Delivery != "local" || plan.Review != "never" || plan.UserChoice == nil || plan.UserChoice.Quote != r.UserChoice.Quote || plan.UserChoice.BriefSHA256 != r.Brief.SHA256 || strings.Join(plan.UserChoice.Fields, ",") != "delivery,review,base,target" {
		t.Fatalf("scoped preference provenance lost: %#v", plan)
	}
	for _, grant := range plan.Grants {
		if grant.Value != "ask" {
			t.Fatalf("workspace context escalated an effect grant: %#v", grant)
		}
	}
	plan.Execution.RootContext = taskplan.RootContextNone
	if err := plan.Validate(); err == nil {
		t.Fatal("scoped choice bypassed frozen root-context validation")
	}
	plan.Execution.RootContext = taskplan.RootContextReadOnly
	plan.UserChoice.Quote = " "
	if err := plan.Validate(); err == nil {
		t.Fatal("workspace context bypassed scoped choice provenance validation")
	}
}
