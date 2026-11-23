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
	binary := os.Getenv("POSSE_T1258_GREENHOUSE_BIN")
	if binary == "" {
		t.Skip("set POSSE_T1258_GREENHOUSE_BIN to the absolute path of a real Greenhouse binary")
	}
	f := newWorkerTabsFixture(t)
	if err := os.WriteFile(filepath.Join(f.repo, ".gitattributes"), []byte("schema.sql linguist-generated greenhouse-generator=tools/schema.go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, f.repo, "add", ".gitattributes")
	gitTest(t, f.env, f.repo, "commit", "-q", "-m", "declare generated SQL")
	gitTest(t, f.env, f.repo, "push", "origin", "main")
	c := newGreenhouseContract(t, f)
	c.change = f.start(t, "t1", "Greenhouse partial coverage", "greenhouse-partial")
	large := strings.Repeat("CREATE TABLE example (id integer);\n", 5000)
	if err := os.MkdirAll(filepath.Join(c.change.WorktreePath, "tools"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"large.sql":       large,
		"reviewed.go":     "package sample\n\nconst reviewed = true\n",
		"schema.sql":      "CREATE TABLE generated_example (id integer);\n",
		"tools/schema.go": "package tools\n",
	} {
		if err := os.WriteFile(filepath.Join(c.change.WorktreePath, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, f.env, c.change.WorktreePath, "add", "large.sql", "reviewed.go", "schema.sql", "tools/schema.go")
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
	invocations, err := os.ReadFile(filepath.Join(c.ctl, "invocations"))
	if err != nil || len(strings.Fields(string(invocations))) != 1 {
		t.Fatalf("partial review reviewer invocations = %q, %v; want one", invocations, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if result["verdict"] != "partial" {
		attached := c.greenhouse(t, map[string]any{"version": 2, "op": "attach", "host": map[string]any{"name": "posse", "id": "shop/t1/shop"}})
		inspection := c.greenhouse(t, map[string]any{"version": 2, "op": "inspect", "run_id": attached["run_id"]})
		encodedInspection, _ := json.Marshal(inspection)
		t.Fatalf("real Greenhouse Verdict = %v, want partial: %s\ninspection: %s", result["verdict"], encoded, encodedInspection)
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
	if path["path"] != "large.sql" || reason != "hunk windows exceed 65536 bytes" {
		t.Fatalf("real Greenhouse uncovered path = %s", encodedResult)
	}
	generated, _ := hostResult["generated"].([]any)
	if len(generated) != 1 {
		t.Fatalf("real Greenhouse generated paths = %s", encodedResult)
	}
	generatedPath, _ := generated[0].(map[string]any)
	generators, _ := generatedPath["generators"].([]any)
	if generatedPath["path"] != "schema.sql" || len(generators) != 1 || generators[0] != "tools/schema.go" {
		t.Fatalf("real Greenhouse generated path = %s", encodedResult)
	}
	decisions, err := c.posse(c.env, f.repo, "decisions", "--json")
	if err != nil {
		t.Fatalf("list partial-review Decisions: %v\n%s", err, decisions)
	}
	for _, expected := range []string{"greenhouse_coverage", "accept-coverage-at-exact-head", "hold", result["head"].(string), "large.sql", "hunk windows exceed 65536 bytes", "Generated paths were reviewed by provenance", "schema.sql", "tools/schema.go"} {
		if !strings.Contains(decisions, expected) {
			t.Fatalf("partial-review Decision omitted %q: %s", expected, decisions)
		}
	}
	if got := c.notices(t, "greenhouse_review_failed"); got != 0 {
		t.Fatalf("partial review created %d greenhouse_review_failed Notices", got)
	}
	if strings.Contains(decisions, "schema.sql: hunk windows exceed") {
		t.Fatalf("generated path appeared among uncovered paths: %s", decisions)
	}
	if got := c.notices(t, "greenhouse_partial_review"); got != 1 {
		t.Fatalf("partial review Notices = %d, want 1", got)
	}
}
