package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func TestAgentAttributionTrailer(t *testing.T) {
	for line, want := range map[string]bool{
		"Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>":        true,
		"co-authored-by: Claude <noreply@anthropic.com>":                 true,
		"Co-authored-by: Codex <codex@openai.com>":                       true,
		"Co-authored-by: aider (gpt-5) <noreply@aider.chat>":             true,
		"Co-authored-by: Bot <bot@mail.anthropic.com>":                   true,
		"Assisted-by: Gemini":                                            true,
		"Co-authored-by: Jane Doe <jane@example.com>":                    false,
		"Co-authored-by: Claudette Smith <claudette@example.com>":        false,
		"Signed-off-by: Claude Shannon <claude@example.com>":             false,
		"Reviewed-by: Claude <noreply@anthropic.com>":                    false,
		"Co-authored-by: Mallory <mallory@anthropic.com.example.org>":    false,
		"Refs: Co-Authored-By: Claude <noreply@anthropic.com>":           false,
		"🤖 Generated with [Claude Code](https://claude.com/claude-code)": false,
	} {
		if got := agentAttributionTrailer(line); got != want {
			t.Errorf("agentAttributionTrailer(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestClaudeLaunchesDisableAttributionUnlessProfileKeepsIt(t *testing.T) {
	args := []string{"--model", "opus", "--settings", "profile.json"}
	got := prependAttributionArgs("claude", config.Profile{Kind: "claude"}, args)
	if !equalStrings(got, append([]string{"--settings", claudeNoAttributionSettings}, args...)) {
		t.Fatalf("default Claude launch args = %q", got)
	}
	if got := prependAttributionArgs("claude", config.Profile{Kind: "claude", Attribution: true}, args); !equalStrings(got, args) {
		t.Fatalf("attribution Profile changed args: %q", got)
	}
	if got := prependAttributionArgs("codex", config.Profile{Kind: "codex"}, args); !equalStrings(got, args) {
		t.Fatalf("Codex launch args changed: %q", got)
	}
}

func TestPublishRefusesAgentAttributionTrailerBeforePush(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateWorking)
	if err := os.WriteFile(filepath.Join(f.worktree, "agent.txt"), []byte("agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.worktree, "add", "agent.txt")
	gitTest(t, f.worktree, "commit", "-m", "agent change\n\nCo-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>")
	trailered := shortSHA(strings.TrimSpace(gitTest(t, f.worktree, "rev-parse", "HEAD")))
	code, output, _ := f.run("publish", "Worker completion summary")
	if code != 1 || !strings.Contains(output, "agent_attribution_refused") || !strings.Contains(output, trailered+" Co-Authored-By: Claude Opus 5.5") {
		t.Fatalf("publish with agent trailer: %d %s", code, output)
	}
	if got := strings.TrimSpace(gitTest(t, f.remote, "for-each-ref", "--format=%(refname)", "refs/heads/posse/")); got != "" {
		t.Fatalf("refused publish pushed %s", got)
	}

	gitTest(t, f.worktree, "commit", "--amend", "-m", "agent change\n\nCo-authored-by: Jane Doe <jane@example.com>")
	f.headSHA = strings.TrimSpace(gitTest(t, f.worktree, "rev-parse", "HEAD"))
	f.setGHHead(t, f.headSHA)
	if code, output, stderr := f.run("publish", "Worker completion summary"); code != 0 {
		t.Fatalf("publish after amend: %d %s %s", code, output, stderr)
	}
}

func TestAttributionCheckCoversOnlyTaskCommitsAndHonorsProfile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	gitTest(t, repo, "commit", "--allow-empty", "-m", "old main\n\nCo-Authored-By: Claude <noreply@anthropic.com>")
	gitTest(t, repo, "checkout", "-b", "posse/t1")
	gitTest(t, repo, "commit", "--allow-empty", "-m", "task change")
	task := store.Task{BaseRef: "main", TargetBranch: "main"}
	if err := refuseAgentAttribution(ctx, repo, "HEAD", task); err != nil {
		t.Fatalf("base commit trailer was checked: %v", err)
	}

	gitTest(t, repo, "commit", "--allow-empty", "-m", "agent change\n\nCo-Authored-By: Claude <noreply@anthropic.com>")
	if err := refuseAgentAttribution(ctx, repo, "HEAD", task); err == nil || !strings.Contains(err.Error(), "attribute an agent") {
		t.Fatalf("Task trailer was not refused: %v", err)
	}
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief:     taskplan.Select("brief", "change.md", []byte("---\ntype: change\ntitle: Change\ndone_when: published\n---\n")),
		Execution: taskplan.Execution{Scope: "repo", Base: "main", Target: "main"},
		Launch:    taskplan.Launch{ProfileName: "credited", Profile: config.Profile{Kind: "claude", Attribution: true}},
		Review:    "none", Delivery: "pr", Autonomy: config.Autonomy{Review: "ask", Merge: "ask"},
		Paths: taskplan.Paths{Scratch: filepath.Join(root, "scratch"), Output: filepath.Join(root, "output")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.PlanJSON, err = plan.Encode(); err != nil {
		t.Fatal(err)
	}
	if err := refuseAgentAttribution(ctx, repo, "HEAD", task); err != nil {
		t.Fatalf("Profile with attribution = true was refused: %v", err)
	}
}
