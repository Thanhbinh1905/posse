//go:build e2e && linux

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRealGreenhousePartialReviewWithOversizedFile(t *testing.T) {
	binary := os.Getenv("POSSE_T1237_GREENHOUSE_BIN")
	if binary == "" {
		t.Skip("set POSSE_T1237_GREENHOUSE_BIN to the absolute path of a real Greenhouse binary")
	}
	f := newWorkerTabsFixture(t)
	c := newGreenhouseContract(t, f)
	c.change = f.start(t, "t1", "Greenhouse partial coverage", "greenhouse-partial")
	large := strings.Repeat("CREATE TABLE example (id integer);\n", 5000)
	for name, body := range map[string]string{"large.sql": large, "reviewed.go": "package sample\n\nconst reviewed = true\n"} {
		if err := os.WriteFile(filepath.Join(c.change.WorktreePath, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, f.env, c.change.WorktreePath, "add", "large.sql", "reviewed.go")
	gitTest(t, f.env, c.change.WorktreePath, "commit", "-q", "-m", "oversized SQL candidate")
	if err := os.WriteFile(filepath.Join(c.ctl, "prefix-input"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	output, reviewErr := c.posse(c.env, c.f.repo, c.reviewArgs()...)
	if reviewErr != nil {
		invocations, _ := os.ReadFile(filepath.Join(c.ctl, "invocations"))
		attached := c.greenhouse(t, map[string]any{"version": 2, "op": "attach", "host": map[string]any{"name": "posse", "id": "shop/t1/shop"}})
		inspection := c.greenhouse(t, map[string]any{"version": 2, "op": "inspect", "run_id": attached["run_id"]})
		encodedInspection, _ := json.Marshal(inspection)
		t.Fatalf("real Greenhouse review failed: %v\n%s\nreviewer invocations: %s\ninspection: %s", reviewErr, output, invocations, encodedInspection)
	}
	result := decodeGreenhouseOutput(t, output)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if result["verdict"] != "partial" {
		t.Fatalf("real Greenhouse Verdict = %v, want partial: %s", result["verdict"], encoded)
	}
	attached := c.greenhouse(t, map[string]any{"version": 2, "op": "attach", "host": map[string]any{"name": "posse", "id": "shop/t1/shop"}})
	inspection := c.greenhouse(t, map[string]any{"version": 2, "op": "inspect", "run_id": attached["run_id"]})
	hostResult, ok := inspection["host_result"].(map[string]any)
	encodedResult, _ := json.Marshal(hostResult)
	if !ok || hostResult["verdict"] != "partial" || hostResult["status"] != "partial-review" {
		t.Fatalf("real Greenhouse host result = %s", encodedResult)
	}
	uncovered, _ := hostResult["uncovered"].([]any)
	if len(uncovered) != 1 {
		t.Fatalf("real Greenhouse uncovered paths = %s", encodedResult)
	}
	path, _ := uncovered[0].(map[string]any)
	reason, _ := path["reason"].(string)
	if path["path"] != "large.sql" || strings.TrimSpace(reason) == "" {
		t.Fatalf("real Greenhouse uncovered path = %s", encodedResult)
	}
	decisions, err := c.posse(c.env, f.repo, "decisions", "--json")
	if err != nil {
		t.Fatalf("list partial-review Decisions: %v\n%s", err, decisions)
	}
	for _, expected := range []string{"greenhouse_coverage", "accept-coverage-at-exact-head", "hold", "large.sql"} {
		if !strings.Contains(decisions, expected) {
			t.Fatalf("partial-review Decision omitted %q: %s", expected, decisions)
		}
	}
	if got := c.notices(t, "greenhouse_review_failed"); got != 0 {
		t.Fatalf("partial review created %d greenhouse_review_failed Notices", got)
	}
	if got := c.notices(t, "greenhouse_partial_review"); got != 1 {
		t.Fatalf("partial review Notices = %d, want 1", got)
	}
}
