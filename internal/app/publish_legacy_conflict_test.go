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

func TestT199LegacyHumanEdit(t *testing.T) {
	for _, forge := range []string{"github", "gitlab"} {
		t.Run(forge, func(t *testing.T) {
			var fixture *prLandingFixture
			if forge == "gitlab" {
				fixture = gitlabFixture(t, store.StateWorking)
			} else {
				fixture = newPRLandingFixture(t, "pr", store.StateWorking)
			}
			_, legacyBody, err := prDetails(context.Background(), fixture.db, fixture.project, fixture.task, fixture.service.homePath, "", "First summary", "first tests -> pass", "first proof", "Risk: first\nRollback: first")
			if err != nil {
				t.Fatal(err)
			}
			legacyBody = strings.Replace(legacyBody, "## Verification", "## Maintainer Review\n\nHuman release note: deployment needs approval.\n\n## Verification", 1)

			var before string
			if forge == "gitlab" {
				state, err := os.ReadFile(fixture.ghState)
				if err != nil {
					t.Fatal(err)
				}
				var mr map[string]any
				if err := json.Unmarshal(state, &mr); err != nil {
					t.Fatal(err)
				}
				mr["title"], mr["description"] = "E2E Brief title", legacyBody
				state, err = json.Marshal(mr)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fixture.ghState, state, 0o600); err != nil {
					t.Fatal(err)
				}
				request, err := json.Marshal([]map[string]any{{
					"web_url": "https://git.example.com/group/sub/shop/-/merge_requests/17",
					"title":   "E2E Brief title", "description": legacyBody, "state": "opened",
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
				before = string(state)
			} else {
				t.Setenv("POSSE_TEST_GH_BODY", legacyBody)
				openPR := `[{"url":"https://github.com/acme/shop/pull/17","headRefName":"posse/t1","headRefOid":"` + fixture.headSHA + `"}]`
				if err := os.WriteFile(fixture.ghOpenPRs, []byte(openPR), 0o600); err != nil {
					t.Fatal(err)
				}
				before = legacyBody
			}

			code, output, stderr := fixture.run("publish", "--refresh", "Second summary", "--verify", "second tests -> pass", "--proof", "second proof", "--risk", "Risk: second\nRollback: second")
			if code != 1 || !strings.Contains(output, "pr_body_conflict") {
				t.Fatalf("publish --refresh did not refuse the interleaved human section: code=%d output=%s stderr=%s", code, output, stderr)
			}
			if !strings.Contains(legacyBody, "Human release note: deployment needs approval.") {
				t.Fatal("fixture omitted the human release note")
			}
			var calls []byte
			calls, err = os.ReadFile(fixture.ghLog)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(calls), "pr edit") || strings.Contains(string(calls), "pr create") || strings.Contains(string(calls), "--method PUT") || strings.Contains(string(calls), "mr create") {
				t.Fatalf("conflicting legacy description was changed or a PR/MR was created: %s", calls)
			}
			if forge == "gitlab" {
				state, err := os.ReadFile(fixture.ghState)
				if err != nil || string(state) != before {
					t.Fatalf("GitLab description/title changed on conflict: %s (want %s): %v", state, before, err)
				}
			} else if got := os.Getenv("POSSE_TEST_GH_BODY"); got != before {
				t.Fatalf("GitHub description changed on conflict: %s (want %s)", got, before)
			}
			task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
			if err != nil || task.State != store.StateWorking || task.PRURL != "" {
				t.Fatalf("refusal changed Task state or PR URL: %#v %v", task, err)
			}
		})
	}
}
