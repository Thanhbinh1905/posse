//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestConversationHandoffMovesAcrossLeadHarnessOnce(t *testing.T) {
	fixture := newPRLifecycleFixtureWithLead(t, true)
	defer fixture.db.Close()

	client := herdr.NewWithEnv("herdr", fixture.env)
	if _, err := client.Run(t.Context(), "pane", "wait-output", fixture.project.LeadPaneID, "--match", "POSSE_TEST_CLAUDE_INTERRUPT_READY", "--source", "recent-unwrapped", "--timeout", "30000"); err != nil {
		snapshot, snapshotErr := client.Snapshot(t.Context())
		paneOutput, readErr := client.Run(t.Context(), "pane", "read", fixture.project.LeadPaneID, "--source", "recent-unwrapped", "--lines", "40")
		t.Fatalf("initial Claude Lead did not install its interrupt handler: %v; snapshot=%#v snapshotErr=%v paneOutput=%s readErr=%v", err, snapshot, snapshotErr, paneOutput, readErr)
	}

	const note = "Open question: should this workflow remain optional?\nNext: compare the User's answer with ADR 0027.\n"
	const piStarted = "POSSE_TEST_PI_STARTED"
	source := filepath.Join(fixture.root, "handoff.md")
	if err := os.WriteFile(source, []byte(note), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "handoff", "write", "--file", source); !strings.Contains(output, "state: written") {
		t.Fatalf("handoff write failed: %s", output)
	}

	pi := filepath.Join(fixture.root, "bin", "pi")
	piScript := "#!/bin/sh\n" +
		"trap 'herdr pane release-agent \"$HERDR_PANE_ID\" --source posse.fake --agent pi >/dev/null 2>&1 || true; exit 0' INT\n" +
		"printf '%s\\n' \"$@\" > \"$POSSE_TEST_ROOT/pi-args.log\"\n" +
		"printf '%s\\n' '" + piStarted + "'\n" +
		"herdr pane report-agent \"$HERDR_PANE_ID\" --source posse.fake --agent pi --state idle >/dev/null 2>&1\n" +
		"while IFS= read -r line; do herdr pane report-agent \"$HERDR_PANE_ID\" --source posse.fake --agent pi --state working >/dev/null 2>&1; done\n"
	if err := os.WriteFile(pi, []byte(piScript), 0o700); err != nil {
		t.Fatal(err)
	}

	startCount := 0
	startPi := func() {
		t.Helper()
		startCount++
		if err := os.Remove(filepath.Join(fixture.root, "pi-args.log")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		userWorkspace, err := createWorkspace(client, fixture.repo)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Call(context.Background(), "pane.focus", map[string]any{"pane_id": userWorkspace.RootPane.PaneID}); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Run(context.Background(), "pane", "run", userWorkspace.RootPane.PaneID, "posse up --pi --replace --yes"); err != nil {
			paneOutput, readErr := client.Run(context.Background(), "pane", "read", userWorkspace.RootPane.PaneID, "--source", "recent-unwrapped", "--lines", "40")
			t.Fatalf("replace Lead with Pi: %v\n%s\nreadErr=%v", err, paneOutput, readErr)
		}
		if _, err := client.Run(t.Context(), "pane", "wait-output", userWorkspace.RootPane.PaneID, "--match", piStarted, "--source", "recent-unwrapped", "--timeout", "30000"); err != nil {
			snapshot, snapshotErr := client.Snapshot(t.Context())
			paneOutput, readErr := client.Run(t.Context(), "pane", "read", userWorkspace.RootPane.PaneID, "--source", "recent-unwrapped", "--lines", "40")
			lead, leadErr := fixture.db.ProjectLeadAgent(context.Background(), fixture.project.ID)
			t.Fatalf("replacement Pi %d did not report its startup marker: %v; snapshot=%#v snapshotErr=%v lead=%#v leadErr=%v paneOutput=%s readErr=%v", startCount, err, snapshot, snapshotErr, lead, leadErr, paneOutput, readErr)
		}
	}
	startPi()

	instructionsPath := filepath.Join(fixture.home, "projects", "shop", "lead.md")
	instructions, err := os.ReadFile(instructionsPath)
	if err != nil || strings.Count(string(instructions), note) != 1 {
		t.Fatalf("replacement Lead instructions omitted or duplicated the note: %s, %v", instructions, err)
	}
	archive := filepath.Join(fixture.home, "projects", "shop", "handoff-archive", "handoff-0001.md")
	if !waitForCondition(10*time.Second, func() bool {
		_, err := os.Stat(archive)
		return err == nil
	}) {
		t.Fatal("loaded note was not archived after the replacement Lead started")
	}
	if _, err := os.Stat(filepath.Join(fixture.home, "projects", "shop", "handoff.md")); !os.IsNotExist(err) {
		t.Fatalf("pending note remains after startup: %v", err)
	}

	startPi()
	instructions, err = os.ReadFile(instructionsPath)
	if err != nil || strings.Contains(string(instructions), note) {
		t.Fatalf("second Lead restart reloaded the archived note: %s, %v", instructions, err)
	}
	if task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1"); err == nil || !store.IsNotFound(err) {
		t.Fatalf("handoff unexpectedly created a Task: %#v, %v", task, err)
	}
}
