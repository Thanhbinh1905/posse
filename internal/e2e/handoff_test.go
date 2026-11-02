//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestConversationHandoffMovesAcrossLeadHarnessOnce(t *testing.T) {
	fixture := newPRLifecycleFixtureWithLead(t, true)
	defer fixture.db.Close()

	const note = "Open question: should this workflow remain optional?\nNext: compare the User's answer with ADR 0027.\n"
	source := filepath.Join(fixture.root, "handoff.md")
	if err := os.WriteFile(source, []byte(note), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "handoff", "write", "--file", source); !strings.Contains(output, "state: written") {
		t.Fatalf("handoff write failed: %s", output)
	}

	pi := filepath.Join(fixture.root, "bin", "pi")
	piScript := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$POSSE_TEST_ROOT/pi-args.log\"\n" +
		"herdr pane report-agent \"$HERDR_PANE_ID\" --source posse.fake --agent pi --state idle >/dev/null 2>&1\n" +
		"while IFS= read -r line; do herdr pane report-agent \"$HERDR_PANE_ID\" --source posse.fake --agent pi --state working >/dev/null 2>&1; done\n"
	if err := os.WriteFile(pi, []byte(piScript), 0o700); err != nil {
		t.Fatal(err)
	}

	startPi := func() {
		t.Helper()
		if err := os.Remove(filepath.Join(fixture.root, "pi-args.log")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		command := exec.Command(fixture.binary, "up", "--pi", "--replace", "--yes")
		command.Dir, command.Env = fixture.repo, fixture.callerEnv
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("replace Lead with Pi: %v\n%s", err, output)
		}
		if !waitForCondition(10*time.Second, func() bool {
			_, err := os.Stat(filepath.Join(fixture.root, "pi-args.log"))
			return err == nil
		}) {
			t.Fatal("replacement Pi did not start")
		}
	}
	startPi()

	instructionsPath := filepath.Join(fixture.home, "projects", "shop", "lead.md")
	instructions, err := os.ReadFile(instructionsPath)
	if err != nil || strings.Count(string(instructions), note) != 1 {
		t.Fatalf("replacement Lead instructions omitted or duplicated the note: %s, %v", instructions, err)
	}
	archive := filepath.Join(fixture.home, "projects", "shop", "handoff-archive", "handoff-0001.md")
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("loaded note was not archived: %v", err)
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
