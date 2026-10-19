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

func TestRelaunchProfileSwitchDoesNotResumeAnotherHarnessSession(t *testing.T) {
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

	if code, output := rideReviewIdentityTask(t, fixture, "Author candidate", "author", ""); code != 0 {
		t.Fatalf("author spawn: code=%d %s", code, output)
	}
	if code, output := rideReviewIdentityTask(t, fixture, "Pi review", "reviewer", "t1"); code != 0 {
		t.Fatalf("Pi review spawn: code=%d %s", code, output)
	}
	review := fixture.mustTask(t, "t2")
	// This test seeds a synthetic observation. Stop the live Watch first so a
	// poll of the fixture's session-less Pi pane cannot overwrite that seed.
	stopBackgroundWatch(t, fixture)
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
		t.Fatalf("focus review pane to reproduce prompt-delivery failure: %v", err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		code, output := runReviewIdentityCLI(t, fixture, "relaunch", "t2", "--profile", "claude-opus")
		if code == 0 || !strings.Contains(output, "pane_focused") {
			t.Fatalf("relaunch %d did not fail at focused-pane prompt delivery: code=%d\n%s", attempt, code, output)
		}
	}

	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if task.Profile != "claude-opus" || !strings.Contains(task.AgentSession, piSession) || task.AgentSessionKind != "pi" {
		t.Errorf("failed prompt delivery corrupted the Task's Profile or session ownership: Profile=%q session=%q owner=%q", task.Profile, task.AgentSession, task.AgentSessionKind)
	}
	identities, err := fixture.db.TaskLaunchIdentities(context.Background(), review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 3 || identities[1].Profile != "claude-opus" || identities[1].ConfiguredModel != "opus" || !identities[1].ModelKnown || identities[2].Profile != "claude-opus" || identities[2].ConfiguredModel != "opus" || !identities[2].ModelKnown {
		t.Errorf("failed relaunches lost their configured launch identities: %#v", identities)
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
	if len(reviewStarts) != 3 {
		t.Fatalf("review harness starts=%d, want initial Pi plus two Claude relaunches; log:\n%s", len(reviewStarts), log)
	}
	for attempt, args := range reviewStarts[1:] {
		if strings.Contains(args, piSession) || strings.Contains(args, "--resume") {
			t.Errorf("Claude relaunch %d received another harness's Pi session: args=%q; Task profile=%q session=%q", attempt+1, args, task.Profile, task.AgentSession)
		}
	}
	if len(reviewStarts) > 1 && !strings.Contains(reviewStarts[1], "--model opus") {
		t.Errorf("first failed launch did not use the requested Claude Profile: %q", reviewStarts[1])
	}
}
