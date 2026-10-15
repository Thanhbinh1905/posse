//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestPiExtensionSessionReplacementOwnsOneLookoutGeneration(t *testing.T) {
	root := newFixtureRoot(t, fixturePrefix("pi-"))
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	env := isolatedE2EEnv(t, root)
	binary := filepath.Join(binDir, "posse")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = moduleRoot(t)
	build.Env = env
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse: %v\n%s", err, output)
	}
	env = setEnv(env, "POSSE_TEST_ROOT", root)
	env = setEnv(env, "POSSE_E2E_POSSE_BIN", binary)
	env = setEnv(env, "PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	initRepository(t, repo, filepath.Join(root, "origin.git"), env)
	client := herdr.NewWithEnv("herdr", env)
	startServer(t, client)
	workspace, err := createWorkspace(client, repo)
	if err != nil {
		t.Fatalf("create private Herdr workspace: %v", err)
	}

	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, workspace.Workspace.WorkspaceID, workspace.RootPane.PaneID, "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	env = setEnv(env, "POSSE_E2E_PI_EXTENSION", filepath.Join(moduleRoot(t), "internal/app/lead_pi_extension.ts"))
	env = setEnv(env, "POSSE_E2E_LIFECYCLE_STATE", filepath.Join(root, "lifecycle-state.json"))
	if err := os.MkdirAll(filepath.Dir(config.ConfigPath(home, "shop")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.ConfigPath(home, "shop"), []byte("[lowkey]\nlead = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	harnessEnv := setEnv(env, "HERDR_ENV", "1")
	harnessEnv = setEnv(harnessEnv, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	harnessEnv = setEnv(harnessEnv, "HERDR_WORKSPACE_ID", workspace.Workspace.WorkspaceID)
	harnessEnv = setEnv(harnessEnv, "HERDR_TAB_ID", workspace.RootPane.TabID)
	command := exec.Command("node", filepath.Join(moduleRoot(t), "internal/e2e/testdata/pi_notice_lifecycle.mjs"))
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Dir = repo
	command.Env = harnessEnv
	var output strings.Builder
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()

	ready := filepath.Join(root, "lifecycle-state.json")
	if !waitForCondition(20*time.Second, func() bool {
		data, err := os.ReadFile(ready)
		if err != nil {
			return false
		}
		var state piLifecycleState
		return json.Unmarshal(data, &state) == nil && state.Session == "fork-session" && state.Stage == "ready-first-notice" && state.ActiveLookouts == 1
	}) {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		data, _ := os.ReadFile(ready)
		t.Fatalf("Pi harness did not signal readiness for the first Notice batch: state=%s output=%s", data, output.String())
	}

	routineID, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "pr_opened", Summary: "safe routine Notice"})
	if err != nil {
		t.Fatal(err)
	}
	actionableID, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "actionable Notice"})
	if err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(20*time.Second, func() bool {
		data, err := os.ReadFile(ready)
		if err != nil {
			return false
		}
		var state piLifecycleState
		return json.Unmarshal(data, &state) == nil && state.Stage == "ready-send-failure" && state.ActiveLookouts == 1
	}) {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		data, _ := os.ReadFile(ready)
		t.Fatalf("Pi extension did not re-arm after accepting the first batch: state=%s output=%s", data, output.String())
	}
	sendFailureID, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "recoverable send failure"})
	if err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(20*time.Second, func() bool {
		data, err := os.ReadFile(ready)
		if err != nil {
			return false
		}
		var state piLifecycleState
		return json.Unmarshal(data, &state) == nil && state.Stage == "ready-replacement" && state.ActiveLookouts == 1
	}) {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		data, _ := os.ReadFile(ready)
		t.Fatalf("Pi extension did not re-arm after the rejected send: state=%s output=%s", data, output.String())
	}
	replacementID, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "accepted but not persisted before replacement"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Pi extension lifecycle adapter: %v output=%s", err, output.String())
		}
	case <-time.After(30 * time.Second):
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatalf("Pi extension did not settle and auto-rearm: %s", output.String())
	}

	var result piLifecycleResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &result); err != nil {
		t.Fatalf("decode Pi lifecycle result %q: %v", output.String(), err)
	}
	if result.MaxActiveLookouts != 1 {
		t.Fatalf("session replacements overlapped lookout generations: maximum active=%d destinations=%v", result.MaxActiveLookouts, result.Destinations)
	}
	if !reflect.DeepEqual(result.Reasons, []string{"reload", "new", "resume", "fork", "new", "quit"}) {
		t.Fatalf("Pi session lifecycle reasons = %v", result.Reasons)
	}
	if !reflect.DeepEqual(result.DeliveredNoticeIDs, []int64{actionableID}) {
		t.Fatalf("lowkey wake did not quietly acknowledge only the safe routine Notice: routine=%d actionable=%d result=%#v", routineID, actionableID, result)
	}
	routine, err := db.NoticesByIDs(ctx, project.ID, []int64{routineID})
	if err != nil || routine[0].AckedAt == 0 || routine[0].DeliveredAt == 0 {
		t.Fatalf("safe routine acknowledgment = %#v, %v", routine, err)
	}
	actionable, err := db.NoticesByIDs(ctx, project.ID, []int64{actionableID})
	if err != nil || actionable[0].DeliveredAt == 0 {
		t.Fatalf("actionable batch was not accepted after session lifecycle: %#v, %v", actionable, err)
	}
	if result.AutoRearmedLookouts < 1 {
		t.Fatalf("Pi extension did not auto-rearm after the ordinary Notice wake: %#v", result)
	}
	if !reflect.DeepEqual(result.SendFailureNoticeIDs, []int64{sendFailureID}) || result.SendFailureAttempts != 2 || !result.SendFailureKeptStableID {
		t.Fatalf("known send failure did not retry the same batch by ID: notice=%d result=%#v", sendFailureID, result)
	}
	if result.FailedLookoutAttempts != 5 || result.HealthEpisodes != 1 {
		t.Fatalf("lookout spawn failure retries/health episode = %d/%d, want 5/1", result.FailedLookoutAttempts, result.HealthEpisodes)
	}
	if !reflect.DeepEqual(result.ReplacementNoticeIDs, []int64{replacementID}) || !reflect.DeepEqual(result.ReplacementUncertaintyIDs, []int64{replacementID}) || !result.ReplacementPendingRetained || result.ReplacementDeliveryID == "" || result.ReplacementBatchID == "" {
		t.Fatalf("accepted-but-unconsumed replacement lost recoverable IDs: notice=%d result=%#v", replacementID, result)
	}
	replacementReceipt, err := db.NoticeDelivery(ctx, result.ReplacementDeliveryID)
	if err != nil || replacementReceipt.State != "uncertain" || !reflect.DeepEqual(replacementReceipt.NoticeIDs, []int64{replacementID}) || replacementReceipt.BatchID != result.ReplacementBatchID {
		t.Fatalf("replacement delivery receipt = %#v, %v", replacementReceipt, err)
	}
	secondActionable, err := db.NoticesByIDs(ctx, project.ID, []int64{sendFailureID})
	if err != nil || secondActionable[0].DeliveredAt == 0 {
		t.Fatalf("retried send batch was not accepted: %#v, %v", secondActionable, err)
	}
}

type piLifecycleState struct {
	Session        string `json:"session"`
	ActiveLookouts int    `json:"activeLookouts"`
	Stage          string `json:"stage"`
}

type piLifecycleResult struct {
	Reasons                    []string `json:"reasons"`
	Destinations               []string `json:"destinations"`
	MaxActiveLookouts          int      `json:"maxActiveLookouts"`
	AutoRearmedLookouts        int      `json:"autoRearmedLookouts"`
	DeliveredNoticeIDs         []int64  `json:"deliveredNoticeIDs"`
	SendFailureNoticeIDs       []int64  `json:"sendFailureNoticeIDs"`
	SendFailureAttempts        int      `json:"sendFailureAttempts"`
	SendFailureKeptStableID    bool     `json:"sendFailureKeptStableID"`
	ReplacementNoticeIDs       []int64  `json:"replacementNoticeIDs"`
	ReplacementDeliveryID      string   `json:"replacementDeliveryID"`
	ReplacementBatchID         string   `json:"replacementBatchID"`
	ReplacementPendingRetained bool     `json:"replacementPendingRetained"`
	ReplacementUncertaintyIDs  []int64  `json:"replacementUncertaintyIDs"`
	FailedLookoutAttempts      int      `json:"failedLookoutAttempts"`
	HealthEpisodes             int      `json:"healthEpisodes"`
}
