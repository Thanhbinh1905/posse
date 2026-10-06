//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestNoticeHandoffSurvivesHarnessCrashAndAvoidsRecordedReplay(t *testing.T) {
	root := newFixtureRoot(t, fixturePrefix("notice-handoff-"))
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
	env = setEnv(env, "POSSE_E2E_RECEIVED", filepath.Join(root, "harness-received.json"))
	env = setEnv(env, "POSSE_E2E_INBOX", filepath.Join(root, "harness-inbox.txt"))
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

	db, err := store.Open(filepath.Join(root, "posse"))
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
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	noticeID, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "Worker finished"})
	if err != nil {
		t.Fatal(err)
	}

	// The fake harness persists a same-session receipt before applying a forge
	// effect. It crashes before injection, after its effect but before Posse
	// records acceptance, and after acceptance but before user acknowledgment.
	receiver := filepath.Join(root, "fake-harness")
	script := `#!/bin/sh
set -eu
if [ "$POSSE_E2E_PHASE" = replay ]; then
  "$POSSE_E2E_POSSE_BIN" lookout --json --quiet-routine --handoff --destination fake:session-1 --timeout 1 > "$POSSE_E2E_RECEIVED" 2>"$POSSE_TEST_ROOT/harness-error.log"
  exit 0
fi
"$POSSE_E2E_POSSE_BIN" lookout --json --quiet-routine --handoff --destination fake:session-1 > "$POSSE_E2E_RECEIVED" 2>"$POSSE_TEST_ROOT/harness-error.log"
ID=$(sed -n 's/.*"delivery_id":"\([^"]*\)".*/\1/p' "$POSSE_E2E_RECEIVED")
TOKEN=$(sed -n 's/.*"owner_token":"\([^"]*\)".*/\1/p' "$POSSE_E2E_RECEIVED")
test -n "$ID"
case "$POSSE_E2E_PHASE" in
  crash-before) exit 86 ;;
  crash-after)
    if ! grep -Fxq "$ID" "$POSSE_E2E_INBOX" 2>/dev/null; then
      printf '%s\n' "$ID" >> "$POSSE_E2E_INBOX"
      printf 'forge-effect\n' >> "$POSSE_TEST_ROOT/forge-effects.log"
    fi
    exit 87
    ;;
  recover)
    if ! grep -Fxq "$ID" "$POSSE_E2E_INBOX" 2>/dev/null; then
      printf '%s\n' "$ID" >> "$POSSE_E2E_INBOX"
      printf 'forge-effect\n' >> "$POSSE_TEST_ROOT/forge-effects.log"
    fi
    "$POSSE_E2E_POSSE_BIN" lookout --json --receipt "$ID" --receipt-outcome accepted --receipt-token "$TOKEN" >/dev/null
    exit 89
    ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(receiver, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	harnessEnv := setEnv(env, "HERDR_ENV", "1")
	harnessEnv = setEnv(harnessEnv, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	harnessEnv = setEnv(harnessEnv, "HERDR_WORKSPACE_ID", workspace.Workspace.WorkspaceID)
	harnessEnv = setEnv(harnessEnv, "HERDR_TAB_ID", workspace.RootPane.TabID)

	runHarness := func(phase string, expectedExit int) noticeHandoffPayload {
		t.Helper()
		phaseEnv := setEnv(harnessEnv, "POSSE_E2E_PHASE", phase)
		command := exec.Command(receiver)
		command.Dir = repo
		command.Env = phaseEnv
		output, err := command.CombinedOutput()
		if expectedExit == 0 {
			if err != nil {
				t.Fatalf("fake harness %s: %v output=%s", phase, err, output)
			}
		} else {
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != expectedExit {
				t.Fatalf("fake harness %s exit = %v output=%s, want %d", phase, err, output, expectedExit)
			}
		}
		data, err := os.ReadFile(filepath.Join(root, "harness-received.json"))
		if err != nil {
			t.Fatal(err)
		}
		var payload noticeHandoffPayload
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatalf("decode %s receipt: %s: %v", phase, data, err)
		}
		return payload
	}

	first := runHarness("crash-before", 86)
	if len(first.Notices) != 1 || first.Notices[0].ID != noticeID || first.Delivery.State != "printed" {
		t.Fatalf("first printed batch = %#v", first)
	}
	stored, err := db.NoticeDelivery(ctx, first.Delivery.DeliveryID)
	if err != nil || stored.State != "printed" || stored.OwnerToken != first.Delivery.OwnerToken {
		t.Fatalf("receipt was not durable before print: %#v, %v", stored, err)
	}
	pending, err := db.UndeliveredNotices(ctx, project.ID)
	if err != nil || len(pending) != 0 {
		t.Fatalf("printed handoff should be held for its receipt: %#v, %v", pending, err)
	}

	if _, err := db.ExecContext(ctx, `UPDATE notice_delivery_receipts SET lease_until=0 WHERE delivery_id=?`, first.Delivery.DeliveryID); err != nil {
		t.Fatal(err)
	}
	second := runHarness("crash-after", 87)
	if second.Delivery.DeliveryID != first.Delivery.DeliveryID || second.Delivery.BatchID != first.Delivery.BatchID || second.Delivery.OwnerToken == first.Delivery.OwnerToken || len(second.Notices) != 1 || second.Notices[0].ID != noticeID {
		t.Fatalf("recovered batch did not retain its stable identity and exact IDs: first=%#v second=%#v", first, second)
	}
	if _, err := db.ExecContext(ctx, `UPDATE notice_delivery_receipts SET lease_until=0 WHERE delivery_id=?`, first.Delivery.DeliveryID); err != nil {
		t.Fatal(err)
	}
	third := runHarness("recover", 89)
	if third.Delivery.DeliveryID != first.Delivery.DeliveryID || third.Delivery.BatchID != first.Delivery.BatchID {
		t.Fatalf("same-session receipt changed across replay: first=%#v third=%#v", first, third)
	}
	replay := runHarness("replay", 0)
	if replay.State != "timeout" || len(replay.Notices) != 0 {
		t.Fatalf("accepted batch replay result = %#v", replay)
	}
	effects, err := os.ReadFile(filepath.Join(root, "forge-effects.log"))
	if err != nil || strings.Count(string(effects), "forge-effect") != 1 {
		t.Fatalf("recorded forge effect repeated: effects=%q err=%v", effects, err)
	}
	final, err := db.NoticeDelivery(ctx, first.Delivery.DeliveryID)
	if err != nil || final.State != "accepted" {
		t.Fatalf("delivery receipt after recovery = %#v, %v", final, err)
	}
	pending, err = db.UndeliveredNotices(ctx, project.ID)
	if err != nil || len(pending) != 0 {
		t.Fatalf("accepted Notice remained pending: %#v, %v", pending, err)
	}
	if delivered, err := db.NoticesByIDs(ctx, project.ID, []int64{noticeID}); err != nil || delivered[0].DeliveredAt == 0 || delivered[0].AckedAt != 0 {
		t.Fatalf("accepted Notice delivery before acknowledgment = %#v, %v", delivered, err)
	}
}

type noticeHandoffPayload struct {
	State   string `json:"state"`
	Notices []struct {
		ID int64 `json:"id"`
	} `json:"notices"`
	Delivery struct {
		DeliveryID string  `json:"delivery_id"`
		BatchID    string  `json:"batch_id"`
		NoticeIDs  []int64 `json:"notice_ids"`
		State      string  `json:"state"`
		OwnerToken string  `json:"owner_token"`
	} `json:"delivery"`
}
