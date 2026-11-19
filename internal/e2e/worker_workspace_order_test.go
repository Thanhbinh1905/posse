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

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWorkerWorkspaceOrderGroupsTwoProjectsAndRelaunchesOnRealHerdr(t *testing.T) {
	f := newWorkerTabsFixture(t)
	secondRepo := filepath.Join(f.root, "greenhouse")
	initRepository(t, secondRepo, filepath.Join(f.root, "greenhouse-origin.git"), f.env)
	secondLead, err := createWorkspace(f.client, secondRepo)
	if err != nil {
		t.Fatal(err)
	}
	secondLeadEnv := setEnv(f.env, "HERDR_ENV", "1")
	secondLeadEnv = setEnv(secondLeadEnv, "HERDR_SOCKET_PATH", herdr.IsolatedTestSocketPath(f.root))
	secondLeadEnv = setEnv(secondLeadEnv, "HERDR_PANE_ID", secondLead.RootPane.PaneID)
	secondLeadEnv = setEnv(secondLeadEnv, "HERDR_WORKSPACE_ID", secondLead.Workspace.WorkspaceID)
	secondLeadEnv = setEnv(secondLeadEnv, "HERDR_TAB_ID", secondLead.RootPane.TabID)
	if output := runPosse(t, f.binary, secondRepo, secondLeadEnv, "up", "--name", "greenhouse", "--yes"); !strings.Contains(output, "greenhouse") {
		t.Fatalf("start second Project Lead: %s", output)
	}
	if !waitForCondition(10*time.Second, func() bool {
		for _, pane := range f.snapshot(t).Panes {
			if pane.PaneID == secondLead.RootPane.PaneID && pane.Agent != "" {
				return true
			}
		}
		return false
	}) {
		t.Fatal("second Project Lead did not start")
	}

	waitTask := func(repo, taskID, name, output string) store.Task {
		t.Helper()
		var project store.Project
		var task store.Task
		if !waitForCondition(20*time.Second, func() bool {
			db, err := store.OpenReadOnly(f.home)
			if err != nil {
				return false
			}
			defer db.Close()
			project, err = db.ProjectByRoot(context.Background(), repo)
			if err != nil {
				return false
			}
			task, err = db.Task(context.Background(), project.ID, taskID)
			return err == nil && task.State == store.StateWorking
		}) {
			t.Fatalf("%s Worker did not start: %s", name, output)
		}
		return task
	}
	start := func(repo string, env []string, taskID, name string) store.Task {
		t.Helper()
		brief := filepath.Join(f.root, name+".md")
		if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: "+name+"\ndone_when: the E2E verifies workspace placement\n---\nWait.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		output := runPosse(t, f.binary, repo, env, "start", "--brief", brief, "--name", name)
		if !strings.Contains(output, "task:") {
			t.Fatalf("start %s: %s", name, output)
		}
		return waitTask(repo, taskID, name, output)
	}
	shopFirst := start(f.repo, f.leadEnv, "t1", "shop-first")
	greenhouseFirst := start(secondRepo, secondLeadEnv, "t1", "greenhouse-first")
	shopSecond := start(f.repo, f.leadEnv, "t2", "shop-second")
	greenhouseSecond := start(secondRepo, secondLeadEnv, "t2", "greenhouse-second")

	type launch struct {
		repo, taskID, name string
		env                []string
	}
	jobs := []launch{
		{repo: f.repo, env: f.leadEnv, taskID: "t3", name: "shop-third"},
		{repo: secondRepo, env: secondLeadEnv, taskID: "t3", name: "greenhouse-third"},
	}
	commands := make([]*exec.Cmd, len(jobs))
	for i, job := range jobs {
		brief := filepath.Join(f.root, job.name+".md")
		if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: "+job.name+"\ndone_when: the E2E verifies workspace placement\n---\nWait.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		commands[i] = exec.Command(f.binary, "start", "--brief", brief, "--name", job.name)
		commands[i].Dir = job.repo
		commands[i].Env = taskAuthorGenerationEnv(job.env)
	}
	begin := make(chan struct{})
	outputs := make([][]byte, len(jobs))
	errs := make([]error, len(jobs))
	done := make(chan int, len(jobs))
	for i, command := range commands {
		go func(index int, command *exec.Cmd) {
			<-begin
			outputs[index], errs[index] = command.CombinedOutput()
			done <- index
		}(i, command)
	}
	close(begin)
	for range jobs {
		<-done
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent start %s: %v\n%s", jobs[i].name, err, outputs[i])
		}
	}
	shopThird := waitTask(f.repo, "t3", "shop-third", string(outputs[0]))
	greenhouseThird := waitTask(secondRepo, "t3", "greenhouse-third", string(outputs[1]))

	assertBlocks := func(snapshot herdr.Snapshot) {
		t.Helper()
		ids := make([]string, len(snapshot.Workspaces))
		for i, workspace := range snapshot.Workspaces {
			ids[i] = workspace.WorkspaceID
		}
		assertBlock := func(lead string, workers ...string) {
			t.Helper()
			index := -1
			for i, id := range ids {
				if id == lead {
					index = i
					break
				}
			}
			if index < 0 || index+len(workers) >= len(ids) {
				t.Fatalf("Lead workspace %s or Worker block is absent: %v", lead, ids)
			}
			for i, worker := range workers {
				if ids[index+1+i] != worker {
					t.Fatalf("workspace order after Lead %s = %v, want Worker block %v immediately after Lead; full order %v", lead, ids[index+1:], workers, ids)
				}
			}
		}
		assertBlock(f.leadWorkspaceID, shopFirst.HerdrWorkspaceID, shopSecond.HerdrWorkspaceID, shopThird.HerdrWorkspaceID)
		assertBlock(secondLead.Workspace.WorkspaceID, greenhouseFirst.HerdrWorkspaceID, greenhouseSecond.HerdrWorkspaceID, greenhouseThird.HerdrWorkspaceID)
	}
	assertBlocks(f.snapshot(t))

	f.fail(t, "t1")
	f.exitWorker(t, "t1")
	runPosse(t, f.binary, f.repo, f.leadEnv, "relaunch", "t1")
	f.waitState(t, "t1", store.StateWorking)
	shopFirst = f.task(t, "t1")
	assertBlocks(f.snapshot(t))

}
