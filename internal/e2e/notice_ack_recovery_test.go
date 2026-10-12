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
	root := newFixtureRoot(t, fixturePrefix("notice-ack-recovery-"))
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
