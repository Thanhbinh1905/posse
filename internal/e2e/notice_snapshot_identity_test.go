//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestNoticeReceiptsSurviveProjectMoveAndRebuild(t *testing.T) {
	f := newAcknowledgedNoticeFixture(t)
	ctx := context.Background()
	if _, err := f.db.CreateTask(ctx, f.project.ID, store.Task{
		Seq: 1, Type: "scout", Title: "Move recovery snapshot", LandingMode: "local",
	}); err != nil {
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

func TestNoticeReceiptRebuildRejectsReusedProjectID(t *testing.T) {
	f := newAcknowledgedNoticeFixture(t)
	ctx := context.Background()
	oldProject := f.project
	batch := deliverAndAcceptNotice(t, f)
	if err := os.Remove(f.db.ProjectSnapshotPath(oldProject.Name)); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(f.home, "posse.db") + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	otherRoot := filepath.Join(f.root, "other-repo")
	if err := os.MkdirAll(otherRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	f.db = db
	t.Cleanup(func() { _ = db.Close() })
	other, err := db.CreateProject(ctx, "other", otherRoot, "main")
	if err != nil {
		t.Fatal(err)
	}
	if other.ID != oldProject.ID || other.UUID == oldProject.UUID {
		t.Fatalf("fixture did not reuse the numeric ID with a new UUID: old=(%d,%q) new=(%d,%q)", oldProject.ID, oldProject.UUID, other.ID, other.UUID)
	}

	code, output := f.cliAsUser(t, "recover", "--rebuild")
	if code == 0 || !strings.Contains(string(output), "Notice delivery snapshot Project UUID") || !strings.Contains(string(output), oldProject.UUID) {
		t.Fatalf("rebuild did not refuse the stale receipt UUID: code=%d output=%s", code, output)
	}
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 || projects[0].ID != other.ID || projects[0].UUID != other.UUID {
		t.Fatalf("failed rebuild changed the replacement Project: %+v, %v", projects, err)
	}
	if _, err := db.NoticeDelivery(ctx, batch.Delivery.DeliveryID); !store.IsNotFound(err) {
		t.Fatalf("old receipt was attached to the replacement Project: %v", err)
	}
	notices, err := db.Notices(ctx, other.ID, false)
	if err != nil || len(notices) != 0 {
		t.Fatalf("old Notices were attached to the replacement Project: %+v, %v", notices, err)
	}
}

func TestAcceptedNoticeReceiptSurvivesColdRebuild(t *testing.T) {
	f := newAcknowledgedNoticeFixture(t)
	ctx := context.Background()
	acknowledgedID, err := f.db.CreateNotice(ctx, store.Notice{ProjectID: f.project.ID, Kind: "needs_decision", Summary: "Acknowledgment must survive cold rebuild"})
	if err != nil {
		t.Fatal(err)
	}
	code, output := f.cli(t, "ack", strconv.FormatInt(acknowledgedID, 10), "--json")
	if code != 0 {
		t.Fatalf("acknowledge Notice before cold rebuild: %s", output)
	}
	acknowledgedBefore, err := f.db.NoticesByIDs(ctx, f.project.ID, []int64{acknowledgedID})
	if err != nil || len(acknowledgedBefore) != 1 || acknowledgedBefore[0].AckedAt == 0 {
		t.Fatalf("acknowledged Notice before cold rebuild = %+v, %v", acknowledgedBefore, err)
	}
	accepted := deliverAndAcceptNotice(t, f)
	before, err := f.db.NoticesByIDs(ctx, f.project.ID, accepted.Delivery.NoticeIDs)
	if err != nil || len(before) != 1 || before[0].DeliveredAt == 0 {
		t.Fatalf("accepted Notice before cold rebuild = %+v, %v", before, err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(f.home, "posse.db") + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	code, output = f.cliAsUser(t, "recover", "--rebuild", "--json")
	if code != 0 {
		t.Fatalf("recover after database loss: %s", output)
	}
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	f.db = db
	t.Cleanup(func() { _ = db.Close() })
	restoredReceipt, err := db.NoticeDelivery(ctx, accepted.Delivery.DeliveryID)
	if err != nil || restoredReceipt.State != "accepted" {
		t.Fatalf("accepted receipt after cold rebuild = %+v, %v", restoredReceipt, err)
	}
	acknowledgedAfter, err := db.NoticesByIDs(ctx, f.project.ID, []int64{acknowledgedID})
	if err != nil || len(acknowledgedAfter) != 1 || acknowledgedAfter[0].AckedAt != acknowledgedBefore[0].AckedAt {
		t.Fatalf("acknowledged Notice after cold rebuild = %+v, %v; before=%+v", acknowledgedAfter, err, acknowledgedBefore)
	}
	after, err := db.NoticesByIDs(ctx, f.project.ID, accepted.Delivery.NoticeIDs)
	if err != nil || len(after) != 1 {
		t.Fatalf("accepted Notice after cold rebuild = %+v, %v", after, err)
	}
	if after[0].DeliveredAt != before[0].DeliveredAt {
		t.Errorf("accepted Notice after cold rebuild = %+v; before=%+v", after, before)
	}
	newerID, err := db.CreateNotice(ctx, store.Notice{ProjectID: f.project.ID, Kind: "needs_decision", Summary: "Next Notice after recovery"})
	if err != nil {
		t.Fatal(err)
	}
	code, output = f.cli(t, "lookout", "--json", "--handoff", "--destination", f.destination, "--timeout", "2000")
	if code != 0 {
		t.Fatalf("handoff after cold rebuild: %s", output)
	}
	var next noticeHandoffPayload
	if err := json.Unmarshal(output, &next); err != nil {
		t.Fatalf("decode post-rebuild handoff %s: %v", output, err)
	}
	if len(next.Notices) != 1 || next.Notices[0].ID != newerID || next.Delivery.DeliveryID == accepted.Delivery.DeliveryID {
		t.Fatalf("accepted Notice was replayed in a new batch: accepted=%+v next=%+v", accepted, next)
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
