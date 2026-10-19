//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type acknowledgedNoticeFixture struct {
	root, repo, home, binary, destination string
	env                                   []string
	db                                    *store.DB
	project                               store.Project
	noticeID                              int64
}

func newAcknowledgedNoticeFixture(t *testing.T) *acknowledgedNoticeFixture {
	t.Helper()
	// Leave room for herdr-client.sock under the Rider's longer scratch path.
	root := newFixtureRoot(t, fixturePrefix("notice-ack-"))
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	env := isolatedE2EEnv(t, root)
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(binDir, "posse")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir, build.Env = moduleRoot(t), env
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse: %v\n%s", err, output)
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
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, workspace.Workspace.WorkspaceID, workspace.RootPane.PaneID, "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	noticeID, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "Requires handled ack"})
	if err != nil {
		t.Fatal(err)
	}
	env = setEnv(env, "PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	env = setEnv(env, "HERDR_ENV", "1")
	env = setEnv(env, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	env = setEnv(env, "HERDR_WORKSPACE_ID", workspace.Workspace.WorkspaceID)
	env = setEnv(env, "HERDR_TAB_ID", workspace.RootPane.TabID)
	return &acknowledgedNoticeFixture{root: root, repo: repo, home: home, binary: binary, destination: "opencode:ack-session", env: env, db: db, project: project, noticeID: noticeID}
}

func (f *acknowledgedNoticeFixture) cli(t *testing.T, args ...string) (int, []byte) {
	t.Helper()
	command := exec.Command(f.binary, args...)
	command.Dir, command.Env = f.repo, f.env
	output, err := command.CombinedOutput()
	if err == nil {
		return 0, output
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return exit.ExitCode(), output
}

func (f *acknowledgedNoticeFixture) crashAfterAck(t *testing.T) noticeHandoffPayload {
	t.Helper()
	consumer := filepath.Join(f.root, "ack-consumer.sh")
	batchPath := filepath.Join(f.root, "ack-batch.json")
	script := `#!/bin/sh
set -eu
"$NOTICE_ACK_BIN" lookout --json --handoff --destination "$NOTICE_ACK_DESTINATION" > "$NOTICE_ACK_BATCH"
"$NOTICE_ACK_BIN" ack "$NOTICE_ACK_ID" --json
kill -KILL $$
`
	if err := os.WriteFile(consumer, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	env := setEnv(f.env, "NOTICE_ACK_BIN", f.binary)
	env = setEnv(env, "NOTICE_ACK_DESTINATION", f.destination)
	env = setEnv(env, "NOTICE_ACK_BATCH", batchPath)
	env = setEnv(env, "NOTICE_ACK_ID", strconv.FormatInt(f.noticeID, 10))
	command := exec.Command(consumer)
	command.Dir, command.Env = f.repo, env
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != -1 {
		t.Fatalf("receiver did not crash after handling ack: %v output=%s", err, output)
	}
	var batch noticeHandoffPayload
	data, err := os.ReadFile(batchPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &batch); err != nil {
		t.Fatalf("decode printed receipt: %s: %v", data, err)
	}
	if batch.Delivery.State != "printed" || len(batch.Notices) != 1 || batch.Notices[0].ID != f.noticeID {
		t.Fatalf("crashed receiver batch = %+v", batch)
	}
	notices, err := f.db.NoticesByIDs(context.Background(), f.project.ID, []int64{f.noticeID})
	if err != nil || notices[0].DeliveredAt != 0 || notices[0].AckedAt == 0 {
		t.Fatalf("handled ack before crash = %+v err=%v", notices, err)
	}
	return batch
}

func TestAcknowledgedPrintedNoticeRecoversAfterCrashAndRebuild(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		name := "crash"
		if rebuild {
			name = "rebuild"
		}
		t.Run(name, func(t *testing.T) {
			f := newAcknowledgedNoticeFixture(t)
			first := f.crashAfterAck(t)
			if poller, err := f.db.LookoutPoller(context.Background(), f.project.ID); err != nil || poller.PID != 0 {
				t.Fatalf("Notice acknowledgement unexpectedly started a Project poller: %+v %v", poller, err)
			}
			if rebuild {
				if _, err := f.db.RebuildFromSnapshots(context.Background(), f.home); err != nil {
					t.Fatalf("rebuild after handled ack: %v", err)
				}
				var restoredOwner, restoredClaim string
				var ackedAt int64
				if err := f.db.QueryRowContext(context.Background(), `SELECT d.owner_token,n.claim_token,n.acked_at FROM notice_delivery_receipts d JOIN notices n ON n.id=? WHERE d.delivery_id=?`, f.noticeID, first.Delivery.DeliveryID).Scan(&restoredOwner, &restoredClaim, &ackedAt); err != nil {
					t.Fatal(err)
				}
				if restoredOwner != first.Delivery.OwnerToken || restoredClaim != restoredOwner || ackedAt == 0 {
					t.Fatalf("rebuild lost printed receipt ownership: owner=%q claim=%q acked_at=%d", restoredOwner, restoredClaim, ackedAt)
				}
			}
			newerID, err := f.db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, Kind: "needs_decision", Summary: "Later Notice must still reach the adapter"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.ExecContext(context.Background(), `UPDATE notice_delivery_receipts SET lease_until=0 WHERE delivery_id=?`, first.Delivery.DeliveryID); err != nil {
				t.Fatal(err)
			}
			code, output := f.cli(t, "lookout", "--json", "--handoff", "--destination", f.destination, "--timeout", "2000")
			if code != 0 {
				t.Fatalf("same-session receipt recovery after ack: %s", output)
			}
			var retry noticeHandoffPayload
			if err := json.Unmarshal(output, &retry); err != nil {
				t.Fatalf("decode recovered receipt: %s: %v", output, err)
			}
			if retry.Delivery.DeliveryID != first.Delivery.DeliveryID || retry.Delivery.BatchID != first.Delivery.BatchID || retry.Delivery.OwnerToken == first.Delivery.OwnerToken || len(retry.Notices) != 1 || retry.Notices[0].ID != f.noticeID {
				t.Fatalf("recovery changed or merged the acknowledged batch: first=%+v retry=%+v", first, retry)
			}
			code, output = f.cli(t, "lookout", "--json", "--receipt", retry.Delivery.DeliveryID, "--receipt-outcome", "accepted", "--receipt-token", retry.Delivery.OwnerToken)
			if code != 0 {
				t.Fatalf("accept recovered receipt: %s", output)
			}
			notices, err := f.db.NoticesByIDs(context.Background(), f.project.ID, []int64{f.noticeID})
			if err != nil || notices[0].DeliveredAt == 0 || notices[0].AckedAt == 0 {
				t.Fatalf("receipt recovery lost handled acknowledgment: %+v err=%v", notices, err)
			}
			nextCode, nextOutput := f.cli(t, "lookout", "--json", "--handoff", "--destination", f.destination, "--timeout", "2000")
			if nextCode != 0 {
				t.Fatalf("later Notice did not reach receiver: %s", nextOutput)
			}
			var next noticeHandoffPayload
			if err := json.Unmarshal(nextOutput, &next); err != nil {
				t.Fatalf("decode later batch: %s: %v", nextOutput, err)
			}
			if len(next.Notices) != 1 || next.Notices[0].ID != newerID || next.Delivery.BatchID == first.Delivery.BatchID {
				t.Fatalf("later Notice was lost or merged into the old batch: %+v", next)
			}
		})
	}
}

func TestOpenCodeQueuesNoticeBatchDuringBusyTurn(t *testing.T) {
	f := newAcknowledgedNoticeFixture(t)
	leadFile := filepath.Join(f.root, "lead.md")
	if err := os.WriteFile(leadFile, []byte("Fixture Lead"), 0o600); err != nil {
		t.Fatal(err)
	}
	releasePrompt := filepath.Join(f.root, "release-prompt")
	reloadPlugin := filepath.Join(f.root, "reload-plugin")
	resumedPlugin := filepath.Join(f.root, "resumed-plugin.json")
	statusFile := filepath.Join(f.root, "adapter-status.json")
	env := setEnv(f.env, "OPENCODE_BUSY_SOURCE", moduleRoot(t))
	env = setEnv(env, "OPENCODE_BUSY_BIN", f.binary)
	env = setEnv(env, "OPENCODE_BUSY_LEAD_FILE", leadFile)
	env = setEnv(env, "OPENCODE_BUSY_RELEASE", releasePrompt)
	env = setEnv(env, "OPENCODE_BUSY_RELOAD", reloadPlugin)
	env = setEnv(env, "OPENCODE_BUSY_RESUMED", resumedPlugin)
	env = setEnv(env, "OPENCODE_BUSY_STATUS", statusFile)
	command := exec.Command("node", filepath.Join(moduleRoot(t), "internal/e2e/testdata/opencode_notice_busy_batch.mjs"))
	command.Dir, command.Env = f.repo, env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = command.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = command.Process.Kill()
			<-done
		}
	})

	waitUntil := func(description string, condition func() (bool, error)) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			ready, err := condition()
			if err != nil {
				t.Fatalf("%s: %v", description, err)
			}
			if ready {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", description)
	}
	waitUntil("the first OpenCode prompt to remain busy", func() (bool, error) {
		contents, err := os.ReadFile(statusFile)
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
		return strings.Contains(string(contents), `"promptCalls":1`) && strings.Contains(string(contents), `"busy":true`), nil
	})
	secondNoticeID, err := f.db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, Kind: "needs_decision", Summary: "Observed while OpenCode is busy"})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil("the second receipt to be printed before the first prompt completes", func() (bool, error) {
		var state string
		err := f.db.QueryRowContext(context.Background(), `SELECT COALESCE((SELECT r.state FROM notice_delivery_receipts r, json_each(r.notice_ids_json) n WHERE r.project_id=? AND CAST(n.value AS INTEGER)=? ORDER BY r.created_at DESC LIMIT 1), '')`, f.project.ID, secondNoticeID).Scan(&state)
		return state == "printed", err
	})
	thirdNoticeID, err := f.db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, Kind: "task_done", Summary: "Keep this batch outside the bounded queue"})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var receiptState, claimToken string
		if err := f.db.QueryRowContext(context.Background(), `SELECT COALESCE((SELECT r.state FROM notice_delivery_receipts r, json_each(r.notice_ids_json) n WHERE r.project_id=? AND CAST(n.value AS INTEGER)=? ORDER BY r.created_at DESC LIMIT 1), ''), (SELECT claim_token FROM notices WHERE id=?)`, f.project.ID, thirdNoticeID, thirdNoticeID).Scan(&receiptState, &claimToken); err != nil {
			t.Fatal(err)
		}
		if receiptState != "" || claimToken != "" {
			t.Fatalf("third Notice exceeded the bounded in-memory queue: receipt=%q claim=%q", receiptState, claimToken)
		}
	}
	contents, err := os.ReadFile(statusFile)
	if err != nil || !strings.Contains(string(contents), `"promptCalls":1`) || !strings.Contains(string(contents), `"busy":true`) {
		t.Fatalf("later Notice was not collected during the still-busy turn: status=%s err=%v", contents, err)
	}
	if err := os.WriteFile(reloadPlugin, []byte("reload"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitUntil("the replacement plugin to retire the old plugin's processes", func() (bool, error) {
		contents, err := os.ReadFile(resumedPlugin)
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
		return strings.Contains(string(contents), `"retiredChildren":true`), nil
	})
	if _, err := f.db.ExecContext(context.Background(), `UPDATE notice_delivery_receipts SET lease_until=0 WHERE project_id=? AND destination=? AND state IN ('claimed','printed')`, f.project.ID, "opencode:busy-session"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releasePrompt, []byte("continue"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if waitErr != nil {
			t.Fatalf("OpenCode busy-batch adapter: %v stdout=%s stderr=%s", waitErr, stdout.String(), stderr.String())
		}
	case <-time.After(15 * time.Second):
		_ = command.Process.Kill()
		<-done
		t.Fatalf("OpenCode busy-batch adapter did not finish: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	var result struct {
		PromptCalls        int      `json:"promptCalls"`
		SettlementCodes    []int    `json:"settlementCodes"`
		SettlementAttempts []string `json:"settlementAttempts"`
		WatchFailures      []int    `json:"watchFailures"`
		RetiredChildren    bool     `json:"retiredChildren"`
		ShutdownHadLookout bool     `json:"shutdownHadLookout"`
		AllChildrenRetired bool     `json:"allChildrenRetired"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode busy-batch adapter output %q: %v", stdout.String(), err)
	}
	if result.PromptCalls != 3 || len(result.SettlementCodes) < 3 || len(result.SettlementAttempts) != 3 || len(result.WatchFailures) != 0 || !result.RetiredChildren || !result.ShutdownHadLookout || !result.AllChildrenRetired {
		t.Fatalf("OpenCode busy-batch result = %+v stderr=%s", result, stderr.String())
	}
	for _, outcome := range result.SettlementAttempts {
		if outcome != "accepted" {
			t.Fatalf("OpenCode busy-batch receipt outcome = %q, want accepted; result=%+v", outcome, result)
		}
	}
	for _, id := range []int64{f.noticeID, secondNoticeID, thirdNoticeID} {
		notices, err := f.db.NoticesByIDs(context.Background(), f.project.ID, []int64{id})
		if err != nil || len(notices) != 1 || notices[0].DeliveredAt == 0 {
			t.Fatalf("OpenCode did not accept Notice %d: notices=%+v err=%v", id, notices, err)
		}
	}
}

func TestOpenCodePromptOutcomesFollowReceiptEvidence(t *testing.T) {
	f := newAcknowledgedNoticeFixture(t)
	leadFile := filepath.Join(f.root, "lead.md")
	if err := os.WriteFile(leadFile, []byte("Fixture Lead"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		mode         string
		wantState    string
		wantPrompts  int
		wantOutcomes []string
	}{
		{mode: "rejected", wantState: "accepted", wantPrompts: 2, wantOutcomes: []string{"rejected", "accepted"}},
		{mode: "accepted-error", wantState: "accepted", wantPrompts: 1, wantOutcomes: []string{"accepted"}},
		{mode: "ambiguous", wantState: "uncertain", wantPrompts: 1, wantOutcomes: []string{"uncertain"}},
	} {
		t.Run(testCase.mode, func(t *testing.T) {
			noticeID, err := f.db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, Kind: "task_done", Summary: "OpenCode outcome " + testCase.mode})
			if err != nil {
				t.Fatal(err)
			}
			env := setEnv(f.env, "OPENCODE_OUTCOME_SOURCE", moduleRoot(t))
			env = setEnv(env, "OPENCODE_OUTCOME_BIN", f.binary)
			env = setEnv(env, "OPENCODE_OUTCOME_LEAD_FILE", leadFile)
			env = setEnv(env, "OPENCODE_OUTCOME_MODE", testCase.mode)
			command := exec.Command("node", filepath.Join(moduleRoot(t), "internal/e2e/testdata/opencode_notice_prompt_outcomes.mjs"))
			command.Dir, command.Env = f.repo, env
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			if err := command.Run(); err != nil {
				t.Fatalf("OpenCode %s adapter: %v stdout=%s stderr=%s", testCase.mode, err, stdout.String(), stderr.String())
			}
			var result struct {
				PromptCalls int `json:"promptCalls"`
				Settlements []struct {
					Outcome string `json:"outcome"`
					Code    int    `json:"code"`
				} `json:"settlements"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("decode %s adapter output %q: %v", testCase.mode, stdout.String(), err)
			}
			if result.PromptCalls != testCase.wantPrompts {
				t.Fatalf("%s prompt calls = %d, want %d; result=%s stderr=%s", testCase.mode, result.PromptCalls, testCase.wantPrompts, stdout.String(), stderr.String())
			}
			var outcomes []string
			for _, settlement := range result.Settlements {
				if settlement.Code == 0 {
					outcomes = append(outcomes, settlement.Outcome)
				}
			}
			if strings.Join(outcomes, ",") != strings.Join(testCase.wantOutcomes, ",") {
				t.Fatalf("%s receipt outcomes = %v, want %v; stderr=%s", testCase.mode, outcomes, testCase.wantOutcomes, stderr.String())
			}
			var state string
			if err := f.db.QueryRowContext(context.Background(), `SELECT r.state FROM notice_delivery_receipts r, json_each(r.notice_ids_json) n WHERE r.project_id=? AND CAST(n.value AS INTEGER)=?`, f.project.ID, noticeID).Scan(&state); err != nil || state != testCase.wantState {
				t.Fatalf("%s receipt state = %q, %v; want %q", testCase.mode, state, err, testCase.wantState)
			}
		})
	}
}

func TestOpenCodeAcknowledgedReceiptRecoversAfterLeaseExpiry(t *testing.T) {
	f := newAcknowledgedNoticeFixture(t)
	leadFile := filepath.Join(f.root, "lead.md")
	if err := os.WriteFile(leadFile, []byte("Fixture Lead"), 0o600); err != nil {
		t.Fatal(err)
	}
	releasePrompt := filepath.Join(f.root, "release-prompt")
	env := setEnv(f.env, "NOTICE_ACK_SOURCE", moduleRoot(t))
	env = setEnv(env, "NOTICE_ACK_BIN", f.binary)
	env = setEnv(env, "NOTICE_ACK_ID", strconv.FormatInt(f.noticeID, 10))
	env = setEnv(env, "NOTICE_ACK_LEAD_FILE", leadFile)
	env = setEnv(env, "NOTICE_ACK_RELEASE", releasePrompt)
	command := exec.Command("node", filepath.Join(moduleRoot(t), "internal/e2e/testdata/opencode_notice_ack_expiry.mjs"))
	command.Dir, command.Env = f.repo, env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill() }()
	done := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		notices, err := f.db.NoticesByIDs(context.Background(), f.project.ID, []int64{f.noticeID})
		if err == nil && notices[0].AckedAt != 0 {
			done = true
			break
		}
	}
	if !done {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("OpenCode did not acknowledge its in-turn Notice: %s%s", stdout.String(), stderr.String())
	}
	if _, err := f.db.ExecContext(context.Background(), `UPDATE notice_delivery_receipts SET lease_until=0 WHERE project_id=?`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	newerID, err := f.db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, Kind: "needs_decision", Summary: "Later Notice must reach OpenCode"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releasePrompt, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("OpenCode receipt recovery probe: %v output=%s%s", err, stdout.String(), stderr.String())
	}
	output := stdout.String() + stderr.String()
	var result struct {
		PromptCalls     int   `json:"promptCalls"`
		SettlementCodes []int `json:"settlementCodes"`
		WatchFailures   []int `json:"watchFailures"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode adapter probe: %s: %v", output, err)
	}
	if result.PromptCalls != 2 || len(result.SettlementCodes) < 3 || len(result.WatchFailures) != 0 {
		t.Fatalf("expired handled receipt was replayed or stopped later delivery: %+v", result)
	}
	first, err := f.db.NoticesByIDs(context.Background(), f.project.ID, []int64{f.noticeID})
	if err != nil || first[0].DeliveredAt == 0 || first[0].AckedAt == 0 {
		t.Fatalf("OpenCode receipt recovery lost handled acknowledgment: %+v err=%v", first, err)
	}
	later, err := f.db.NoticesByIDs(context.Background(), f.project.ID, []int64{newerID})
	if err != nil || len(later) != 1 || later[0].DeliveredAt == 0 {
		t.Fatalf("later Notice did not reach OpenCode after recovery: %+v err=%v", later, err)
	}
}
