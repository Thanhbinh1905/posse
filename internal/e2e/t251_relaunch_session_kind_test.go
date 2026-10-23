//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestFocusedRelaunchProfileSwitchDoesNotResumeAnotherHarnessSession(t *testing.T) {
	fixture := reviewIdentityFixture(t)
	defer fixture.db.Close()

	client := herdr.NewWithEnv("herdr", fixture.env)
	if err := os.MkdirAll(filepath.Join(fixture.root, "pi", "extensions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "integration", "install", "pi"); err != nil {
		t.Fatalf("install Pi integration in isolated Herdr: %v", err)
	}
	for _, setting := range [][2]string{
		{"profiles.reviewer.kind", "pi"},
		{"profiles.reviewer.model", "pi-model"},
		{"profiles.claude-opus.kind", "claude"},
		{"profiles.claude-opus.model", "opus"},
	} {
		runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", setting[0], setting[1])
	}

	if code, output := startReviewIdentityTask(t, fixture, "Author candidate", "author", ""); code != 0 {
		t.Fatalf("author spawn: code=%d %s", code, output)
	}
	if code, output := startReviewIdentityTask(t, fixture, "Pi review", "reviewer", "t1"); code != 0 {
		t.Fatalf("Pi review spawn: code=%d %s", code, output)
	}
	review := fixture.mustTask(t, "t2")
	// The shared review fixture keeps its poll-only Watch stopped, so it
	// cannot overwrite the synthetic session observation seeded below.
	const piSession = "pi-session-211"
	// Seed the session Herdr observed from Pi. Older schemas have no owner-kind
	// column, so this setup also reproduces the pre-fix database state.
	var hasOwnerKind int
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name='agent_session_kind'`).Scan(&hasOwnerKind); err != nil {
		t.Fatal(err)
	}
	query := `UPDATE tasks SET agent_session=? WHERE id=?`
	if hasOwnerKind != 0 {
		query = `UPDATE tasks SET agent_session=?, agent_session_kind='pi' WHERE id=?`
	}
	if _, err := fixture.db.ExecContext(context.Background(), query, `{"session_id":"`+piSession+`"}`, review.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := client.Call(context.Background(), "pane.focus", map[string]any{"pane_id": review.PaneID}); err != nil {
		t.Fatalf("focus review pane before relaunch: %v", err)
	}
	if code, output := runReviewIdentityCLI(t, fixture, "relaunch", "t2", "--profile", "claude-opus"); code != 0 {
		t.Fatalf("focused relaunch failed: code=%d\n%s", code, output)
	}

	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if task.Profile != "claude-opus" || !strings.Contains(task.AgentSession, piSession) || task.AgentSessionKind != "pi" {
		t.Errorf("focused relaunch corrupted the Task's Profile or session ownership: Profile=%q session=%q owner=%q", task.Profile, task.AgentSession, task.AgentSessionKind)
	}
	identities, err := fixture.db.TaskLaunchIdentities(context.Background(), review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 2 || identities[1].Profile != "claude-opus" || identities[1].ConfiguredModel != "opus" || !identities[1].ModelKnown {
		t.Errorf("focused relaunch lost its configured launch identity: %#v", identities)
	}

	log, err := os.ReadFile(filepath.Join(fixture.root, "review-identity-args.log"))
	if err != nil {
		t.Fatal(err)
	}
	var reviewStarts []string
	for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) == 2 && filepath.Clean(fields[0]) == filepath.Clean(review.WorktreePath) {
			reviewStarts = append(reviewStarts, fields[1])
		}
	}
	if len(reviewStarts) != 2 {
		t.Fatalf("review harness starts=%d, want initial Pi plus one Claude relaunch; log:\n%s", len(reviewStarts), log)
	}
	for attempt, args := range reviewStarts[1:] {
		if strings.Contains(args, piSession) || strings.Contains(args, "--resume") {
			t.Errorf("Claude relaunch %d received another harness's Pi session: args=%q; Task profile=%q session=%q", attempt+1, args, task.Profile, task.AgentSession)
		}
	}
	if len(reviewStarts) > 1 && (!strings.Contains(reviewStarts[1], "--model opus") || strings.Contains(reviewStarts[1], "relaunch.md")) {
		t.Errorf("focused launch used unexpected Claude Profile arguments: %q", reviewStarts[1])
	}
	input, err := os.ReadFile(filepath.Join(fixture.root, "review-identity-input.log"))
	if err != nil {
		t.Fatal(err)
	}
	relaunchPrompt := "Read " + filepath.Join(fixture.home, "projects", "shop", "tasks", "t2", "relaunch.md") + " and follow it."
	if !strings.Contains(string(input), relaunchPrompt) {
		t.Errorf("focused relaunch prompt was not typed into the new Claude session: want %q in %q", relaunchPrompt, input)
	}
}
