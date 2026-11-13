package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func TestFailedReviewLaunchDoesNotConsumeRoundCap(t *testing.T) {
	f := newStartFixture(t)
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(`[defaults]
merge_mode = "local"

[profiles.author]
kind = "claude"

[profiles.reviewer]
kind = "claude"

[dispatch.default]
use = "author"

[review_loop]
enabled = true
max_rounds = 1
auto_start = false
`), 0o600); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "origin.git")
	gitTest(t, f.repo, "init", "--bare", remote)
	gitTest(t, f.repo, "remote", "add", "origin", remote)
	gitTest(t, f.repo, "push", "-u", "origin", "main")
	if code, output := f.start(t); code != 0 {
		t.Fatalf("start Change Task: code=%d %s", code, output)
	}
	change, _ := f.onlyTask(t)
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "change.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, change.WorktreePath, "add", "change.txt")
	gitTest(t, change.WorktreePath, "commit", "-m", "candidate change")
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Transition(ctx, change.ID, change.State, store.StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	f.fake.Errors["agent.start"] = paneBusy()
	startReview := func() (int, string) {
		t.Helper()
		var output bytes.Buffer
		cli := f.service.CLI()
		cli.Out, cli.ErrOut = &output, &output
		code := cli.Run([]string{"review", "start", fmt.Sprintf("t%d", change.Seq), "--profile", "reviewer"})
		return code, output.String()
	}
	if code, output := startReview(); code != 1 || !strings.Contains(output, "agent_pane_busy") {
		t.Fatalf("busy Review launch: code=%d %s", code, output)
	}
	usage, err := db.ReviewCapUsage(ctx, change.ID)
	if err != nil || usage.ReviewRounds != 0 {
		t.Fatalf("round cap after void launch = %+v, %v", usage, err)
	}
	var status bytes.Buffer
	cli := f.service.CLI()
	cli.Out, cli.ErrOut = &status, &status
	if code := cli.Run([]string{"review", fmt.Sprintf("t%d", change.Seq)}); code != 0 || !strings.Contains(status.String(), "remaining: 1") || !strings.Contains(status.String(), "pending_review_rounds: 0") {
		t.Fatalf("failed launch consumed the visible round cap: code=%d %s", code, status.String())
	}
	f.fake.Errors = map[string]error{}
	f.fake.ErrorQueue["agent.start"] = []error{paneBusy(), paneBusy(), nil}
	if code, output := startReview(); code != 0 {
		t.Fatalf("start Review after the pane became available: code=%d %s", code, output)
	}
	rounds, err := db.ReviewRoundsFor(ctx, change.ID)
	if err != nil || len(rounds) != 2 || rounds[0].Status != "void" || rounds[1].Round != 1 || rounds[1].Status != "admitted" {
		t.Fatalf("rounds after retry = %#v, %v", rounds, err)
	}
}

func TestReviewRoundCarriesScopeInstructionsThroughThirdCompletion(t *testing.T) {
	f := newStartFixture(t)
	if code, output := f.start(t); code != 0 {
		t.Fatalf("start code=%d %s", code, output)
	}
	change, _ := f.onlyTask(t)
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, at := range []int64{100, 300, 500} {
		if _, err := db.ExecContext(context.Background(), `INSERT INTO signals(task_id,verb,at) VALUES(?,'done',?)`, change.ID, at); err != nil {
			t.Fatal(err)
		}
	}
	for _, message := range []struct {
		body        string
		createdAt   int64
		deliveredAt int64
	}{
		{body: "Expand scope before the first completion.", createdAt: 20, deliveredAt: 50},
		{body: "Expand scope to include the reviewed MR findings.", createdAt: 200, deliveredAt: 250},
		{body: "Clarify the later scope instruction.", createdAt: 400, deliveredAt: 450},
		{body: "Instruction delivered after completion.", createdAt: 550, deliveredAt: 600},
	} {
		if _, err := db.ExecContext(context.Background(), `INSERT INTO messages(task_id,body,created_at,delivered_at,status) VALUES(?,?,?,?, 'delivered')`, change.ID, message.body, message.createdAt, message.deliveredAt); err != nil {
			t.Fatal(err)
		}
	}
	instructions, err := reviewScopeInstructions(context.Background(), db, change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(instructions) != 3 || instructions[0] != "Expand scope before the first completion." || instructions[1] != "Expand scope to include the reviewed MR findings." || instructions[2] != "Clarify the later scope instruction." {
		t.Fatalf("review scope instructions = %#v", instructions)
	}
	brief := string(generatedReviewBrief(change, taskplan.Plan{}, 3, nil, instructions, false))
	if !strings.Contains(brief, "Lead instructions delivered before this Change completion") || !strings.Contains(brief, instructions[0]) || !strings.Contains(brief, instructions[1]) || !strings.Contains(brief, instructions[2]) || !strings.Contains(brief, "Judge scope against these instructions and the Change Brief") {
		t.Fatalf("Review Brief omits an accumulated scope instruction: %s", brief)
	}
}

func TestReviewRoundCarriesScopeInstructionBeforeFirstCompletion(t *testing.T) {
	f := newStartFixture(t)
	if code, output := f.start(t); code != 0 {
		t.Fatalf("start code=%d %s", code, output)
	}
	change, _ := f.onlyTask(t)
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO signals(task_id,verb,at) VALUES(?,'done',200)`, change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `INSERT INTO messages(task_id,body,created_at,delivered_at,status) VALUES(?, ?, 100, 150, 'delivered')`, change.ID, "Expand scope before the first completion."); err != nil {
		t.Fatal(err)
	}
	instructions, err := reviewScopeInstructions(context.Background(), db, change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(instructions) != 1 || instructions[0] != "Expand scope before the first completion." {
		t.Fatalf("first-completion scope instructions = %#v", instructions)
	}
}

func TestReviewStartAdmitsFrozenRoundAndGeneratedFiles(t *testing.T) {
	f := newStartFixture(t)
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(`[defaults]
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
guidance = ["docs/review.md"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.repo, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, "docs", "review.md"), []byte("Frozen main guidance.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.repo, "add", "docs/review.md")
	gitTest(t, f.repo, "commit", "-m", "add review guidance")
	remote := filepath.Join(t.TempDir(), "origin.git")
	gitTest(t, f.repo, "init", "--bare", remote)
	gitTest(t, f.repo, "remote", "add", "origin", remote)
	gitTest(t, f.repo, "push", "-u", "origin", "main")
	if code, output := f.start(t); code != 0 {
		t.Fatalf("start code=%d %s", code, output)
	}
	change, _ := f.onlyTask(t)
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "author-change.txt"), []byte("review me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, change.WorktreePath, "add", "author-change.txt")
	gitTest(t, change.WorktreePath, "commit", "-m", "author change")
	head := strings.TrimSpace(gitTest(t, change.WorktreePath, "rev-parse", "HEAD"))
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Transition(ctx, change.ID, change.State, store.StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
	for _, at := range []int64{100, 300} {
		if _, err := db.ExecContext(ctx, `INSERT INTO signals(task_id,verb,at) VALUES(?,'done',?)`, change.ID, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO messages(task_id,body,created_at,delivered_at,status) VALUES(?,?,200,250,'delivered')`, change.ID, "Expand scope to include the reviewed MR findings."); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	start := func() (int, string) {
		var output bytes.Buffer
		cli := f.service.CLI()
		cli.Out, cli.ErrOut = &output, &output
		code := cli.Run([]string{"review", "start", fmt.Sprintf("t%d", change.Seq), "--profile", "reviewer"})
		return code, output.String()
	}
	if code, output := start(); code != 0 {
		t.Fatalf("review start code=%d %s", code, output)
	}
	rounds, err := db.ReviewRoundsFor(ctx, change.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("admitted rounds = %#v, %v", rounds, err)
	}
	round := rounds[0]
	target := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "refs/remotes/origin/main"))
	if round.Round != 1 || round.Status != "admitted" || round.HeadSHA != head || round.TargetCommitSHA != target || round.ScopeBaseSHA != target || round.Repository != "shop" {
		t.Fatalf("round binding = %#v, target=%s head=%s", round, target, head)
	}
	review, err := db.TaskByID(ctx, change.ProjectID, round.ReviewTaskID)
	if err != nil {
		t.Fatal(err)
	}
	// The fake Herdr adapter does not materialize panes created by tab.create.
	// Keep the admitted Review Worker visible so the next start exercises the
	// active-round guard rather than marking the absent pane lost first.
	f.fake.SnapshotValue.Panes = append(f.fake.SnapshotValue.Panes, herdr.Pane{
		PaneID: review.PaneID, WorkspaceID: review.HerdrWorkspaceID, Label: review.PaneLabel,
		Agent: "claude", AgentStatus: "working",
	})
	f.fake.Results["pane.read"] = json.RawMessage(`{"read":{"text":"review worker is active"}}`)
	plan, err := review.AdmittedPlan()
	if err != nil || plan == nil || len(plan.Inputs) == 0 || !strings.Contains(string(plan.Inputs[0].Bytes), "Create a committed file.") {
		t.Fatalf("generated Review Brief lost Change intent: plan=%#v err=%v", plan, err)
	}
	read := func(name string) []byte {
		t.Helper()
		contents, err := os.ReadFile(filepath.Join(plan.Paths.Output, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("read generated %s: %v", name, err)
		}
		return contents
	}
	var metadata struct {
		Round            int      `json:"round"`
		MaxRounds        int      `json:"max_rounds"`
		Remaining        int      `json:"remaining_rounds"`
		LeadInstructions []string `json:"lead_instructions"`
		Repositories     []struct {
			HeadSHA         string `json:"head_sha"`
			TargetCommitSHA string `json:"target_commit_sha"`
			DeltaSHA256     string `json:"delta_sha256"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(read("review/round.json"), &metadata); err != nil {
		t.Fatal(err)
	}
	delta := read("review/delta.patch")
	if metadata.Round != 1 || metadata.MaxRounds != 1 || metadata.Remaining != 0 || len(metadata.Repositories) != 1 || metadata.Repositories[0].HeadSHA != head || metadata.Repositories[0].TargetCommitSHA != target || len(metadata.LeadInstructions) != 1 || metadata.LeadInstructions[0] != "Expand scope to include the reviewed MR findings." || !strings.Contains(string(delta), "author-change.txt") {
		t.Fatalf("generated scope metadata = %#v; delta=%s", metadata, delta)
	}
	digest := sha256.Sum256(delta)
	if metadata.Repositories[0].DeltaSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("delta digest = %s, want %x", metadata.Repositories[0].DeltaSHA256, digest)
	}
	if got := string(read("review/guidance/docs/review.md")); got != "Frozen main guidance.\n" {
		t.Fatalf("guidance was not frozen from origin/main: %q", got)
	}
	if got := string(read("review/ledger.md")); !strings.Contains(got, "No findings have been recorded yet.") {
		t.Fatalf("generated Finding Ledger = %s", got)
	}
	launchPath := filepath.Join(plan.Paths.Output, "launch.md")
	launch, err := os.ReadFile(launchPath)
	if err != nil || !strings.Contains(string(launch), "## Review Round 1") || !strings.Contains(string(launch), "review/delta.patch") || !strings.Contains(string(launch), "Expand scope to include the reviewed MR findings.") || !strings.Contains(string(launch), "Judge scope against these instructions and the Change Brief") {
		t.Fatalf("launch instructions omit Review Round files: %s err=%v", launch, err)
	}
	for _, name := range []string{"delta.patch", "cumulative.patch", "round.json", "conflicts.txt", "ledger.md", "guidance"} {
		want := filepath.Join(plan.Paths.Output, "review", name)
		if !strings.Contains(string(launch), want) {
			t.Fatalf("launch does not name %s from the Reviewer's working directory %s:\n%s", want, review.WorktreePath, launch)
		}
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("named review path is missing: %v", err)
		}
	}
	if err := os.Remove(filepath.Join(plan.Paths.Output, "review", "delta.patch")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(launchPath); err != nil {
		t.Fatal(err)
	}
	if err := restoreAdmittedLaunchFiles(f.home, f.project, review, plan); err != nil {
		t.Fatalf("restore generated review inputs: %v", err)
	}
	if restored := read("review/delta.patch"); !bytes.Equal(restored, delta) {
		t.Fatalf("restored delta patch differs from the admitted input: %s", restored)
	}
	restoredLaunch, err := os.ReadFile(launchPath)
	if err != nil || !strings.Contains(string(restoredLaunch), filepath.Join(plan.Paths.Output, "review", "delta.patch")) {
		t.Fatalf("restored launch omits Review Round instructions: %s err=%v", restoredLaunch, err)
	}
	beforeTabs := f.fake.CallCount("tab.create")
	if code, output := start(); code != 1 || !strings.Contains(output, "review_round_active") {
		t.Fatalf("active round refusal code=%d: %s", code, output)
	}
	if f.fake.CallCount("tab.create") != beforeTabs {
		t.Fatal("active-round refusal created another pane")
	}
	if _, err := db.ExecContext(ctx, `UPDATE review_rounds SET status='verdict',verdict='pass' WHERE ship_task_id=?`, change.ID); err != nil {
		t.Fatal(err)
	}
	if code, output := start(); code != 1 || !strings.Contains(output, "review_round_unchanged") {
		t.Fatalf("unchanged head refusal code=%d: %s", code, output)
	}
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "later-change.txt"), []byte("new review scope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, change.WorktreePath, "add", "later-change.txt")
	gitTest(t, change.WorktreePath, "commit", "-m", "later change")
	if code, output := start(); code != 1 || !strings.Contains(output, "review_round_cap") {
		t.Fatalf("round cap refusal code=%d: %s", code, output)
	}
	plan.Paths.Output = filepath.Join(f.home, "projects", f.project.Name, "tasks", "t99")
	plan.Paths.Scratch = filepath.Join(f.home, "scratch", f.project.Name, "t99")
	planJSON, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET seq=99,branch='posse/t99',plan_json=?,state='failed',mount_id=NULL WHERE id=?`, planJSON, review.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=?`, planJSON, review.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE mounts SET state='idle',task_id=NULL WHERE task_id=?`, review.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM transitions WHERE task_id=? AND to_state='working'`, review.ID); err != nil {
		t.Fatal(err)
	}
	currentProject, err := db.ProjectByID(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.fake.SnapshotValue.Panes = []herdr.Pane{{PaneID: currentProject.LeadPaneID, WorkspaceID: currentProject.HerdrWorkspaceID, Label: currentProject.LeadLabel, Agent: "claude", AgentStatus: "idle"}}
	var relaunchOutput bytes.Buffer
	cli := f.service.CLI()
	cli.Out, cli.ErrOut = &relaunchOutput, &relaunchOutput
	if code := cli.Run([]string{"relaunch", "t99"}); code != 0 {
		t.Fatalf("relaunch never-started Review Task: code=%d output=%s", code, relaunchOutput.String())
	}
	for _, name := range []string{"brief.md", "launch.md"} {
		contents, err := os.ReadFile(filepath.Join(plan.Paths.Output, name))
		if err != nil || strings.Contains(string(contents), "{{review_dir}}") || !strings.Contains(string(contents), filepath.Join(plan.Paths.Output, "review")) {
			t.Fatalf("relaunched %s has unresolved Review path: %s err=%v", name, contents, err)
		}
	}
	if _, err := os.Stat(filepath.Join(plan.Paths.Output, "review", "round.json")); err != nil {
		t.Fatalf("relaunched Review Round files are missing: %v", err)
	}
}

func TestReviewLedgerShowsFindingSeverity(t *testing.T) {
	f := newStartFixture(t)
	if code, output := f.start(t); code != 0 {
		t.Fatalf("start code=%d %s", code, output)
	}
	change, _ := f.onlyTask(t)
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO review_patterns(id,ship_task_id,seq,key,title,rule,root_cause,fix_guidance,created_round) VALUES(1,?,1,'dropped-error','t','r','c','g',1)`, change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO review_findings(ship_task_id,seq,pattern_id,fingerprint,round,repository,path,symbol,excerpt,line_at_report,head_sha,severity,origin,in_scope,state) VALUES(?,1,1,?,1,'shop','x.go','f','e',1,?,'P1','introduced',1,'open')`, change.ID, strings.Repeat("a", 64), strings.Repeat("b", 40)); err != nil {
		t.Fatal(err)
	}
	ledger, err := reviewLedgerMarkdown(ctx, db, change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ledger, "| F-1 | P-1 `dropped-error` | open | P1 |") {
		t.Fatalf("ledger omits the finding severity:\n%s", ledger)
	}
}

func TestWriteReviewRoundFilesCreatesEmptyGuidanceDirectory(t *testing.T) {
	home := t.TempDir()
	if err := writeReviewRoundFiles(home, map[string][]byte{"review/ledger.md": []byte("x")}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(home, "review", "guidance"))
	if err != nil || !info.IsDir() {
		t.Fatalf("review/guidance is missing for a target without guidance files: %v", err)
	}
}

func TestAutoStartReviewLoopRespectsSelectedReviewPolicy(t *testing.T) {
	if autoStartReviewLoop(&taskplan.Plan{Review: "none", ValidationLoops: &taskplan.ValidationLoops{Review: config.ReviewLoop{Enabled: true, AutoStart: true}}}) {
		t.Fatal("auto-start enabled for a Task whose admitted review policy is none")
	}
	if !autoStartReviewLoop(&taskplan.Plan{Review: "quick", ValidationLoops: &taskplan.ValidationLoops{Review: config.ReviewLoop{Enabled: true, AutoStart: true}}}) {
		t.Fatal("auto-start disabled for a reviewed Task")
	}
}

func TestAutoStartReviewLoopStartsWhenMergeIsRequested(t *testing.T) {
	f := newStartFixture(t)
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(`[defaults]
merge_mode = "local"
review = "quick"

[profiles.author]
kind = "claude"
model = "model-a"

[profiles.reviewer]
kind = "claude"
model = "model-b"

[[dispatch]]
type = "change"
use = "author"

[[dispatch]]
type = "review"
use = "reviewer"

[dispatch.default]
use = "author"

[review_loop]
enabled = true
max_rounds = 1
auto_start = true
`), 0o600); err != nil {
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
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "change.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, change.WorktreePath, "add", "change.txt")
	gitTest(t, change.WorktreePath, "commit", "-m", "candidate change")
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Transition(ctx, change.ID, change.State, store.StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	merge := func() (int, string) {
		t.Helper()
		var output bytes.Buffer
		cli := f.service.CLI()
		cli.Out, cli.ErrOut = &output, &output
		code := cli.Run([]string{"merge", fmt.Sprintf("t%d", change.Seq)})
		return code, output.String()
	}
	mergeCode, mergeOutput := merge()
	if mergeCode != 0 {
		t.Fatalf("auto-start merge exit=%d output=%s", mergeCode, mergeOutput)
	}
	rounds, err := db.ReviewRoundsFor(ctx, change.ID)
	if err != nil || len(rounds) != 1 || rounds[0].Round != 1 || rounds[0].Status != "admitted" {
		current, _ := db.TaskByID(ctx, change.ProjectID, change.ID)
		frozen, _ := current.AdmittedPlan()
		t.Fatalf("auto-start rounds = %#v, err=%v, output=%s, state=%s loops=%#v", rounds, err, mergeOutput, current.State, frozen.ValidationLoops)
	}
	review, err := db.TaskByID(ctx, change.ProjectID, rounds[0].ReviewTaskID)
	if err != nil || review.Profile != "reviewer" {
		t.Fatalf("auto-start Review Task = %#v, %v", review, err)
	}
	current, err := db.TaskByID(ctx, change.ProjectID, change.ID)
	if err != nil || current.State != store.StateDone {
		t.Fatalf("auto-start moved Change Task before review: %#v, %v", current, err)
	}
}

func TestAutoStartReviewRoundIgnoresMaxWorkersAtCapacity(t *testing.T) {
	f := newStartFixture(t)
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(`[defaults]
max_workers = 1
merge_mode = "local"
review = "quick"

[profiles.author]
kind = "claude"
model = "model-a"

[profiles.reviewer]
kind = "claude"
model = "model-b"

[[dispatch]]
type = "change"
use = "author"

[[dispatch]]
type = "review"
use = "reviewer"

[dispatch.default]
use = "author"

[review_loop]
enabled = true
max_rounds = 1
auto_start = true
`), 0o600); err != nil {
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
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "change.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, change.WorktreePath, "add", "change.txt")
	gitTest(t, change.WorktreePath, "commit", "-m", "candidate change")
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Transition(ctx, change.ID, change.State, store.StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
	queued := store.Task{Type: "change", Title: "queued", ShortName: "queued", State: store.StateQueued, MergeMode: "local"}
	if _, _, err := db.CreateTaskWithSequence(ctx, change.ProjectID, f.project.Name, queued, func(int) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	merge := func() (int, string) {
		t.Helper()
		var output bytes.Buffer
		cli := f.service.CLI()
		cli.Out, cli.ErrOut = &output, &output
		code := cli.Run([]string{"merge", fmt.Sprintf("t%d", change.Seq)})
		return code, output.String()
	}
	mergeCode, mergeOutput := merge()
	if mergeCode != 0 {
		t.Fatalf("auto-start merge exit=%d output=%s", mergeCode, mergeOutput)
	}
	rounds, err := db.ReviewRoundsFor(ctx, change.ID)
	if err != nil || len(rounds) != 1 || rounds[0].Round != 1 || rounds[0].Status != "admitted" {
		current, _ := db.TaskByID(ctx, change.ProjectID, change.ID)
		frozen, _ := current.AdmittedPlan()
		t.Fatalf("auto-start rounds = %#v, err=%v, output=%s, state=%s loops=%#v", rounds, err, mergeOutput, current.State, frozen.ValidationLoops)
	}
	review, err := db.TaskByID(ctx, change.ProjectID, rounds[0].ReviewTaskID)
	if err != nil || review.Profile != "reviewer" {
		t.Fatalf("auto-start Review Task = %#v, %v", review, err)
	}
	current, err := db.TaskByID(ctx, change.ProjectID, change.ID)
	if err != nil || current.State != store.StateDone {
		t.Fatalf("auto-start moved Change Task before review: %#v, %v", current, err)
	}
}

func TestAutoStartSkipsUnavailableReviewerWithoutBlockingMerge(t *testing.T) {
	for _, test := range []struct {
		name         string
		config       string
		explicitCode string
	}{
		{
			name: "reviewer resumes a session",
			config: `[defaults]
merge_mode = "local"
review = "quick"

[profiles.author]
kind = "claude"

[profiles.reviewer]
kind = "claude"
args = ["--resume", "author-session"]

[[dispatch]]
type = "change"
use = "author"

[[dispatch]]
type = "review"
use = "reviewer"

[review_loop]
enabled = true
auto_start = true
`,
			explicitCode: "review_identity_ineligible",
		},
		{
			name: "no review dispatch",
			config: `[defaults]
merge_mode = "local"
review = "quick"

[profiles.author]
kind = "claude"

[[dispatch]]
type = "change"
use = "author"

[review_loop]
enabled = true
auto_start = true
`,
			explicitCode: "profile_required",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertAutoStartSkipsUnavailableReviewer(t, test.config, test.explicitCode, false)
		})
	}
}

func TestAutoStartDoesNotReportSkippedReviewForPassedHead(t *testing.T) {
	assertAutoStartSkipsUnavailableReviewer(t, `[defaults]
merge_mode = "local"
review = "quick"

[profiles.author]
kind = "claude"

[[dispatch]]
type = "change"
use = "author"

[review_loop]
enabled = true
auto_start = true
`, "profile_required", true)
}

func assertAutoStartSkipsUnavailableReviewer(t *testing.T, configText, explicitCode string, passedHead bool) {
	t.Helper()
	f, db, change := newAutoStartReviewFixture(t, configText)
	ctx := context.Background()
	if passedHead {
		if _, err := db.ExecContext(ctx, `INSERT INTO review_rounds(ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,verdict,created_at)
VALUES(?,1,?,?,'delta',?,?,?,?,?,'[]','verdict','pass',0)`, change.ID, change.ID, f.project.Name, change.GatedSHA, change.GatedSHA, change.GatedSHA, change.GatedSHA, strings.Repeat("0", 64)); err != nil {
			t.Fatal(err)
		}
		autoStartReviewExpectSkip(t, f, db, change)
		if notices := reviewAutoStartSkippedNotices(t, db, change); len(notices) != 0 {
			t.Fatalf("passed head raised %#v", notices)
		}
		return
	}
	autoStartReviewExpectSkip(t, f, db, change)
	rounds, err := db.ReviewRoundsFor(ctx, change.ID)
	if err != nil || len(rounds) != 0 {
		t.Fatalf("unavailable auto-start admitted Review Rounds: %#v, %v", rounds, err)
	}
	assertReviewAutoStartReadinessNotices(t, db, change, 1)
	autoStartReviewExpectSkip(t, f, db, change)
	assertReviewAutoStartReadinessNotices(t, db, change, 1)
	var output bytes.Buffer
	cli := f.service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"review", "start", fmt.Sprintf("t%d", change.Seq)}); code == 0 || !strings.Contains(output.String(), explicitCode) {
		t.Fatalf("explicit review start should fail with %s: code=%d output=%s", explicitCode, code, output.String())
	}
}

func TestReviewStartWithoutProfileChoosesReviewerIndependentOfAuthor(t *testing.T) {
	configText := `[defaults]
merge_mode = "local"
review = "quick"

[profiles.author]
kind = "claude"
model = "author-model"

[profiles.sonnet]
kind = "claude"
model = "review-model-a"

[profiles.opus]
kind = "claude"
model = "review-model-b"

[[dispatch]]
type = "change"
use = "author"

[[dispatch]]
type = "review"
use = "sonnet"

[[dispatch]]
type = "review"
use = "opus"

[review_loop]
enabled = true
auto_start = true
`
	f, db, change := newAutoStartReviewFixture(t, configText)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	var output bytes.Buffer
	cli := f.service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"review", "start", fmt.Sprintf("t%d", change.Seq)}); code == 0 || !strings.Contains(output.String(), "profile_required") || !strings.Contains(output.String(), "sonnet, opus") {
		t.Fatalf("review start among two unconditional review Profiles exit=%d output=%s", code, output.String())
	}
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(strings.Replace(configText, "[[dispatch]]\ntype = \"review\"\nuse = \"opus\"\n\n", "", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if code := cli.Run([]string{"review", "start", fmt.Sprintf("t%d", change.Seq)}); code != 0 {
		t.Fatalf("review start without --profile exit=%d output=%s", code, output.String())
	}
	rounds, err := db.ReviewRoundsFor(context.Background(), change.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("review rounds = %#v, %v", rounds, err)
	}
	review, err := db.TaskByID(context.Background(), change.ProjectID, rounds[0].ReviewTaskID)
	if err != nil || review.Profile != "sonnet" || review.Profile == "author" {
		t.Fatalf("Review Task profile=%q, err=%v", review.Profile, err)
	}
}

func TestAutoStartUsesTheReviewRuleProfileEvenWithAuthorModel(t *testing.T) {
	f, db, change := newAutoStartReviewFixture(t, `[defaults]
merge_mode = "local"
review = "quick"

[profiles.sonnet]
kind = "claude"
model = "model-a"

[profiles.opus]
kind = "claude"
model = "model-b"

[[dispatch]]
type = "change"
use = "sonnet"

[[dispatch]]
type = "review"
use = "sonnet"

[review_loop]
enabled = true
auto_start = true
`)
	ctx := context.Background()
	started, err := f.service.maybeAutoStartReviewRound(&axi.Context{Context: ctx, Out: &bytes.Buffer{}}, db, f.home, f.project, change)
	if err != nil || !started {
		t.Fatalf("auto-start result = started %v, err %v; notices=%#v", started, err, reviewAutoStartSkippedNotices(t, db, change))
	}
	rounds, err := db.ReviewRoundsFor(ctx, change.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("auto-start rounds = %#v, %v", rounds, err)
	}
	review, err := db.TaskByID(ctx, change.ProjectID, rounds[0].ReviewTaskID)
	if err != nil || review.Profile != "sonnet" {
		t.Fatalf("auto-start Review Task = %#v, %v", review, err)
	}
}

func TestAutoStartUsesReviewLoopProfile(t *testing.T) {
	f, db, change := newAutoStartReviewFixture(t, `[defaults]
merge_mode = "local"
review = "quick"

[profiles.sonnet]
kind = "claude"
model = "model-a"

[profiles.opus]
kind = "claude"
model = "model-b"

[[dispatch]]
type = "change"
use = "sonnet"

[[dispatch]]
type = "review"
use = "sonnet"

[[dispatch]]
type = "review"
use = "opus"

[review_loop]
enabled = true
auto_start = true
profile = "opus"
`)
	ctx := context.Background()
	started, err := f.service.maybeAutoStartReviewRound(&axi.Context{Context: ctx, Out: &bytes.Buffer{}}, db, f.home, f.project, change)
	if err != nil || !started {
		t.Fatalf("auto-start result = started %v, err %v; notices=%#v", started, err, reviewAutoStartSkippedNotices(t, db, change))
	}
	rounds, err := db.ReviewRoundsFor(ctx, change.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("auto-start rounds = %#v, %v", rounds, err)
	}
	review, err := db.TaskByID(ctx, change.ProjectID, rounds[0].ReviewTaskID)
	if err != nil || review.Profile != "opus" || review.DispatchRule != "review_loop.profile" {
		t.Fatalf("auto-start Review Task = %#v, %v", review, err)
	}
}

func TestAutoStartNeverGuessesAmongConditionalRules(t *testing.T) {
	const base = `[defaults]
merge_mode = "local"
review = "quick"

[profiles.author]
kind = "claude"
model = "model-author"

[profiles.img]
kind = "claude"
model = "model-img"

[profiles.reviewer]
kind = "claude"
model = "model-reviewer"

[profiles.resumer]
kind = "claude"
model = "model-reviewer"
args = ["--continue"]

[review_loop]
enabled = true
auto_start = true
`
	for _, test := range []struct {
		name    string
		global  string
		project string
		want    string
		notice  []string
	}{
		{
			name:    "global conditional image rule first and a default",
			global:  "[[dispatch]]\nwhen = \"Task requires generating images\"\nuse = \"img\"\n\n[[dispatch]]\nwhen = \"Heavy work\"\nuse = \"reviewer\"\n\n[dispatch.default]\nuse = \"reviewer\"\n",
			project: "[[dispatch]]\ntype = \"change\"\nuse = \"author\"\n",
			notice:  []string{"review_loop.profile", "img"},
		},
		{
			name:    "only conditional review rules",
			global:  "[[dispatch]]\ntype = \"review\"\nwhen = \"security changes\"\nuse = \"img\"\n",
			project: "[[dispatch]]\ntype = \"change\"\nuse = \"author\"\n",
			notice:  []string{"review_loop.profile", "img"},
		},
		{
			name:    "unconditional review rules in global and Project config name different Profiles",
			global:  "[[dispatch]]\ntype = \"review\"\nuse = \"reviewer\"\n",
			project: "[[dispatch]]\ntype = \"change\"\nuse = \"author\"\n\n[[dispatch]]\ntype = \"review\"\nuse = \"img\"\n",
			notice:  []string{"review_loop.profile", "img, reviewer"},
		},
		{
			name:    "an unconditional review rule with a use array",
			project: "[[dispatch]]\ntype = \"change\"\nuse = \"author\"\n\n[[dispatch]]\ntype = \"review\"\nuse = [\"reviewer\", \"img\"]\n",
			notice:  []string{"review_loop.profile", "reviewer, img"},
		},
		{
			name:    "unconditional review rules in global and Project config name the same Profile",
			global:  "[[dispatch]]\ntype = \"review\"\nuse = \"reviewer\"\n",
			project: "[[dispatch]]\ntype = \"change\"\nuse = \"author\"\n\n[[dispatch]]\ntype = \"review\"\nuse = \"reviewer\"\n",
			want:    "reviewer",
		},
		{
			name:    "one unconditional review Profile starts a fresh session",
			project: "[[dispatch]]\ntype = \"change\"\nuse = \"author\"\n\n[[dispatch]]\ntype = \"review\"\nuse = \"resumer\"\n\n[[dispatch]]\ntype = \"review\"\nuse = \"reviewer\"\n",
			want:    "reviewer",
		},
		{
			name:    "an unconditional review rule beside a conditional image rule",
			global:  "[[dispatch]]\nwhen = \"Task requires generating images\"\nuse = \"img\"\n",
			project: "[[dispatch]]\ntype = \"change\"\nuse = \"author\"\n\n[[dispatch]]\ntype = \"review\"\nuse = \"reviewer\"\n",
			want:    "reviewer",
		},
		{
			name:    "an unconditional review rule after a conditional review rule",
			global:  "[[dispatch]]\ntype = \"review\"\nwhen = \"security changes\"\nuse = \"img\"\n\n[[dispatch]]\ntype = \"review\"\nuse = \"reviewer\"\n",
			project: "[[dispatch]]\ntype = \"change\"\nuse = \"author\"\n",
			want:    "reviewer",
		},
		{
			name:    "dispatch.default with no conditional rule",
			global:  "[dispatch.default]\nuse = \"reviewer\"\n",
			project: "[[dispatch]]\ntype = \"change\"\nuse = \"author\"\n",
			want:    "reviewer",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, db, change := newAutoStartReviewFixture(t, base+test.project)
			if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(base+test.global), 0o600); err != nil {
				t.Fatal(err)
			}
			projectConfig := config.ConfigPath(f.home, f.project.Name)
			if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(projectConfig, []byte(test.project), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			started, err := f.service.maybeAutoStartReviewRound(&axi.Context{Context: ctx, Out: &bytes.Buffer{}}, db, f.home, f.project, change)
			if err != nil {
				t.Fatal(err)
			}
			rounds, err := db.ReviewRoundsFor(ctx, change.ID)
			if err != nil {
				t.Fatal(err)
			}
			if test.want == "" {
				if started || len(rounds) != 0 {
					review, _ := db.TaskByID(ctx, change.ProjectID, rounds[0].ReviewTaskID)
					t.Fatalf("auto-start guessed Profile %q (%s)", review.Profile, review.DispatchRule)
				}
				notices := assertReviewAutoStartReadinessNotices(t, db, change, 1)
				for _, want := range test.notice {
					if !strings.Contains(notices[0].Summary, want) {
						t.Fatalf("Notice lacks %q: %s", want, notices[0].Summary)
					}
				}
				return
			}
			if !started || len(rounds) != 1 {
				t.Fatalf("auto-start started=%v rounds=%d notices=%#v", started, len(rounds), reviewAutoStartSkippedNotices(t, db, change))
			}
			review, err := db.TaskByID(ctx, change.ProjectID, rounds[0].ReviewTaskID)
			if err != nil || review.Profile != test.want {
				t.Fatalf("auto-start Review Task profile=%q rule=%q, want %q (%v)", review.Profile, review.DispatchRule, test.want, err)
			}
		})
	}
}

func TestAutoStartNamesIneligibleReviewRuleCandidatesPerHead(t *testing.T) {
	f, db, change := newAutoStartReviewFixture(t, `[defaults]
merge_mode = "local"
review = "quick"

[profiles.author]
kind = "claude"
model = "model-a"

[profiles.sonnet]
kind = "claude"
model = "model-a"
args = ["--continue"]

[profiles.sonnet-twin]
kind = "claude"
model = "model-a"
args = ["--resume", "author-session"]

[[dispatch]]
type = "change"
use = "author"

[[dispatch]]
type = "review"
use = "sonnet"

[[dispatch]]
type = "review"
use = "sonnet-twin"

[review_loop]
enabled = true
auto_start = true
`)
	autoStartReviewExpectSkip(t, f, db, change)
	notices := assertReviewAutoStartReadinessNotices(t, db, change, 1)
	for _, want := range []string{"sonnet (launch arguments contain 1 session selector(s))", "sonnet-twin (launch arguments contain 1 session selector(s))", fmt.Sprintf("posse review start t%d --profile <profile>", change.Seq)} {
		if !strings.Contains(notices[0].Summary, want) {
			t.Fatalf("auto-start readiness notice lacks %q: %s", want, notices[0].Summary)
		}
	}
	if strings.Contains(notices[0].Summary, "no eligible reviewer Profile is configured") {
		t.Fatalf("auto-start readiness notice hides the real reason: %s", notices[0].Summary)
	}
	autoStartReviewExpectSkip(t, f, db, change)
	assertReviewAutoStartReadinessNotices(t, db, change, 1)
	change = commitAutoStartCandidate(t, db, change, "second.txt")
	autoStartReviewExpectSkip(t, f, db, change)
	assertReviewAutoStartReadinessNotices(t, db, change, 2)
	var output bytes.Buffer
	cli := f.service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"merge", fmt.Sprintf("t%d", change.Seq)}); code == 0 || !strings.Contains(output.String(), "review_required") || !strings.Contains(output.String(), fmt.Sprintf("posse review start t%d --profile <profile>", change.Seq)) {
		t.Fatalf("merge without an eligible reviewer: code=%d output=%s", code, output.String())
	}
}

func newAutoStartReviewFixture(t *testing.T, configText string) (startFixture, *store.DB, store.Task) {
	t.Helper()
	f := newStartFixture(t)
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
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := db.Transition(ctx, change.ID, change.State, store.StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
	change.State = store.StateDone
	return f, db, commitAutoStartCandidate(t, db, change, "change.txt")
}

func commitAutoStartCandidate(t *testing.T, db *store.DB, change store.Task, name string) store.Task {
	t.Helper()
	if err := os.WriteFile(filepath.Join(change.WorktreePath, name), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, change.WorktreePath, "add", name)
	gitTest(t, change.WorktreePath, "commit", "-m", "candidate "+name)
	change.GatedSHA = strings.TrimSpace(gitTest(t, change.WorktreePath, "rev-parse", "HEAD"))
	if _, err := db.ExecContext(context.Background(), `UPDATE tasks SET gated_sha=? WHERE id=?`, change.GatedSHA, change.ID); err != nil {
		t.Fatal(err)
	}
	return change
}

func autoStartReviewExpectSkip(t *testing.T, f startFixture, db *store.DB, change store.Task) {
	t.Helper()
	started, err := f.service.maybeAutoStartReviewRound(&axi.Context{Context: context.Background(), Out: &bytes.Buffer{}}, db, f.home, f.project, change)
	if err != nil || started {
		t.Fatalf("auto-start result = started %v, err %v", started, err)
	}
}

func reviewAutoStartSkippedNotices(t *testing.T, db *store.DB, change store.Task) []store.Notice {
	t.Helper()
	notices, err := db.Notices(context.Background(), change.ProjectID, false)
	if err != nil {
		t.Fatal(err)
	}
	var skipped []store.Notice
	for _, notice := range notices {
		if notice.Kind == "review_auto_start_skipped" {
			skipped = append(skipped, notice)
		}
	}
	return skipped
}

func assertReviewAutoStartReadinessNotices(t *testing.T, db *store.DB, change store.Task, want int) []store.Notice {
	t.Helper()
	skipped := reviewAutoStartSkippedNotices(t, db, change)
	for _, notice := range skipped {
		if !strings.Contains(notice.Summary, "eligible reviewer") || !strings.Contains(notice.Summary, "posse review start") {
			t.Fatalf("auto-start readiness notice = %#v", notice)
		}
	}
	if len(skipped) != want {
		t.Fatalf("auto-start readiness notices = %d, want %d; notices=%#v", len(skipped), want, skipped)
	}
	return skipped
}

func TestAutoStartReviewerUnavailableOnlySkipsProfileResolutionFailures(t *testing.T) {
	for _, test := range []struct {
		code string
		want bool
	}{
		{code: "review_identity_ineligible", want: true},
		{code: "profile_required", want: true},
		{code: "profile_unknown", want: true},
		{code: "kind_disabled", want: true},
		{code: "review_target_unresolved", want: false},
		{code: "gate_failed", want: false},
	} {
		t.Run(test.code, func(t *testing.T) {
			failure, got := autoStartReviewerUnavailable(fmt.Errorf("wrapped: %w", axi.Failure(test.code, "details", false)))
			if got != test.want {
				t.Fatalf("autoStartReviewerUnavailable(%q) = %v, want %v", test.code, got, test.want)
			}
			if got && failure.Code != test.code {
				t.Fatalf("matched failure code = %q, want %q", failure.Code, test.code)
			}
		})
	}
}

func TestReviewRoundScopeUsesFrozenTargetVirtualBaseline(t *testing.T) {
	for _, test := range []struct {
		name            string
		conflict        bool
		rewrite         bool
		targetNotMerged bool
		wantKind        string
		wantIn          []string
		wantOut         []string
		wantConflict    string
	}{
		{
			name:     "clean target merge then one author commit",
			wantKind: "delta",
			wantIn:   []string{"author-after.txt"},
			wantOut:  []string{"target-only.txt", "author-before.txt"},
		},
		{
			name:         "resolved target conflict",
			conflict:     true,
			wantKind:     "delta",
			wantOut:      []string{"conflict.txt", "<<<<<<<", ">>>>>>>", "target version", "resolved"},
			wantConflict: "conflict.txt",
		},
		{
			name:            "target advances without entering the candidate",
			targetNotMerged: true,
			wantKind:        "delta",
			wantIn:          []string{"author-after.txt"},
			wantOut:         []string{"target-only.txt", "author-before.txt"},
		},
		{
			name:     "rewritten Change branch",
			rewrite:  true,
			wantKind: "full_fallback",
			wantIn:   []string{"rewritten.txt"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			remote := filepath.Join(root, "remote.git")
			initRepo(t, repo)
			gitTest(t, root, "init", "--bare", remote)
			gitTest(t, repo, "remote", "add", "origin", remote)
			gitTest(t, repo, "push", "-u", "origin", "main")
			gitTest(t, repo, "switch", "-c", "staging")
			gitTest(t, repo, "push", "-u", "origin", "staging")
			gitTest(t, repo, "switch", "main")
			gitTest(t, repo, "switch", "-c", "feature")
			if test.conflict {
				if err := os.WriteFile(filepath.Join(repo, "conflict.txt"), []byte("author version\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filepath.Join(repo, "author-before.txt"), []byte("author before\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitTest(t, repo, "add", ".")
			gitTest(t, repo, "commit", "-m", "first reviewed change")
			reviewed := strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD"))

			gitTest(t, repo, "switch", "staging")
			if test.conflict {
				if err := os.WriteFile(filepath.Join(repo, "conflict.txt"), []byte("target version\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filepath.Join(repo, "target-only.txt"), []byte("target change\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "docs"), []byte("target guidance\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitTest(t, repo, "add", ".")
			gitTest(t, repo, "commit", "-m", "advance staging")
			gitTest(t, repo, "push", "origin", "staging")
			targetTip := strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD"))

			if test.rewrite {
				gitTest(t, repo, "switch", "-c", "rewritten", "main")
				if err := os.WriteFile(filepath.Join(repo, "rewritten.txt"), []byte("rewritten candidate\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				gitTest(t, repo, "add", "rewritten.txt")
				gitTest(t, repo, "commit", "-m", "rewritten candidate")
			} else {
				gitTest(t, repo, "switch", "feature")
				gitTest(t, repo, "fetch", "origin", "staging")
				if test.targetNotMerged {
					if err := os.WriteFile(filepath.Join(repo, "author-after.txt"), []byte("author after\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					gitTest(t, repo, "add", "author-after.txt")
					gitTest(t, repo, "commit", "-m", "one change after review")
				} else {
					command := exec.Command("git", "-C", repo, "merge", "--no-edit", "origin/staging")
					command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
					output, mergeErr := command.CombinedOutput()
					if test.conflict {
						if mergeErr == nil {
							t.Fatal("expected the target merge to conflict")
						}
						if err := os.WriteFile(filepath.Join(repo, "conflict.txt"), []byte("resolved author and target\n"), 0o600); err != nil {
							t.Fatal(err)
						}
						gitTest(t, repo, "add", "conflict.txt")
						gitTest(t, repo, "commit", "-m", "resolve staging conflict")
					} else {
						if mergeErr != nil {
							t.Fatalf("clean target merge failed: %v\n%s", mergeErr, output)
						}
						if err := os.WriteFile(filepath.Join(repo, "author-after.txt"), []byte("author after\n"), 0o600); err != nil {
							t.Fatal(err)
						}
						gitTest(t, repo, "add", "author-after.txt")
						gitTest(t, repo, "commit", "-m", "one change after merge")
					}
				}
			}
			head := strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD"))
			guidance, err := readReviewGuidance(ctx, repo, "refs/remotes/origin/staging", "staging", []string{"docs"})
			if err != nil {
				t.Fatal(err)
			}
			if string(guidance["docs"]) != "target guidance\n" {
				t.Fatalf("guidance was not read from origin/staging: %q", guidance["docs"])
			}
			previous := store.ReviewRound{Round: 1, Repository: "shop", HeadSHA: reviewed, Status: "verdict"}
			scope, err := calculateReviewRoundScope(ctx, reviewRoundRepository{
				name: "shop", root: repo, worktree: repo, targetBranch: "staging", head: head,
			}, previous, 2, []string{"docs"})
			if err != nil {
				t.Fatal(err)
			}
			if scope.Binding.ScopeKind != test.wantKind {
				t.Fatalf("scope kind = %q, want %q", scope.Binding.ScopeKind, test.wantKind)
			}
			expectedTargetCommit := strings.TrimSpace(gitTest(t, repo, "merge-base", head, targetTip))
			if scope.Binding.TargetCommitSHA != expectedTargetCommit {
				t.Fatalf("target commit = %s, want merge base %s", scope.Binding.TargetCommitSHA, expectedTargetCommit)
			}
			if string(scope.Guidance["docs"]) != "target guidance\n" {
				t.Fatalf("Review Round did not freeze guidance from origin/staging: %q", scope.Guidance["docs"])
			}
			for _, value := range test.wantIn {
				if !strings.Contains(string(scope.DeltaPatch), value) {
					t.Errorf("delta patch omits %q:\n%s", value, scope.DeltaPatch)
				}
			}
			for _, value := range test.wantOut {
				if strings.Contains(string(scope.DeltaPatch), value) {
					t.Errorf("delta patch unexpectedly includes %q:\n%s", value, scope.DeltaPatch)
				}
			}
			if test.wantConflict != "" && (len(scope.Conflicts) != 1 || scope.Conflicts[0] != test.wantConflict) {
				t.Fatalf("virtual merge conflicts = %#v, want %q", scope.Conflicts, test.wantConflict)
			}
			if !test.conflict && len(scope.Conflicts) != 0 {
				t.Fatalf("unexpected virtual merge conflicts: %#v", scope.Conflicts)
			}
			if test.conflict {
				brief := string(generatedReviewBrief(store.Task{Seq: 1}, taskplan.Plan{}, 2, []reviewRoundScope{scope}, nil, false))
				if !strings.Contains(brief, "Conflicted paths: `conflict.txt`.") || !strings.Contains(brief, "Review them in `{{review_dir}}/cumulative.patch`.") {
					t.Fatalf("Review Round brief does not name the conflicted path and point to cumulative.patch:\n%s", brief)
				}
				if err := verifyReviewDeltaBindings(ctx, store.Project{}, []store.ReviewRound{scope.Binding}, scope.DeltaPatch, map[string]string{"shop": repo}); err != nil {
					t.Fatalf("delta.patch does not verify against its binding: %v", err)
				}
			}
			if test.name == "clean target merge then one author commit" && scope.Binding.TargetCommitSHA != targetTip {
				t.Fatalf("target commit = %s, want latest frozen target commit %s", scope.Binding.TargetCommitSHA, targetTip)
			}
		})
	}
}

func TestReviewRoundCumulativePatchStartsAtMergeBase(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	remote := filepath.Join(root, "remote.git")
	initRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\ntarget removes this line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "commit", "-am", "branch point")
	gitTest(t, root, "init", "--bare", remote)
	gitTest(t, repo, "remote", "add", "origin", remote)
	gitTest(t, repo, "push", "-u", "origin", "main")
	branchPoint := strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD"))

	gitTest(t, repo, "switch", "-c", "feature")
	if err := os.WriteFile(filepath.Join(repo, "change.txt"), []byte("change line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", "change.txt")
	gitTest(t, repo, "commit", "-m", "change")
	head := strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD"))

	gitTest(t, repo, "switch", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "target-only.txt"), []byte("target line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "target moves after the branch point")
	gitTest(t, repo, "push", "origin", "main")

	scope, err := calculateReviewRoundScope(ctx, reviewRoundRepository{
		name: "shop", root: repo, worktree: repo, targetBranch: "main", head: head,
	}, store.ReviewRound{}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if scope.Binding.TargetCommitSHA != branchPoint {
		t.Fatalf("target commit = %s, want merge base %s", scope.Binding.TargetCommitSHA, branchPoint)
	}
	want := gitTest(t, repo, "diff", "--binary", "--no-ext-diff", branchPoint, head)
	if string(scope.Cumulative) != want {
		t.Fatalf("cumulative.patch is not the merge base to head diff:\n%s\nwant:\n%s", scope.Cumulative, want)
	}
	for _, value := range []string{"target removes this line", "target-only.txt", "target line"} {
		if strings.Contains(string(scope.Cumulative), value) {
			t.Errorf("cumulative.patch includes target-side %q:\n%s", value, scope.Cumulative)
		}
	}
}

func TestReviewStartIgnoresMaxWorkersAtCapacity(t *testing.T) {
	f := newStartFixture(t)
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(`[defaults]
max_workers = 1
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
`), 0o600); err != nil {
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
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "change.txt"), []byte("review me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, change.WorktreePath, "add", "change.txt")
	gitTest(t, change.WorktreePath, "commit", "-m", "candidate change")
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Transition(ctx, change.ID, change.State, store.StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
	queued := store.Task{Type: "change", Title: "queued", ShortName: "queued", State: store.StateQueued, MergeMode: "local"}
	if _, _, err := db.CreateTaskWithSequence(ctx, change.ProjectID, f.project.Name, queued, func(int) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	var output bytes.Buffer
	cli := f.service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"review", "start", fmt.Sprintf("t%d", change.Seq), "--profile", "reviewer"}); code != 0 {
		t.Fatalf("review start at max_workers exit=%d output=%s", code, output.String())
	}
	rounds, err := db.ReviewRoundsFor(ctx, change.ID)
	if err != nil || len(rounds) != 1 || rounds[0].Status != "admitted" {
		t.Fatalf("rounds = %#v, %v", rounds, err)
	}
}

func TestReviewDispositionRefreshesAdmittedReviewerLedger(t *testing.T) {
	f := newStartFixture(t)
	if code, output := f.start(t); code != 0 {
		t.Fatalf("start code=%d %s", code, output)
	}
	change, _ := f.onlyTask(t)
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	reviewID, err := db.CreateTask(ctx, change.ProjectID, store.Task{Seq: change.Seq + 1, Type: "review", ReviewsTaskID: change.ID, Title: "Review round", MergeMode: "local", ReviewTargets: []store.ReviewTarget{{Repository: "shop", Commit: strings.Repeat("b", 40)}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO review_rounds(ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,verdict,created_at) VALUES(?,2,?,'shop','full',?,?,?,?,?,'[]','admitted',NULL,1)`, change.ID, reviewID, strings.Repeat("b", 40), strings.Repeat("b", 40), strings.Repeat("b", 40), strings.Repeat("b", 40), strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO review_patterns(id,ship_task_id,seq,key,title,rule,root_cause,fix_guidance,created_round) VALUES(1,?,1,'dropped-error','t','r','c','g',1)`, change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO review_findings(ship_task_id,seq,pattern_id,fingerprint,round,repository,path,symbol,excerpt,line_at_report,head_sha,severity,origin,in_scope,state) VALUES(?,1,1,?,1,'shop','x.go','f','e',1,?,'P1','introduced',1,'disputed')`, change.ID, strings.Repeat("a", 64), strings.Repeat("b", 40)); err != nil {
		t.Fatal(err)
	}
	reviewTask, err := db.TaskByID(ctx, change.ProjectID, reviewID)
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(f.home, "projects", f.project.Name, "tasks", taskIDString(reviewTask.Seq), "review", "ledger.md")
	stale, err := reviewLedgerMarkdown(ctx, db, change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeReviewRoundFiles(filepath.Dir(filepath.Dir(ledgerPath)), map[string][]byte{"review/ledger.md": []byte(stale)}); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyReviewDisposition(ctx, change.ID, []int{1}, nil, "withdraw", "", "", "not a defect", "approved", "user", "P1", false); err != nil {
		t.Fatal(err)
	}
	if err := refreshAdmittedReviewLedgers(ctx, db, f.home, f.project, change); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(ledgerPath)
	if err != nil || !strings.Contains(string(contents), "| F-1 | P-1 `dropped-error` | withdrawn | P1 |") {
		t.Fatalf("running reviewer ledger = %q, %v", contents, err)
	}
}
