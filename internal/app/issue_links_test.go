package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestDispatchWarnsForMissingAndClosedLinkedIssues(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte("[defaults]\nlanding_mode = \"pr\"\nmerge_method = \"squash\"\nauto_unsaddle = \"never\"\n\n[profiles.deep]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"deep\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	briefPath := filepath.Join(fixture.root, "linked-issues.md")
	brief := "---\ntype: ship\ntitle: Fix linked issues\ndone_when: dispatch warns about issue state\nissues: [12]\nrefs: [13]\n---\nWork on the linked issues.\n"
	if err := os.WriteFile(briefPath, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output, stderr := fixture.run("dispatch", "--brief", briefPath)
	if code != 0 {
		t.Fatalf("dispatch: %d %s %s", code, output, stderr)
	}
	for _, warning := range []string{"issue #12 is already closed", "issue #13 does not exist"} {
		if !strings.Contains(output, warning) {
			t.Fatalf("dispatch omitted warning %q: %s", warning, output)
		}
	}
}

func TestShowAndFullRosterDisplayIssueLinks(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	briefPath := filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "brief.md")
	brief := "---\ntype: ship\ntitle: E2E Brief title\ndone_when: commit exists\nissues: [12, 16]\nrefs: [18]\n---\nE2E intent\n"
	if err := os.WriteFile(briefPath, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"show", "t1"}, {"roster", "--full"}} {
		code, output, stderr := fixture.run(args...)
		if code != 0 {
			t.Fatalf("%v: %d %s %s", args, code, output, stderr)
		}
		for _, issue := range []string{"#12", "#16", "#18"} {
			if !strings.Contains(output, issue) {
				t.Fatalf("%v omitted linked issue %s: %s", args, issue, output)
			}
		}
	}
}

func TestIssueReferenceScopeMatchesProjectAndShipMembers(t *testing.T) {
	workspace := store.Project{Kind: store.ProjectKindWorkspace}
	targets := []repoTarget{{Name: "worker"}, {Name: "api"}}
	brief, err := dispatch.ParseBriefText("---\ntype: ship\ntitle: Update issues\ndone_when: issue scope is valid\nrepos: [worker]\nissues: [worker#12]\n---\nUpdate the member.\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateBriefIssueReferences(workspace, brief, targets); err != nil {
		t.Fatalf("valid workspace reference was rejected: %v", err)
	}
	if err := validateShipIssueTargets(brief, []repoTarget{{Name: "api"}}); err == nil {
		t.Fatal("Ship Brief linked an issue in a member without a PR")
	}
	brief.Type = "scout"
	if err := validateShipIssueTargets(brief, nil); err != nil {
		t.Fatalf("non-Ship Brief was restricted to its task members: %v", err)
	}
	if err := validateBriefIssueReferences(store.Project{}, brief, targets); err == nil {
		t.Fatal("repository Project accepted a workspace member reference")
	}
}

func TestWorkspacePRIssueLinksUseMemberIssueNumbers(t *testing.T) {
	brief, err := dispatch.ParseBriefText("---\ntype: ship\ntitle: Update workspace issues\ndone_when: member links are scoped\nissues: [worker#12, api#14]\nrefs: [worker#16]\n---\nUpdate the workspace.\n")
	if err != nil {
		t.Fatal(err)
	}
	workerBody := issueLinkBody(brief, "worker")
	if workerBody != "## Issue Link\n\nCloses #12\nRefs #16\n" {
		t.Fatalf("worker PR links = %q", workerBody)
	}
	apiBody := issueLinkBody(brief, "api")
	if apiBody != "## Issue Link\n\nCloses #14\n" {
		t.Fatalf("api PR links = %q", apiBody)
	}
}
