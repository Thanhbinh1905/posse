package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestT203LegacyHumanSectionsAppendWithoutDataLoss(t *testing.T) {
	insertions := []struct {
		name, text string
	}{
		{"ATX", "## Maintainer Review\n\nHuman release note: deployment needs approval.\n\n"},
		{"Setext-one", "Maintainer Review\n=\n\nHuman release note: deployment needs approval.\n\n"},
		{"Setext-two", "Maintainer Review\n--\n\nHuman release note: deployment needs approval.\n\n"},
		{"HTML", "<h2>Maintainer Review</h2>\n\nHuman release note: deployment needs approval.\n\n"},
		{"paragraph", "Human release note: deployment needs approval.\n\n"},
	}
	locations := []struct {
		name, next string
	}{
		{"Summary", "## Issue Link"},
		{"Proof", "## Risk And Rollback"},
		{"Risk", "## Documentation"},
	}
	for _, forge := range []string{"github", "gitlab"} {
		for _, location := range locations {
			for _, insertion := range insertions {
				t.Run(forge+"/"+location.name+"/"+insertion.name, func(t *testing.T) {
					fixture, body := t201LegacyFixture(t, forge)
					body = strings.Replace(body, location.next, insertion.text+location.next, 1)
					priorForgeState := t201SetLegacyBody(t, fixture, forge, body)
					briefPath := filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "brief.md")
					brief := readFileForPublishTest(t, briefPath)
					if err := os.WriteFile(briefPath, []byte(strings.Replace(brief, "title: E2E Brief title", "title: Updated E2E title", 1)), 0o600); err != nil {
						t.Fatal(err)
					}
					t.Setenv("POSSE_TEST_GH_EDIT_TITLE", filepath.Join(fixture.root, "gh-edited-title"))
					if err := os.WriteFile(fixture.ghLog, nil, 0o600); err != nil {
						t.Fatal(err)
					}

					code, output, stderr := fixture.run("publish", "--refresh", "Second summary", "--verify", "second tests -> pass", "--proof", "second proof", "--risk", "Risk: second\nRollback: second")
					if code != 0 || strings.Contains(output, "pr_body_conflict") {
						t.Fatalf("publish --refresh did not append an owned block: code=%d output=%s stderr=%s", code, output, stderr)
					}
					calls := readFileForPublishTest(t, fixture.ghLog)
					if strings.Contains(calls, "pr create") || strings.Contains(calls, "mr create") {
						t.Fatalf("refresh created a duplicate request: %s", calls)
					}
					if (forge == "github" && !strings.Contains(calls, "pr edit")) || (forge == "gitlab" && !strings.Contains(calls, "--method PUT")) {
						t.Fatalf("refresh did not write the managed block: %s", calls)
					}

					updatedPath := os.Getenv("POSSE_TEST_GH_EDIT_BODY")
					if forge == "gitlab" {
						updatedPath = os.Getenv("POSSE_TEST_GLAB_DESCRIPTION")
					}
					refreshed := readFileForPublishTest(t, updatedPath)
					if forge == "gitlab" {
						if state := readFileForPublishTest(t, fixture.ghState); state != priorForgeState {
							t.Fatalf("GitLab read state changed unexpectedly: got %s, want %s", state, priorForgeState)
						}
					}
					if !strings.HasPrefix(refreshed, body) {
						t.Fatalf("legacy body was not preserved byte-for-byte: got %s, want prefix %s", refreshed, body)
					}
					marker, err := fixture.db.GetPRBodyMarker(context.Background(), fixture.task.ID, "")
					if err != nil {
						t.Fatal(err)
					}
					managed, ok := managedPublishSection(refreshed, marker.Token)
					if !ok || strings.Count(refreshed, publishBodyMarker("start", marker.Token)) != 1 || strings.Count(refreshed, publishBodyMarker("end", marker.Token)) != 1 {
						t.Fatalf("refresh did not append exactly one managed block: %s", refreshed)
					}
					for _, expected := range []string{"Second summary", "second tests -> pass", "second proof", "Risk: second", "Rollback: second"} {
						if !strings.Contains(managed, expected) {
							t.Errorf("managed body omitted %q: %s", expected, managed)
						}
					}
					titlePath := os.Getenv("POSSE_TEST_GH_EDIT_TITLE")
					if forge == "gitlab" {
						titlePath = os.Getenv("POSSE_TEST_GLAB_TITLE")
					}
					if title := readFileForPublishTest(t, titlePath); title != "Updated E2E title" {
						t.Errorf("refreshed title=%q want %q", title, "Updated E2E title")
					}
					task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
					if err != nil || task.State != store.StateWorking {
						t.Fatalf("refresh changed Task state: %#v %v", task, err)
					}
				})
			}
		}
	}
}

func t201LegacyFixture(t *testing.T, forge string) (*prLandingFixture, string) {
	t.Helper()
	var fixture *prLandingFixture
	if forge == "gitlab" {
		fixture = gitlabFixture(t, store.StateWorking)
	} else {
		fixture = newPRLandingFixture(t, "pr", store.StateWorking)
	}
	_, body, err := prDetails(context.Background(), fixture.db, fixture.project, fixture.task, fixture.service.homePath, "", "First summary", "first tests -> pass", "first proof", "Risk: first\nRollback: first")
	if err != nil {
		t.Fatal(err)
	}
	return fixture, body
}

func t201SetLegacyBody(t *testing.T, fixture *prLandingFixture, forge, body string) string {
	t.Helper()
	if forge == "gitlab" {
		state, err := os.ReadFile(fixture.ghState)
		if err != nil {
			t.Fatal(err)
		}
		var mr map[string]any
		if err := json.Unmarshal(state, &mr); err != nil {
			t.Fatal(err)
		}
		mr["title"], mr["description"] = "E2E Brief title", body
		state, err = json.Marshal(mr)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixture.ghState, state, 0o600); err != nil {
			t.Fatal(err)
		}
		request, err := json.Marshal([]map[string]any{{
			"web_url": "https://git.example.com/group/sub/shop/-/merge_requests/17",
			"title":   "E2E Brief title", "description": body, "state": "opened",
			"sha": fixture.headSHA, "source_branch": "posse/t1", "target_branch": "main",
			"source_project_id": 7, "target_project_id": 7,
		}})
		if err != nil {
			t.Fatal(err)
		}
		listPath := filepath.Join(fixture.root, "existing-mrs.json")
		if err := os.WriteFile(listPath, request, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("POSSE_TEST_GLAB_LIST", listPath)
		return string(state)
	}

	t.Setenv("POSSE_TEST_GH_BODY", body)
	openPR, err := json.Marshal([]map[string]string{{
		"url": "https://github.com/acme/shop/pull/17", "headRefName": "posse/t1", "headRefOid": fixture.headSHA,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.ghOpenPRs, openPR, 0o600); err != nil {
		t.Fatal(err)
	}
	return ""
}

func readFileForPublishTest(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}
