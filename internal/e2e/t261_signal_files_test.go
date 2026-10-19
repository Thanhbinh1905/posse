//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestSignalFilesAcceptOwnTaskScratchAndRejectOtherRoots(t *testing.T) {
	fixture := newRiderTabsFixture(t)
	ship := fixture.ride(t, "t1", "Signal Findings", "signal-findings")
	shipScratch := filepath.Join(fixture.home, "scratch", "shop", "t1")
	otherShip := fixture.ride(t, "t2", "Signal Findings Security", "signal-findings-security")
	otherShipScratch := filepath.Join(fixture.home, "scratch", "shop", "t2")
	if err := os.MkdirAll(otherShipScratch, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(fixture.root, "outside.md")
	if err := os.WriteFile(outside, []byte("outside evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shipScratch, "foreign-findings.md"), []byte("foreign findings\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(otherShipScratch, "findings-symlink.md")); err != nil {
		t.Fatal(err)
	}

	riderEnv := func(task store.Task) []string {
		env := setEnv(fixture.env, "HERDR_ENV", "1")
		env = setEnv(env, "HERDR_PANE_ID", task.PaneID)
		return setEnv(env, "HERDR_WORKSPACE_ID", task.HerdrWorkspaceID)
	}
	runSignalAt := func(task store.Task, dir string, args ...string) (int, string) {
		t.Helper()
		command := exec.Command(fixture.binary, args...)
		command.Dir, command.Env = dir, riderEnv(task)
		output, err := command.CombinedOutput()
		if err == nil {
			return 0, string(output)
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), string(output)
		}
		t.Fatalf("run Rider Signal: %v", err)
		return 1, string(output)
	}
	runSignal := func(task store.Task, args ...string) (int, string) {
		return runSignalAt(task, task.WorktreePath, args...)
	}
	assertRejected := func(task store.Task, flag, path string) {
		t.Helper()
		code, output := runSignal(task, "holler", "needs-decision", "Need review", flag, path)
		taskScratch := filepath.Join(fixture.home, "scratch", "shop", "t"+strconv.Itoa(task.Seq))
		if code == 0 || !strings.Contains(output, "signal_invalid") || !strings.Contains(output, "Signal files must be inside") || !strings.Contains(output, task.WorktreePath) || !strings.Contains(output, taskScratch) {
			t.Fatalf("foreign %s path was not refused with both allowed locations: code=%d output=%s", flag, code, output)
		}
	}

	if err := os.WriteFile(filepath.Join(shipScratch, "findings.md"), []byte("task findings\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runSignal(ship, "holler", "needs-decision", "Need review", "--findings", filepath.Join(shipScratch, "findings.md")); code != 0 {
		t.Fatalf("own Task scratch --findings failed: code=%d output=%s", code, output)
	}
	if !waitForCondition(10*time.Second, func() bool {
		contents, err := os.ReadFile(filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "findings.toon"))
		return err == nil && string(contents) == "task findings\n"
	}) {
		t.Fatal("accepted findings file was not copied into the Task record")
	}
	assertRejected(otherShip, "--findings", filepath.Join(shipScratch, "foreign-findings.md"))
	assertRejected(otherShip, "--findings", filepath.Join(otherShipScratch, "findings-symlink.md"))
	assertRejected(otherShip, "--findings", outside)

	scoutBrief := filepath.Join(fixture.root, "scout.md")
	brief := "---\ntype: scout\ntitle: Scout Signals\ndone_when: the report is recorded\n---\nInspect the Project.\n"
	if err := os.WriteFile(scoutBrief, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "ride", "--brief", scoutBrief, "--name", "scout-signals"); !strings.Contains(output, "t3") {
		t.Fatalf("Scout ride did not return t3: %s", output)
	}
	fixture.waitState(t, "t3", store.StateWorking)
	scout := fixture.task(t, "t3")
	scoutScratch := filepath.Join(fixture.home, "scratch", "shop", "t3")
	if err := os.WriteFile(filepath.Join(shipScratch, "foreign-report.md"), []byte("foreign report\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(scoutScratch, "report-symlink.md")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(shipScratch, "foreign-report.md"), filepath.Join(scoutScratch, "report-symlink.md"), outside} {
		code, output := runSignal(scout, "holler", "done", "Scout complete", "--report", path)
		if code == 0 || !strings.Contains(output, "signal_invalid") || !strings.Contains(output, scout.WorktreePath) || !strings.Contains(output, scoutScratch) {
			t.Fatalf("foreign --report path was not refused with both allowed locations: code=%d output=%s", code, output)
		}
	}
	if err := os.Mkdir(filepath.Join(scout.WorktreePath, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scout.WorktreePath, "report.md"), []byte("Task report\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	launch, err := os.ReadFile(filepath.Join(fixture.home, "projects", "shop", "tasks", "t3", "launch.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{filepath.Join(fixture.home, "scratch", "shop", "t3"), "--findings", "--report"} {
		if !strings.Contains(string(launch), want) {
			t.Errorf("Rider protocol omitted %q:\n%s", want, launch)
		}
	}
	if code, output := runSignalAt(scout, filepath.Join(scout.WorktreePath, "nested"), "holler", "done", "Scout complete", "--report", "report.md"); code != 0 {
		t.Fatalf("worktree-relative --report from a Rider subdirectory failed: code=%d output=%s", code, output)
	}
	if !waitForCondition(10*time.Second, func() bool {
		contents, err := os.ReadFile(filepath.Join(fixture.home, "projects", "shop", "tasks", "t3", "report.md"))
		return err == nil && string(contents) == "Task report\n"
	}) {
		t.Fatal("accepted report file was not copied into the Task record")
	}
	fixture.waitState(t, "t3", store.StateReported)
}
