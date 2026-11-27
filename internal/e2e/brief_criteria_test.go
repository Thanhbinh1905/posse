//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestBriefTicketReferencesInDoneWhen(t *testing.T) {
	f := reviewIdentityFixture(t)
	writeBrief := func(t *testing.T, criteria string) (string, string) {
		t.Helper()
		path := filepath.Join(f.root, "criteria.md")
		text := "---\ntype: change\ntitle: Verify Brief criteria\n" + criteria + "\n---\nVerify the criteria.\n"
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path, text
	}
	workerBrief := func(t *testing.T, task store.Task) string {
		t.Helper()
		env := setEnv(f.env, "HERDR_PANE_ID", task.PaneID)
		env = setEnv(env, "POSSE_WORKER_HOME", f.home)
		output := runPosse(t, f.binary, task.WorktreePath, env, "brief", "--json")
		var response struct {
			Brief string `json:"brief"`
		}
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			t.Fatalf("decode posse brief output: %v\n%s", err, output)
		}
		return response.Brief
	}
	tasks := func(t *testing.T) []store.Task {
		t.Helper()
		items, err := f.db.Tasks(context.Background(), f.project.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		return items
	}

	start := func(t *testing.T, path string) (store.Task, string) {
		t.Helper()
		if code, out := runReviewIdentityCLI(t, f, "start", path, "--profile", "author"); code != 0 {
			t.Fatalf("start rejected the Brief: %s", out)
		}
		items := tasks(t)
		task := items[len(items)-1]
		taskPath := filepath.Join(f.home, "projects", f.project.Name, "tasks", "t"+strconv.Itoa(task.Seq))
		return task, taskPath
	}

	t.Run("linked ticket in Worker Brief", func(t *testing.T) {
		path, original := writeBrief(t, "ticket: 1328\nrefs: [1144]\ndone_when: Verify the linked ticket")
		task, taskPath := start(t, path)
		launch := readTestFile(t, filepath.Join(taskPath, "launch.md"))
		output := workerBrief(t, task)
		for _, view := range []struct{ name, text string }{{"launch.md", launch}, {"posse brief", output}} {
			if !slices.Contains(strings.Split(view.text, "\n"), "Ticket: #1328") {
				t.Errorf("%s lacks Ticket: #1328", view.name)
			}
			if strings.Contains(view.text, "#1144") {
				t.Errorf("%s promotes the non-closing reference", view.name)
			}
		}
		if got := readTestFile(t, filepath.Join(taskPath, "brief.md")); got != original {
			t.Errorf("stored original Brief changed: %q", got)
		}
	})

	t.Run("quoted criteria and original Brief", func(t *testing.T) {
		path, original := writeBrief(t, `refs: [1328]
done_when: # acceptance criteria
  - "Use the exact-head waiver option in #1087, then verify its scope." # reviewer note
  - 'Verify #1144 before changing final-fix behavior.' # follow-up
  - Keep existing coverage # 2 suites`)
		task, taskPath := start(t, path)
		output := workerBrief(t, task)
		if strings.Contains(output, "\nTicket:") {
			t.Error("posse brief rendered a ticket for a refs-only Brief")
		}
		want := "Done when: Use the exact-head waiver option in #1087, then verify its scope.; Verify #1144 before changing final-fix behavior.; Keep existing coverage"
		if !slices.Contains(strings.Split(output, "\n"), want) {
			t.Errorf("posse brief lost criteria or included comments: %s", output)
		}
		originalPath := filepath.Join(taskPath, "brief.md")
		if !strings.Contains(output, "Original Brief: `"+originalPath+"`") {
			t.Errorf("posse brief does not point to the original Brief at %s", originalPath)
		}
		if got := readTestFile(t, originalPath); got != original {
			t.Errorf("stored original Brief changed: %q", got)
		}
	})

	for _, tc := range []struct{ ticket, criteria, truncated string }{
		{"#1087", "Use the exact-head waiver option in #1087, then verify its scope.\n  - Changed lines pass review", "Use the exact-head waiver option in; Changed lines pass review"},
		{"#1144", "Verify #1144 before changing final-fix behavior.", "Verify"},
	} {
		t.Run(tc.ticket, func(t *testing.T) {
			before := tasks(t)
			path, _ := writeBrief(t, "done_when:\n  - "+tc.criteria)
			code, output := runReviewIdentityCLI(t, f, "start", path, "--profile", "author")
			after := tasks(t)
			if code == 0 {
				if len(after) > len(before) && slices.Contains(strings.Split(workerBrief(t, after[len(after)-1]), "\n"), "Done when: "+tc.truncated) {
					t.Fatalf("start admitted %s; posse brief shows truncated criteria %q", tc.ticket, tc.truncated)
				}
				t.Fatalf("start admitted ambiguous criterion containing %s", tc.ticket)
			}
			for _, want := range []string{"brief_invalid", "done_when", "line 5", tc.ticket, "quote the entire criterion"} {
				if !strings.Contains(output, want) {
					t.Errorf("start refusal lacks %q: %s", want, output)
				}
			}
			if len(after) != len(before) {
				t.Fatalf("start left %d Tasks; want %d", len(after), len(before))
			}
		})
	}
}
