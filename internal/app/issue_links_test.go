package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestGitHubIssueBlockersReadPaginatedCanonicalRelationships(t *testing.T) {
	bin := t.TempDir()
	output := `[[{"number":17,"state":"open","repository_url":"https://api.github.com/repos/acme/shop","html_url":"https://github.com/acme/shop/issues/17"}],[{"number":18,"state":"closed","repository_url":"https://api.github.com/repos/acme/shop","html_url":"https://github.com/acme/shop/issues/18"}]]`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf '%s' '"+output+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	blockers, err := githubIssueBlockers(context.Background(), repositoryForge{Kind: "github", Host: "github.com", Path: "acme/shop", Root: t.TempDir()}, 12)
	if err != nil || len(blockers) != 2 || blockers[0].Number != 17 || blockers[1].State != "closed" {
		t.Fatalf("paginated blockers=%+v err=%v", blockers, err)
	}
}

func TestGitHubIssueBlockersFailClosedOnInvalidPageOrRepository(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
	}{
		{name: "malformed pagination", output: `[[]`},
		{name: "different repository", output: `[[{"number":17,"state":"open","repository_url":"https://api.github.com/repos/other/repo"}]]`},
		{name: "unknown state", output: `[[{"number":17,"repository_url":"https://api.github.com/repos/acme/shop"}]]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf '%s' '"+tc.output+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if _, err := githubIssueBlockers(context.Background(), repositoryForge{Kind: "github", Host: "github.com", Path: "acme/shop", Root: t.TempDir()}, 12); err == nil {
				t.Fatal("invalid or non-canonical blocker data was accepted")
			}
		})
	}
}

func TestTicketBlockerCheckPreventsDispatchAndFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   string
	}{
		{name: "open blocker", script: "#!/bin/sh\nprintf '%s' '[[{\"number\":17,\"state\":\"open\",\"repository_url\":\"https://api.github.com/repos/acme/shop\"}]]'\n", want: "ticket_blocked"},
		{name: "forge unavailable", script: "#!/bin/sh\necho unavailable >&2\nexit 1\n", want: "ticket_blockers_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newStartFixture(t)
			gitTest(t, fixture.repo, "remote", "add", "origin", "https://github.com/acme/shop.git")
			cfg, err := config.Load(fixture.home, "shop")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(tc.script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			brief := dispatch.Brief{Type: "change", Ticket: &dispatch.IssueRef{Number: 12}}
			err = validateTicketBlockers(context.Background(), cfg, brief, []repoTarget{{Name: "shop", Root: fixture.repo}})
			var failure *axi.Error
			if err == nil || !errors.As(err, &failure) || failure.Code != tc.want {
				t.Fatalf("blocker validation err=%v, want code %s", err, tc.want)
			}
		})
	}
}

func TestDispatchWarnsForMissingAndClosedLinkedIssues(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte("[defaults]\nmerge_mode = \"pr\"\nmerge_method = \"squash\"\nauto_teardown = \"never\"\n\n[profiles.deep]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"deep\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	briefPath := filepath.Join(fixture.root, "linked-issues.md")
	brief := "---\ntype: change\ntitle: Fix linked issues\ndone_when: dispatch warns about issue state\nticket: 12\nrefs: [13]\n---\nWork on the linked issues.\n"
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

func TestDispatchRejectsMultipleClosingIssues(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte("[defaults]\nmerge_mode = \"pr\"\nmerge_method = \"squash\"\nauto_teardown = \"never\"\n\n[profiles.deep]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"deep\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	briefPath := filepath.Join(fixture.root, "multiple-issues.md")
	brief := "---\ntype: change\ntitle: Reject multiple issues\ndone_when: dispatch enforces one ticket\nissues: [12, 16]\n---\nDo the work.\n"
	if err := os.WriteFile(briefPath, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output, _ := fixture.run("dispatch", "--brief", briefPath)
	if code == 0 || !strings.Contains(output, "only one closing issue") {
		t.Fatalf("dispatch accepted multiple closing issues: code=%d output=%s", code, output)
	}
}

func TestShowAndFullListDisplayIssueLinks(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	briefPath := filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "brief.md")
	brief := "---\ntype: change\ntitle: E2E Brief title\ndone_when: commit exists\nissues: [12, 16]\nrefs: [18]\n---\nE2E intent\n"
	if err := os.WriteFile(briefPath, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"show", "t1"}, {"list", "--full"}} {
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

func TestIssueReferenceScopeMatchesProjectAndChangeMembers(t *testing.T) {
	workspace := store.Project{Kind: store.ProjectKindWorkspace}
	targets := []repoTarget{{Name: "worker"}, {Name: "api"}}
	brief, err := dispatch.ParseBriefText("---\ntype: change\ntitle: Update issues\ndone_when: issue scope is valid\nrepos: [worker]\nticket: worker#12\n---\nUpdate the member.\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateBriefIssueReferences(workspace, brief, targets); err != nil {
		t.Fatalf("valid workspace reference was rejected: %v", err)
	}
	if err := validateChangeIssueTargets(brief, []repoTarget{{Name: "api"}}); err == nil {
		t.Fatal("Change Brief linked an issue in a member without a PR")
	}
	brief.Type = "research"
	if err := validateChangeIssueTargets(brief, nil); err != nil {
		t.Fatalf("non-Change Brief was restricted to its task members: %v", err)
	}
	if err := validateBriefIssueReferences(store.Project{}, brief, targets); err == nil {
		t.Fatal("repository Project accepted a workspace member reference")
	}
}

func TestHistoricalMultiIssueLinksAreRestoredWhenAdoptingRequest(t *testing.T) {
	brief, err := dispatch.ParseHistoricalBriefText("---\ntype: change\ntitle: Historical workspace issues\ndone_when: old Task compatibility\nissues: [worker#12, api#14]\nrefs: [worker#16, api#18]\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	workerBody, changed := appendMissingIssueLinks("Existing PR description", brief, "worker")
	if !changed || workerBody != "Existing PR description\n\n## Issue Link\n\nCloses #12\nRefs #16\n" {
		t.Fatalf("adopted worker PR links = %q, changed=%v", workerBody, changed)
	}
	apiBody, changed := appendMissingIssueLinks("Existing MR description", brief, "api")
	if !changed || apiBody != "Existing MR description\n\n## Issue Link\n\nCloses #14\nRefs #18\n" {
		t.Fatalf("adopted api MR links = %q, changed=%v", apiBody, changed)
	}
}

func TestWorkspacePRIssueLinksUseMemberIssueNumbers(t *testing.T) {
	brief, err := dispatch.ParseBriefText("---\ntype: change\ntitle: Update workspace issues\ndone_when: member links are scoped\nticket: worker#12\nrepos: [worker, api]\nrefs: [worker#16]\n---\nUpdate the workspace.\n")
	if err != nil {
		t.Fatal(err)
	}
	workerBody := issueLinkBody(brief, "worker")
	if workerBody != "## Issue Link\n\nCloses #12\nRefs #16\n" {
		t.Fatalf("worker PR links = %q", workerBody)
	}
	apiBody := issueLinkBody(brief, "api")
	if apiBody != "" {
		t.Fatalf("ticket was rendered outside its owning member: %q", apiBody)
	}
}
