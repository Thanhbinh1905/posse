//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestNoticeReceiptsSurviveProjectMoveAndRebuild(t *testing.T) {
	f := newAcknowledgedNoticeFixture(t)
	ctx := context.Background()
	taskID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{
		Seq: 1, Type: "scout", Title: "Move recovery snapshot", LandingMode: "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := deliverAndAcceptNotice(t, f)

	movedRoot := filepath.Join(f.root, "moved-repo")
	if err := os.Rename(f.repo, movedRoot); err != nil {
		t.Fatal(err)
	}
	f.repo = movedRoot
	code, output := f.cli(t, "project", "move", "shop", movedRoot)
	if code != 0 {
		t.Fatalf("move Project: %s", output)
	}

	// Project snapshot refresh is handled by A24. Materialize that expected
	// post-move Task snapshot without changing the receipt snapshot.
	if err := f.db.PersistTask(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	code, output = f.cliAsUser(t, "recover", "--rebuild")
	if code != 0 {
		t.Fatalf("recover after Project move: %s", output)
	}
	restored, err := f.db.NoticeDelivery(ctx, first.Delivery.DeliveryID)
	if err != nil || restored.State != "accepted" {
		t.Fatalf("receipt %s after Project move rebuild = %+v, %v", first.Delivery.DeliveryID, restored, err)
	}
	notices, err := f.db.NoticesByIDs(ctx, f.project.ID, first.Delivery.NoticeIDs)
	if err != nil || len(notices) != 1 || notices[0].DeliveredAt == 0 {
		t.Fatalf("delivered Notice after Project move rebuild = %+v, %v", notices, err)
	}
	moved, err := f.db.ProjectByID(ctx, f.project.ID)
	if err != nil || moved.Root != movedRoot {
		t.Fatalf("Project root after move rebuild = %q, want %q: %v", moved.Root, movedRoot, err)
	}
}

func (f *acknowledgedNoticeFixture) cliAsUser(t *testing.T, args ...string) (int, []byte) {
	t.Helper()
	env := herdr.IsolatedTestEnvironment(f.root)
	env = setEnv(env, "PATH", filepath.Dir(f.binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	command := exec.Command(f.binary, args...)
	command.Dir, command.Env = f.repo, env
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

func TestNoticeReceiptRebuildAcceptsRenamedProjectSnapshotIdentity(t *testing.T) {
	f := newAcknowledgedNoticeFixture(t)
	ctx := context.Background()
	taskID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{
		Seq: 1, Type: "scout", Title: "Rename recovery snapshot", LandingMode: "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	batch := deliverAndAcceptNotice(t, f)

	oldTaskDir := filepath.Dir(f.db.TaskSnapshotPath(f.project.Name, 1))
	newName := "market"
	newTaskDir := filepath.Dir(f.db.TaskSnapshotPath(newName, 1))
	if err := os.MkdirAll(filepath.Dir(newTaskDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldTaskDir, newTaskDir); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE projects SET name=? WHERE id=?`, newName, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.PersistTask(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	oldSnapshot := f.db.NoticeDeliverySnapshotPath(f.project.Name)
	data, err := os.ReadFile(oldSnapshot)
	if err != nil {
		t.Fatalf("renamed Project's original receipt snapshot disappeared: %v", err)
	}
	newSnapshot := f.db.NoticeDeliverySnapshotPath(newName)
	if err := os.MkdirAll(filepath.Dir(newSnapshot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newSnapshot, data, 0o600); err != nil {
		t.Fatal(err)
	}

	code, output := f.cliAsUser(t, "recover", "--rebuild")
	if code != 0 {
		t.Fatalf("recover after Project rename: %s", output)
	}
	restored, err := f.db.NoticeDelivery(ctx, batch.Delivery.DeliveryID)
	if err != nil || restored.State != "accepted" {
		t.Fatalf("receipt after renamed Project rebuild = %+v, %v", restored, err)
	}
}

func deliverAndAcceptNotice(t *testing.T, f *acknowledgedNoticeFixture) noticeHandoffPayload {
	t.Helper()
	code, output := f.cli(t, "lookout", "--json", "--handoff", "--destination", f.destination, "--timeout", "2000")
	if code != 0 {
		t.Fatalf("print Notice handoff: %s", output)
	}
	var batch noticeHandoffPayload
	if err := json.Unmarshal(output, &batch); err != nil {
		t.Fatalf("decode Notice handoff %s: %v", output, err)
	}
	code, output = f.cli(t, "lookout", "--json", "--receipt", batch.Delivery.DeliveryID, "--receipt-outcome", "accepted", "--receipt-token", batch.Delivery.OwnerToken)
	if code != 0 {
		t.Fatalf("accept Notice handoff: %s", output)
	}
	return batch
}
