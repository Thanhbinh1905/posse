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

func TestT201LegacyInsertedContentConflicts(t *testing.T) {
	insertions := map[string]string{
		"setext heading": "Maintainer Review\n=\n\nKeep this note.\n\n",
		"HTML heading":   "<h2>Maintainer Review</h2>\n\nKeep this note.\n\n",
		"arbitrary text": "Maintainer review requires deployment approval.\n\n",
	}
	for _, forge := range []string{"github", "gitlab"} {
		for name, insertion := range insertions {
			t.Run(forge+"/"+name, func(t *testing.T) {
				fixture, body := t201LegacyFixture(t, forge)
				body = strings.Replace(body, "## Verification", insertion+"## Verification", 1)
				priorForgeState := t201SetLegacyBody(t, fixture, forge, body)
				if err := os.WriteFile(fixture.ghLog, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				callsBefore := readFileForPublishTest(t, fixture.ghLog)

				code, output, stderr := fixture.run("publish", "--refresh", "Second summary", "--verify", "second tests -> pass", "--proof", "second proof", "--risk", "Risk: second\nRollback: second")
				if code != 1 || !strings.Contains(output, "pr_body_conflict") {
					t.Fatalf("publish --refresh did not reject inserted content: code=%d output=%s stderr=%s", code, output, stderr)
				}
				calls := strings.TrimPrefix(readFileForPublishTest(t, fixture.ghLog), callsBefore)
				if strings.Contains(calls, "pr edit") || strings.Contains(calls, "pr create") || strings.Contains(calls, "--method PUT") || strings.Contains(calls, "mr create") {
					t.Fatalf("conflicting legacy description was edited or a request was created: %s", calls)
				}
				if forge == "gitlab" {
					state := readFileForPublishTest(t, fixture.ghState)
					if state != priorForgeState {
						t.Fatalf("GitLab metadata changed on conflict: got %s, want %s", state, priorForgeState)
					}
				} else if got := os.Getenv("POSSE_TEST_GH_BODY"); got != body {
					t.Fatalf("GitHub description changed on conflict: got %s, want %s", got, body)
				}
				task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
				if err != nil || task.State != store.StateWorking || task.PRURL != "" {
					t.Fatalf("conflict changed Task state or PR URL: %#v %v", task, err)
				}
			})
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
