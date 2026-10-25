//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Knowledge stays inert until the User opts in, then loads with provenance
// into Lead instructions and frozen Worker launches without touching the repo.
func TestKnowledgeOptInLoadsForLeadAndWorker(t *testing.T) {
	f := reviewIdentityFixture(t)
	leadInstructions := func() string {
		t.Helper()
		var result struct {
			Instructions string `json:"instructions"`
		}
		output := runPosse(t, f.binary, f.repo, f.leadEnv, "lead", "--json")
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			t.Fatalf("decode posse lead: %v\n%s", err, output)
		}
		return result.Instructions
	}
	writeBody := func(name, content string) string {
		t.Helper()
		path := filepath.Join(f.root, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	projectKnowledge := filepath.Join(f.home, "projects", "shop", "knowledge")
	if err := os.MkdirAll(projectKnowledge, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectKnowledge, "manual.md"), []byte("---\nsource: user\nverified: 2026-10-01\nload: always\n---\nManual knowledge marker.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if text := leadInstructions(); strings.Contains(text, "Manual knowledge marker") || strings.Contains(text, "## User and Project knowledge") || strings.Contains(text, "posse knowledge set") {
		t.Fatalf("disabled knowledge reached the Lead:\n%s", text)
	}
	fact := writeBody("fact.md", "Run the E2E shard self-check before publishing.\n")
	if code, output := runReviewIdentityCLI(t, f, "knowledge", "set", "shards", "--file", fact, "--source", "ref:scripts/e2e-shards.txt"); code != 1 || !strings.Contains(output, "knowledge_disabled") {
		t.Fatalf("disabled knowledge accepted a Lead write: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "config", "set", "knowledge.enabled", "true"); code != 1 || !strings.Contains(output, "user_only") {
		t.Fatalf("the Lead opted into knowledge without the User: %d %s", code, output)
	}
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "knowledge.enabled", "true")

	if code, output := runReviewIdentityCLI(t, f, "knowledge", "set", "shards", "--file", fact, "--source", "ref:scripts/e2e-shards.txt"); code != 0 {
		t.Fatalf("Lead knowledge write: %d %s", code, output)
	}
	steps := writeBody("steps.md", "Release steps marker.\n")
	if code, output := runReviewIdentityCLI(t, f, "knowledge", "set", "release", "--file", steps, "--source", "user", "--when", "when cutting a release"); code != 0 {
		t.Fatalf("on-demand knowledge write: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "knowledge", "set", "state", "--file", fact, "--source", "pr:7"); code != 1 || !strings.Contains(output, "knowledge_source_invalid") {
		t.Fatalf("live PR state was accepted as knowledge: %d %s", code, output)
	}
	text := leadInstructions()
	section := text[strings.Index(text, "## User and Project knowledge"):strings.Index(text, "## Runtime obligations")]
	for _, want := range []string{"Manual knowledge marker", "### shards (Project shop; source: ref:scripts/e2e-shards.txt; verified: ", "- `release` (Project shop, ", "when cutting a release", "cannot answer Decisions"} {
		if !strings.Contains(section, want) {
			t.Fatalf("Lead knowledge section omits %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "Release steps marker") {
		t.Fatalf("on-demand knowledge loaded at startup:\n%s", section)
	}
	show := runPosse(t, f.binary, f.repo, f.leadEnv, "knowledge", "show")
	for _, want := range []string{"enabled: true", "limit_bytes: 8192", "lead_bytes: ", "manual,Project shop,loaded,user,2026-10-01", "release,Project shop,on-demand,user"} {
		if !strings.Contains(show, want) {
			t.Fatalf("knowledge show omits %q:\n%s", want, show)
		}
	}

	runPosse(t, f.binary, f.repo, f.env, "config", "set", "knowledge.budget_bytes", "700", "--project", "shop")
	big := writeBody("big.md", strings.Repeat("x", 400)+"\n")
	if code, output := runReviewIdentityCLI(t, f, "knowledge", "set", "big", "--file", big, "--source", "user"); code != 1 || !strings.Contains(output, "knowledge_over_budget") {
		t.Fatalf("over-budget write was not refused: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "knowledge", "archive", "manual", "--reason", "covered by shards"); code != 0 {
		t.Fatalf("archive: %d %s", code, output)
	}
	runPosse(t, f.binary, f.repo, f.env, "config", "unset", "knowledge.budget_bytes", "--project", "shop")

	if code, output := startReviewIdentityTask(t, f, "Knowledge worker", "author", ""); code != 0 {
		t.Fatalf("start Worker: %d %s", code, output)
	}
	task := f.mustTask(t, "t1")
	launchPath := filepath.Join(f.home, "projects", "shop", "tasks", "t1", "launch.md")
	launch, err := os.ReadFile(launchPath)
	if err != nil || !strings.Contains(string(launch), "Run the E2E shard self-check before publishing.") || !strings.Contains(string(launch), "`posse knowledge show <id> --project shop`") || strings.Contains(string(launch), "Manual knowledge marker") {
		t.Fatalf("Worker launch did not admit the curated knowledge: %v\n%s", err, launch)
	}
	if err := os.WriteFile(fact, []byte("Edited after admission.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runReviewIdentityCLI(t, f, "knowledge", "set", "shards", "--file", fact, "--source", "user"); code != 0 {
		t.Fatalf("edit knowledge: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "relaunch", "t1"); code != 0 {
		t.Fatalf("relaunch: %d %s", code, output)
	}
	relaunched, err := os.ReadFile(launchPath)
	if err != nil || strings.Contains(string(relaunched), "Edited after admission.") || !strings.Contains(string(relaunched), "Run the E2E shard self-check before publishing.") {
		t.Fatalf("relaunch did not keep the admitted knowledge: %v\n%s", err, relaunched)
	}

	workerEnv := setEnv(f.env, "HERDR_PANE_ID", task.PaneID)
	read := exec.Command(f.binary, "knowledge", "show", "release", "--project", "shop")
	read.Dir, read.Env = task.WorktreePath, workerEnv
	if output, err := read.CombinedOutput(); err != nil || !strings.Contains(string(output), "Release steps marker.") {
		t.Fatalf("Worker could not load on-demand knowledge: %v %s", err, output)
	}
	write := exec.Command(f.binary, "knowledge", "set", "worker", "--file", fact, "--source", "user", "--project", "shop")
	write.Dir, write.Env = task.WorktreePath, workerEnv
	if output, err := write.CombinedOutput(); err == nil || !strings.Contains(string(output), "worker_forbidden") {
		t.Fatalf("Worker wrote knowledge: %v %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(projectKnowledge, "worker.md")); !os.IsNotExist(err) {
		t.Fatalf("refused Worker write left an entry: %v", err)
	}
	if status := gitTest(t, f.env, f.repo, "status", "--porcelain", "--ignored"); strings.TrimSpace(status) != "" {
		t.Fatalf("knowledge changed the Project repository: %s", status)
	}
}
