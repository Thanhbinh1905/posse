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
	fixture := newWorkerTabsFixture(t)
	change := fixture.start(t, "t1", "Signal Findings", "signal-findings")
	changeScratch := filepath.Join(fixture.home, "scratch", "shop", "t1")
	otherChange := fixture.start(t, "t2", "Signal Findings Security", "signal-findings-security")
	otherChangeScratch := filepath.Join(fixture.home, "scratch", "shop", "t2")
	if err := os.MkdirAll(otherChangeScratch, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(fixture.root, "outside.md")
	if err := os.WriteFile(outside, []byte("outside evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(changeScratch, "foreign-findings.md"), []byte("foreign findings\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(otherChangeScratch, "findings-symlink.md")); err != nil {
		t.Fatal(err)
	}

	workerEnv := func(task store.Task) []string {
		env := setEnv(fixture.env, "HERDR_ENV", "1")
		env = setEnv(env, "HERDR_PANE_ID", task.PaneID)
		return setEnv(env, "HERDR_WORKSPACE_ID", task.HerdrWorkspaceID)
	}
	runSignal := func(task store.Task, args ...string) (int, string) {
		t.Helper()
		command := exec.Command(fixture.binary, args...)
		command.Dir, command.Env = task.WorktreePath, workerEnv(task)
		output, err := command.CombinedOutput()
		if err == nil {
			return 0, string(output)
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), string(output)
		}
		t.Fatalf("run Worker Signal: %v", err)
		return 1, string(output)
	}
	assertRejected := func(task store.Task, flag, path string) {
		t.Helper()
		code, output := runSignal(task, "signal", "needs-decision", "Need review", flag, path)
		taskScratch := filepath.Join(fixture.home, "scratch", "shop", "t"+strconv.Itoa(task.Seq))
		if code == 0 || !strings.Contains(output, "signal_invalid") || !strings.Contains(output, task.WorktreePath) || !strings.Contains(output, taskScratch) {
			t.Fatalf("foreign %s path was not refused with both allowed locations: code=%d output=%s", flag, code, output)
		}
	}

	if err := os.WriteFile(filepath.Join(changeScratch, "findings.md"), []byte("task findings\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runSignal(change, "signal", "needs-decision", "Need review", "--findings", filepath.Join(changeScratch, "findings.md")); code != 0 {
		t.Fatalf("own Task scratch --findings failed: code=%d output=%s", code, output)
	}
	if !waitForCondition(10*time.Second, func() bool {
		contents, err := os.ReadFile(filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "findings.toon"))
		return err == nil && string(contents) == "task findings\n"
	}) {
		t.Fatal("accepted findings file was not copied into the Task record")
	}
	assertRejected(otherChange, "--findings", filepath.Join(changeScratch, "foreign-findings.md"))
	assertRejected(otherChange, "--findings", filepath.Join(otherChangeScratch, "findings-symlink.md"))
	assertRejected(otherChange, "--findings", outside)

	researchBrief := filepath.Join(fixture.root, "research.md")
	brief := "---\ntype: research\ntitle: Research Signals\ndone_when: the report is recorded\n---\nInspect the Project.\n"
	if err := os.WriteFile(researchBrief, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", researchBrief, "--name", "research-signals"); !strings.Contains(output, "t3") {
		t.Fatalf("Research start did not return t3: %s", output)
	}
	fixture.waitState(t, "t3", store.StateWorking)
	research := fixture.task(t, "t3")
	researchScratch := filepath.Join(fixture.home, "scratch", "shop", "t3")
	if err := os.WriteFile(filepath.Join(changeScratch, "foreign-report.md"), []byte("foreign report\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(researchScratch, "report-symlink.md")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(changeScratch, "foreign-report.md"), filepath.Join(researchScratch, "report-symlink.md"), outside} {
		code, output := runSignal(research, "signal", "done", "Research complete", "--report", path)
		if code == 0 || !strings.Contains(output, "signal_invalid") || !strings.Contains(output, research.WorktreePath) || !strings.Contains(output, researchScratch) {
			t.Fatalf("foreign --report path was not refused with both allowed locations: code=%d output=%s", code, output)
		}
	}
	if err := os.WriteFile(filepath.Join(researchScratch, "report.md"), []byte("Task report\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	launch, err := os.ReadFile(filepath.Join(fixture.home, "projects", "shop", "tasks", "t3", "launch.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{filepath.Join(fixture.home, "scratch", "shop", "t3"), "--findings", "--report"} {
		if !strings.Contains(string(launch), want) {
			t.Errorf("Worker protocol omitted %q:\n%s", want, launch)
		}
	}
	if code, output := runSignal(research, "signal", "done", "Research complete", "--report", filepath.Join(researchScratch, "report.md")); code != 0 {
		t.Fatalf("own Task scratch --report failed: code=%d output=%s", code, output)
	}
	if !waitForCondition(10*time.Second, func() bool {
		contents, err := os.ReadFile(filepath.Join(fixture.home, "projects", "shop", "tasks", "t3", "report.md"))
		return err == nil && string(contents) == "Task report\n"
	}) {
		t.Fatal("accepted report file was not copied into the Task record")
	}
	fixture.waitState(t, "t3", store.StateReported)
}
