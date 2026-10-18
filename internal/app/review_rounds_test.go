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
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestReviewStartAdmitsFrozenRoundAndGeneratedFiles(t *testing.T) {
	f := newRideFixture(t)
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(`[defaults]
landing_mode = "local"

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
	if code, output := f.ride(t); code != 0 {
		t.Fatalf("ride code=%d %s", code, output)
	}
	ship, _ := f.onlyTask(t)
	if err := os.WriteFile(filepath.Join(ship.WorktreePath, "author-change.txt"), []byte("review me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, ship.WorktreePath, "add", "author-change.txt")
	gitTest(t, ship.WorktreePath, "commit", "-m", "author change")
	head := strings.TrimSpace(gitTest(t, ship.WorktreePath, "rev-parse", "HEAD"))
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Transition(ctx, ship.ID, ship.State, store.StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	start := func() (int, string) {
		var output bytes.Buffer
		cli := f.service.CLI()
		cli.Out, cli.ErrOut = &output, &output
		code := cli.Run([]string{"review", "start", fmt.Sprintf("t%d", ship.Seq), "--profile", "reviewer"})
		return code, output.String()
	}
	if code, output := start(); code != 0 {
		t.Fatalf("review start code=%d %s", code, output)
	}
	rounds, err := db.ReviewRoundsFor(ctx, ship.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("admitted rounds = %#v, %v", rounds, err)
	}
	round := rounds[0]
	target := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "refs/remotes/origin/main"))
	if round.Round != 1 || round.Status != "admitted" || round.HeadSHA != head || round.TargetCommitSHA != target || round.ScopeBaseSHA != target || round.Repository != "shop" {
		t.Fatalf("round binding = %#v, target=%s head=%s", round, target, head)
	}
	review, err := db.TaskByID(ctx, ship.ProjectID, round.ReviewTaskID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := review.AdmittedPlan()
	if err != nil || plan == nil || len(plan.Inputs) == 0 || !strings.Contains(string(plan.Inputs[0].Bytes), "Create a committed file.") {
		t.Fatalf("generated Review Brief lost Ship intent: plan=%#v err=%v", plan, err)
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
		Round        int `json:"round"`
		MaxRounds    int `json:"max_rounds"`
		Remaining    int `json:"remaining_rounds"`
		Repositories []struct {
			HeadSHA         string `json:"head_sha"`
			TargetCommitSHA string `json:"target_commit_sha"`
			DeltaSHA256     string `json:"delta_sha256"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(read("review/round.json"), &metadata); err != nil {
		t.Fatal(err)
	}
	delta := read("review/delta.patch")
	if metadata.Round != 1 || metadata.MaxRounds != 1 || metadata.Remaining != 0 || len(metadata.Repositories) != 1 || metadata.Repositories[0].HeadSHA != head || metadata.Repositories[0].TargetCommitSHA != target || !strings.Contains(string(delta), "author-change.txt") {
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
	if err != nil || !strings.Contains(string(launch), "## Review Round 1") || !strings.Contains(string(launch), "review/delta.patch") {
		t.Fatalf("launch instructions omit Review Round files: %s err=%v", launch, err)
	}
	if err := os.Remove(filepath.Join(plan.Paths.Output, "review", "delta.patch")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(launchPath); err != nil {
		t.Fatal(err)
	}
	if err := restoreAdmittedLaunchFiles(f.project, review, plan); err != nil {
		t.Fatalf("restore generated review inputs: %v", err)
	}
	if restored := read("review/delta.patch"); !bytes.Equal(restored, delta) {
		t.Fatalf("restored delta patch differs from the admitted input: %s", restored)
	}
	restoredLaunch, err := os.ReadFile(launchPath)
	if err != nil || !strings.Contains(string(restoredLaunch), "## Review Round 1") {
		t.Fatalf("restored launch omits Review Round instructions: %s err=%v", restoredLaunch, err)
	}
	beforeTabs := f.fake.CallCount("tab.create")
	if code, output := start(); code != 1 || !strings.Contains(output, "review_round_active") {
		t.Fatalf("active round refusal code=%d: %s", code, output)
	}
	if f.fake.CallCount("tab.create") != beforeTabs {
		t.Fatal("active-round refusal created another pane")
	}
	if _, err := db.ExecContext(ctx, `UPDATE review_rounds SET status='verdict',verdict='pass' WHERE ship_task_id=?`, ship.ID); err != nil {
		t.Fatal(err)
	}
	if code, output := start(); code != 1 || !strings.Contains(output, "review_round_unchanged") {
		t.Fatalf("unchanged head refusal code=%d: %s", code, output)
	}
	if err := os.WriteFile(filepath.Join(ship.WorktreePath, "later-change.txt"), []byte("new review scope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, ship.WorktreePath, "add", "later-change.txt")
	gitTest(t, ship.WorktreePath, "commit", "-m", "later change")
	if code, output := start(); code != 1 || !strings.Contains(output, "review_round_cap") {
		t.Fatalf("round cap refusal code=%d: %s", code, output)
	}
}

func TestAutoStartReviewLoopStartsOnlyAfterGatePasses(t *testing.T) {
	f := newRideFixture(t)
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(`[defaults]
landing_mode = "local"
gate = ["test -f gate.ok"]

[profiles.author]
kind = "claude"
model = "model-a"

[profiles.reviewer]
kind = "claude"
model = "model-b"

[[dispatch]]
type = "ship"
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
	if code, output := f.ride(t); code != 0 {
		t.Fatalf("ride code=%d %s", code, output)
	}
	ship, _ := f.onlyTask(t)
	if err := os.WriteFile(filepath.Join(ship.WorktreePath, "change.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, ship.WorktreePath, "add", "change.txt")
	gitTest(t, ship.WorktreePath, "commit", "-m", "candidate change")
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Transition(ctx, ship.ID, ship.State, store.StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	land := func() (int, string) {
		t.Helper()
		var output bytes.Buffer
		cli := f.service.CLI()
		cli.Out, cli.ErrOut = &output, &output
		code := cli.Run([]string{"land", fmt.Sprintf("t%d", ship.Seq)})
		return code, output.String()
	}
	beforeTabs := f.fake.CallCount("tab.create")
	if code, output := land(); code != 1 || !strings.Contains(output, "gate_failed") {
		t.Fatalf("failed gate exit=%d output=%s", code, output)
	}
	rounds, err := db.ReviewRoundsFor(ctx, ship.ID)
	if err != nil || len(rounds) != 0 || f.fake.CallCount("tab.create") != beforeTabs {
		t.Fatalf("auto_start admitted before gate pass: rounds=%#v err=%v tabs=%d", rounds, err, f.fake.CallCount("tab.create"))
	}
	if err := os.WriteFile(filepath.Join(ship.WorktreePath, "gate.ok"), []byte("pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, ship.WorktreePath, "add", "gate.ok")
	gitTest(t, ship.WorktreePath, "commit", "-m", "satisfy gate")
	landCode, landOutput := land()
	if landCode != 0 {
		t.Fatalf("auto-start land exit=%d output=%s", landCode, landOutput)
	}
	rounds, err = db.ReviewRoundsFor(ctx, ship.ID)
	if err != nil || len(rounds) != 1 || rounds[0].Round != 1 || rounds[0].Status != "admitted" {
		current, _ := db.TaskByID(ctx, ship.ProjectID, ship.ID)
		frozen, _ := current.AdmittedPlan()
		t.Fatalf("auto-start rounds = %#v, err=%v, output=%s, state=%s loops=%#v", rounds, err, landOutput, current.State, frozen.ValidationLoops)
	}
	review, err := db.TaskByID(ctx, ship.ProjectID, rounds[0].ReviewTaskID)
	if err != nil || review.Profile != "reviewer" {
		t.Fatalf("auto-start Review Task = %#v, %v", review, err)
	}
	current, err := db.TaskByID(ctx, ship.ProjectID, ship.ID)
	if err != nil || current.State != store.StateDone {
		t.Fatalf("auto-start moved Ship Task before review: %#v, %v", current, err)
	}
}

func TestAutoStartSkipsUnavailableReviewerWithoutBlockingLand(t *testing.T) {
	for _, test := range []struct {
		name         string
		config       string
		explicitCode string
	}{
		{
			name: "ineligible identity",
			config: `[defaults]
landing_mode = "local"

[profiles.author]
kind = "claude"

[dispatch.default]
use = "author"

[review_loop]
enabled = true
auto_start = true
`,
			explicitCode: "review_identity_ineligible",
		},
		{
			name: "no review dispatch",
			config: `[defaults]
landing_mode = "local"

[profiles.author]
kind = "claude"

[[dispatch]]
type = "ship"
use = "author"

[review_loop]
enabled = true
auto_start = true
`,
			explicitCode: "profile_required",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertAutoStartSkipsUnavailableReviewer(t, test.config, test.explicitCode)
		})
	}
}

func assertAutoStartSkipsUnavailableReviewer(t *testing.T, configText, explicitCode string) {
	t.Helper()
	f := newRideFixture(t)
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "origin.git")
	gitTest(t, f.repo, "init", "--bare", remote)
	gitTest(t, f.repo, "remote", "add", "origin", remote)
	gitTest(t, f.repo, "push", "-u", "origin", "main")
	if code, output := f.ride(t); code != 0 {
		t.Fatalf("ride code=%d %s", code, output)
	}
	ship, _ := f.onlyTask(t)
	if err := os.WriteFile(filepath.Join(ship.WorktreePath, "change.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, ship.WorktreePath, "add", "change.txt")
	gitTest(t, ship.WorktreePath, "commit", "-m", "candidate change")
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Transition(ctx, ship.ID, ship.State, store.StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	var output bytes.Buffer
	cli := f.service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"land", fmt.Sprintf("t%d", ship.Seq)}); code != 0 {
		t.Fatalf("Land blocked by unavailable auto-start reviewer: code=%d output=%s", code, output.String())
	}
	current, err := db.TaskByID(ctx, ship.ProjectID, ship.ID)
	if err != nil || current.State != store.StateLanding {
		t.Fatalf("Land did not reach landing: task=%#v err=%v", current, err)
	}
	rounds, err := db.ReviewRoundsFor(ctx, ship.ID)
	if err != nil || len(rounds) != 0 {
		t.Fatalf("unavailable auto-start admitted Review Rounds: %#v, %v", rounds, err)
	}
	assertOneReviewAutoStartReadinessNotice(t, db, ctx, ship)
	current, err = db.TaskByID(ctx, ship.ProjectID, ship.ID)
	if err != nil {
		t.Fatal(err)
	}
	started, err := f.service.maybeAutoStartReviewRound(&axi.Context{Context: ctx, Out: &bytes.Buffer{}}, db, f.home, f.project, current)
	if err != nil || started {
		t.Fatalf("repeat auto-start result = started %v, err %v", started, err)
	}
	assertOneReviewAutoStartReadinessNotice(t, db, ctx, ship)
	output.Reset()
	if code := cli.Run([]string{"review", "start", fmt.Sprintf("t%d", ship.Seq)}); code == 0 || !strings.Contains(output.String(), explicitCode) {
		t.Fatalf("explicit review start should fail with %s: code=%d output=%s", explicitCode, code, output.String())
	}
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

func assertOneReviewAutoStartReadinessNotice(t *testing.T, db *store.DB, ctx context.Context, ship store.Task) {
	t.Helper()
	notices, err := db.Notices(ctx, ship.ProjectID, false)
	if err != nil {
		t.Fatal(err)
	}
	skipped := 0
	for _, notice := range notices {
		if notice.Kind != "review_auto_start_skipped" {
			continue
		}
		skipped++
		if !strings.Contains(notice.Summary, "eligible reviewer") || !strings.Contains(notice.Summary, "posse review start") {
			t.Fatalf("auto-start readiness notice = %#v", notice)
		}
	}
	if skipped != 1 {
		t.Fatalf("auto-start readiness notices = %d, want exactly one; all notices=%#v", skipped, notices)
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
			wantIn:       []string{"resolved"},
			wantOut:      []string{"target-only.txt", "author-before.txt"},
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
			name:     "rewritten Ship branch",
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
			if test.name == "clean target merge then one author commit" && scope.Binding.TargetCommitSHA != targetTip {
				t.Fatalf("target commit = %s, want latest frozen target commit %s", scope.Binding.TargetCommitSHA, targetTip)
			}
		})
	}
}
