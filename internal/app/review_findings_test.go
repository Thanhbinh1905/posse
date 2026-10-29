package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func TestDecodeReviewFindingsEnforcesSchemaAndFailClosedDefaults(t *testing.T) {
	valid := []byte(`{"schema":"posse.review-findings.v1","patterns":[{"title":"Dropped errors","rule":"Check writes","root_cause":"Errors are ignored","fix_guidance":"Check all calls","instances":[{"repository":"shop","path":"internal/x.go","excerpt":"db.Exec()"}]}]}`)
	parsed, err := decodeReviewFindings(valid)
	if err != nil {
		t.Fatal(err)
	}
	pattern := (*parsed.Patterns)[0]
	if pattern.Severity != "unclassified" || pattern.Origin != "introduced" {
		t.Fatalf("fail-closed defaults = severity %q origin %q", pattern.Severity, pattern.Origin)
	}
	for _, malformed := range [][]byte{
		[]byte(`{"schema":"wrong","patterns":[]}`),
		[]byte(`{"schema":"posse.review-findings.v1","patterns":[]}{}`),
		[]byte(`{"schema":"posse.review-findings.v1","patterns":[],"unknown":true}`),
		[]byte(`{"schema":"posse.review-findings.v1"}`),
	} {
		if _, err := decodeReviewFindings(malformed); err == nil {
			t.Fatalf("malformed findings accepted: %s", malformed)
		}
	}
}

func TestFindingAnchorsNormalizeWhitespaceAndScopeChangedLines(t *testing.T) {
	contents := []byte("package p\n\nfunc save() {\n\tdb.Exec(ctx, q)\n}\n")
	anchors, err := findAnchoredExcerpts(contents, "db.Exec(ctx,\n q)")
	if err != nil || len(anchors) != 1 || anchors[0] != (anchoredFindingExcerpt{Start: 4, End: 4}) {
		t.Fatalf("anchor = %#v, %v", anchors, err)
	}
	if _, err := findAnchoredExcerpts(contents, "not present"); err == nil {
		t.Fatal("unanchored excerpt accepted")
	}
	patch := []byte("diff --git a/internal/x.go b/internal/x.go\n--- a/internal/x.go\n+++ b/internal/x.go\n@@ -2,2 +2,3 @@\n context\n-old()\n+db.Exec(ctx, q)\n+second()\n")
	changed := changedLinesFromPatch(patch, []string{"shop"}, false)
	if !changed["shop\x00internal/x.go"][3] || !changed["shop\x00internal/x.go"][4] || changed["shop\x00internal/x.go"][2] {
		t.Fatalf("changed lines = %#v", changed)
	}
	inScope, err := causeNamesChangedLine("internal/x.go:3", "shop", changed, false)
	if err != nil || !inScope {
		t.Fatalf("cause scope = %v, %v", inScope, err)
	}
	if _, err := causeNamesChangedLine("not-a-hunk", "shop", changed, false); err == nil {
		t.Fatal("unparseable cause accepted")
	}
	unchangedAnchor := []anchoredFindingExcerpt{{Start: 1, End: 1}}
	if inScope, err := classifyReviewFindingScope(1, "introduced", true, "shop", "README.md", unchangedAnchor, "", changed, nil, false); err != nil || !inScope {
		t.Fatalf("round-one introduced finding scope = %v, %v", inScope, err)
	}
	if inScope, err := classifyReviewFindingScope(1, "pre_existing", true, "shop", "README.md", unchangedAnchor, "", changed, nil, false); err != nil || inScope {
		t.Fatalf("round-one pre-existing finding scope = %v, %v", inScope, err)
	}
	if inScope, err := classifyReviewFindingScope(2, "introduced", true, "shop", "README.md", unchangedAnchor, "", changed, nil, false); err != nil || inScope {
		t.Fatalf("later-round unchanged finding scope = %v, %v", inScope, err)
	}
	if inScope, err := classifyReviewFindingScope(2, "introduced", true, "shop", "README.md", unchangedAnchor, "internal/x.go:3", changed, nil, false); err != nil || !inScope {
		t.Fatalf("later-round cause scope = %v, %v", inScope, err)
	}
	added := changedLineSet{"shop\x00README.md": {1: true}}
	if inScope, err := classifyReviewFindingScope(2, "introduced", true, "shop", "README.md", unchangedAnchor, "", changed, added, false); err != nil || !inScope {
		t.Fatalf("later-round blocking finding on a Change-added line scope = %v, %v", inScope, err)
	}
	if inScope, err := classifyReviewFindingScope(2, "introduced", false, "shop", "README.md", unchangedAnchor, "", changed, added, false); err != nil || inScope {
		t.Fatalf("later-round non-blocking finding outside the delta scope = %v, %v", inScope, err)
	}
	if inScope, err := classifyReviewFindingScope(2, "pre_existing", true, "shop", "README.md", unchangedAnchor, "", changed, added, false); err != nil || inScope {
		t.Fatalf("later-round pre-existing finding outside the delta scope = %v, %v", inScope, err)
	}
	workspacePatch := []byte("diff --git a/member-a/internal/x.go b/member-a/internal/x.go\n--- a/member-a/internal/x.go\n+++ b/member-a/internal/x.go\n@@ -1 +1 @@\n-old\n+new\n")
	workspaceChanges := changedLinesFromPatch(workspacePatch, []string{"member-a", "member-b"}, true)
	if !workspaceChanges["member-a\x00internal/x.go"][1] || workspaceChanges["member-b\x00internal/x.go"][1] {
		t.Fatalf("workspace changed-line scopes overlap: %#v", workspaceChanges)
	}
	if inScope, err := causeNamesChangedLine("member-a/internal/x.go:1", "member-a", workspaceChanges, true); err != nil || !inScope {
		t.Fatalf("workspace cause scope = %v, %v", inScope, err)
	}
	localPrefixPatch := []byte("diff --git a/shop/internal/x.go b/shop/internal/x.go\n--- a/shop/internal/x.go\n+++ b/shop/internal/x.go\n@@ -1 +1 @@\n-old\n+new\n")
	localChanges := changedLinesFromPatch(localPrefixPatch, []string{"shop"}, false)
	if !localChanges["shop\x00shop/internal/x.go"][1] {
		t.Fatalf("local project-name path was treated as a workspace prefix: %#v", localChanges)
	}
}

func TestReviewRoundFindingsAdmissionAnchoringAndVerdictE2E(t *testing.T) {
	f := newStartFixture(t)
	configText := `[defaults]
merge_mode = "local"

[profiles.author]
kind = "claude"
model = "model-a"

[profiles.reviewer]
kind = "claude"
model = "model-b"

[dispatch.default]
use = "author"

[review_loop]
enabled = true
max_rounds = 1
auto_start = false
block_at = "P1"
`
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "origin.git")
	gitTest(t, f.repo, "init", "--bare", remote)
	gitTest(t, f.repo, "remote", "add", "origin", remote)
	gitTest(t, f.repo, "push", "-u", "origin", "main")
	if code, output := f.start(t); code != 0 {
		t.Fatalf("start code=%d %s", code, output)
	}
	change, _ := f.onlyTask(t)
	if err := os.MkdirAll(filepath.Join(change.WorktreePath, "internal", "app"), 0o700); err != nil {
		t.Fatal(err)
	}
	codeText := "package app\n\nfunc save() {\n\tdb.Exec(ctx, q)\n}\n"
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "internal", "app", "x.go"), []byte(codeText), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, change.WorktreePath, "add", "internal/app/x.go")
	gitTest(t, change.WorktreePath, "commit", "-m", "add unchecked write")
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.Transition(ctx, change.ID, change.State, store.StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	var output bytes.Buffer
	cli := f.service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"review", "start", "t1", "--profile", "reviewer"}); code != 0 {
		t.Fatalf("review start code=%d %s", code, output.String())
	}
	db, err = store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	rounds, err := db.ReviewRoundsFor(ctx, change.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("review rounds = %#v, %v", rounds, err)
	}
	review, err := db.TaskByID(ctx, f.project.ID, rounds[0].ReviewTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(review.WorktreePath, "report.md")
	findingsPath := filepath.Join(review.WorktreePath, "findings.json")
	if err := os.WriteFile(reportPath, []byte("Review complete\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runSignal := func(arguments ...string) (int, string) {
		t.Helper()
		previousPane, panePresent := os.LookupEnv("HERDR_PANE_ID")
		previousHome, homePresent := os.LookupEnv(workerHomeEnv)
		_ = os.Setenv("HERDR_PANE_ID", review.PaneID)
		_ = os.Setenv(workerHomeEnv, f.home)
		defer func() {
			if panePresent {
				_ = os.Setenv("HERDR_PANE_ID", previousPane)
			} else {
				_ = os.Unsetenv("HERDR_PANE_ID")
			}
			if homePresent {
				_ = os.Setenv(workerHomeEnv, previousHome)
			} else {
				_ = os.Unsetenv(workerHomeEnv)
			}
		}()
		var output bytes.Buffer
		cli := f.service.CLI()
		cli.Out, cli.ErrOut = &output, &output
		args := append([]string{"signal"}, arguments...)
		return cli.Run(args), output.String()
	}
	if code, message := runSignal("done", "Reviewed", "--report", reportPath, "--verdict", "pass"); code == 0 || !strings.Contains(message, "review_findings_required") {
		t.Fatalf("Review Round without --findings accepted: %d %s", code, message)
	}
	writeFindings := func(severity, excerpt string) {
		t.Helper()
		payload := map[string]any{"schema": "posse.review-findings.v1", "patterns": []any{map[string]any{
			"key": "Unchecked--Write", "title": "Unchecked database write", "rule": "Check database errors",
			"root_cause": "The new helper ignores the returned error", "fix_guidance": "Check every write in this helper",
			"instances": []any{map[string]any{"repository": "shop", "path": "internal/app/x.go", "line": 4, "symbol": "save", "excerpt": excerpt, "description": "The write result is ignored"}},
		}}}
		if severity != "__missing__" {
			payload["patterns"].([]any)[0].(map[string]any)["severity"] = severity
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(findingsPath, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFindings("P1", "not present in the candidate")
	if code, message := runSignal("done", "Reviewed", "--report", reportPath, "--findings", findingsPath, "--verdict", "pass"); code == 0 || !strings.Contains(message, "review_finding_unanchored") {
		t.Fatalf("unanchored finding accepted: %d %s", code, message)
	}
	writeFindings("__missing__", "db.Exec(ctx, q)")
	if code, message := runSignal("done", "Reviewed", "--report", reportPath, "--findings", findingsPath, "--verdict", "pass"); code == 0 || !strings.Contains(message, "review_verdict_inconsistent") {
		t.Fatalf("pass with missing severity accepted: %d %s", code, message)
	}
	writeFindings("P1", "db.Exec(ctx, q)")
	if code, message := runSignal("done", "Reviewed", "--report", reportPath, "--findings", findingsPath, "--verdict", "pass"); code == 0 || !strings.Contains(message, "review_verdict_inconsistent") {
		t.Fatalf("pass with open P1 accepted: %d %s", code, message)
	}
	if code, message := runSignal("done", "Reviewed", "--report", reportPath, "--findings", findingsPath, "--verdict", "changes_requested"); code != 0 {
		t.Fatalf("changes_requested with open P1 failed: %d %s", code, message)
	}
	db, err = store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var key, state, severity string
	var inScope int
	if err := db.QueryRowContext(ctx, `SELECT p.key,f.state,f.severity,f.in_scope FROM review_findings f JOIN review_patterns p ON p.id=f.pattern_id WHERE f.ship_task_id=?`, change.ID).
		Scan(&key, &state, &severity, &inScope); err != nil {
		t.Fatal(err)
	}
	if key != "unchecked-write" || state != "open" || severity != "P1" || inScope != 1 {
		t.Fatalf("persisted finding = key %q state %q severity %q in_scope %d", key, state, severity, inScope)
	}
}

func TestReviewRoundRecordsUnrelatedHunkAsP1E2E(t *testing.T) {
	f := newStartFixture(t)
	configText := `[defaults]
merge_mode = "local"

[profiles.author]
kind = "claude"
model = "model-a"

[profiles.reviewer]
kind = "claude"
model = "model-b"

[dispatch.default]
use = "author"

[review_loop]
enabled = true
max_rounds = 1
auto_start = false
block_at = "P1"
`
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "origin.git")
	gitTest(t, f.repo, "init", "--bare", remote)
	gitTest(t, f.repo, "remote", "add", "origin", remote)
	gitTest(t, f.repo, "push", "-u", "origin", "main")
	if code, output := f.start(t); code != 0 {
		t.Fatalf("start code=%d %s", code, output)
	}
	change, _ := f.onlyTask(t)
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "committed.txt"), []byte("the Worker acts\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "cleanup.go"), []byte("package app\n\nfunc unrelatedRefactor() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, change.WorktreePath, "add", "committed.txt", "cleanup.go")
	gitTest(t, change.WorktreePath, "commit", "-m", "add committed file and an unrelated refactor")
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.Transition(ctx, change.ID, change.State, store.StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	var output bytes.Buffer
	cli := f.service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"review", "start", "t1", "--profile", "reviewer"}); code != 0 {
		t.Fatalf("review start code=%d %s", code, output.String())
	}
	db, err = store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rounds, err := db.ReviewRoundsFor(ctx, change.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("review rounds = %#v, %v", rounds, err)
	}
	review, err := db.TaskByID(ctx, f.project.ID, rounds[0].ReviewTaskID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := review.AdmittedPlan()
	if err != nil || plan == nil {
		t.Fatalf("review plan = %#v, %v", plan, err)
	}
	launch, err := os.ReadFile(filepath.Join(plan.Paths.Output, "launch.md"))
	if err != nil || !strings.Contains(string(launch), "Scope check: every hunk in") || !strings.Contains(string(launch), "with key `out-of-scope-hunk`, severity `P1` and origin `introduced`") || !strings.Contains(string(launch), "done_when: the Worker acts") {
		t.Fatalf("Review Round launch omits the scope check: %s err=%v", launch, err)
	}
	reportPath := filepath.Join(review.WorktreePath, "report.md")
	findingsPath := filepath.Join(review.WorktreePath, "findings.json")
	if err := os.WriteFile(reportPath, []byte("cleanup.go traces to no done_when item\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	findings := `{"schema":"posse.review-findings.v1","patterns":[{"key":"out-of-scope-hunk","title":"Unrelated refactor","rule":"Every hunk traces to a done_when item","root_cause":"The Change adds a refactor no done_when item needs","fix_guidance":"Revert the hunk and propose a follow-up issue","severity":"P1","origin":"introduced","instances":[{"repository":"shop","path":"cleanup.go","line":3,"symbol":"unrelatedRefactor","excerpt":"func unrelatedRefactor() {}","description":"The hunk serves no done_when item"}]}],"resolutions":[]}`
	if err := os.WriteFile(findingsPath, []byte(findings), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PANE_ID", review.PaneID)
	t.Setenv(workerHomeEnv, f.home)
	signal := func(verdict string) (int, string) {
		var output bytes.Buffer
		cli := f.service.CLI()
		cli.Out, cli.ErrOut = &output, &output
		return cli.Run([]string{"signal", "done", "Reviewed", "--report", reportPath, "--findings", findingsPath, "--verdict", verdict}), output.String()
	}
	if code, message := signal("pass"); code == 0 || !strings.Contains(message, "review_verdict_inconsistent") {
		t.Fatalf("pass with an out-of-scope hunk accepted: %d %s", code, message)
	}
	if code, message := signal("changes_requested"); code != 0 {
		t.Fatalf("changes_requested with an out-of-scope hunk failed: %d %s", code, message)
	}
	var key, state, severity, path string
	var inScope int
	if err := db.QueryRowContext(ctx, `SELECT p.key,f.state,f.severity,f.in_scope,f.path FROM review_findings f JOIN review_patterns p ON p.id=f.pattern_id WHERE f.ship_task_id=?`, change.ID).
		Scan(&key, &state, &severity, &inScope, &path); err != nil {
		t.Fatal(err)
	}
	if key != "out-of-scope-hunk" || state != "open" || severity != "P1" || inScope != 1 || path != "cleanup.go" {
		t.Fatalf("persisted finding = key %q state %q severity %q in_scope %d path %q", key, state, severity, inScope, path)
	}
}

func TestReviewLaunchFindingsExampleCarriesFilledResolution(t *testing.T) {
	brief := string(generatedReviewBrief(store.Task{Seq: 1}, taskplan.Plan{}, 1, nil, false))
	_, after, ok := strings.Cut(brief, "```json\n")
	if !ok {
		t.Fatal("review brief has no json example")
	}
	example, _, _ := strings.Cut(after, "\n```")
	parsed, err := decodeReviewFindings([]byte(example))
	if err != nil {
		t.Fatalf("example does not parse: %v", err)
	}
	if len(parsed.Resolutions) != 1 {
		t.Fatalf("example resolutions = %d, want 1", len(parsed.Resolutions))
	}
	resolution := parsed.Resolutions[0]
	if resolution.Finding == "" || resolution.Status == "" || resolution.Note == "" {
		t.Fatalf("example resolution leaves a field empty: %+v", resolution)
	}
	fields := reflect.TypeOf(resolution).NumField()
	var raw struct {
		Resolutions []map[string]any `json:"resolutions"`
	}
	if err := json.Unmarshal([]byte(example), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Resolutions[0]) != fields {
		t.Fatalf("example resolution has %d fields, parser accepts %d", len(raw.Resolutions[0]), fields)
	}
}
