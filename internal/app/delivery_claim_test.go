package app

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
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type promptGateAdapter struct {
	herdr.Adapter
	entered chan struct{}
	release chan struct{}
}

func exitMessageCrashHelper(code int) {
	for _, name := range []string{"POSSE_HOME", "CODEX_HOME"} {
		path := os.Getenv(name)
		root := filepath.Dir(path)
		if filepath.Dir(root) == os.TempDir() && strings.HasPrefix(filepath.Base(root), "posse-app-test-run-") {
			_ = removeAppTestRun(root)
		}
	}
	os.Exit(code)
}

type acceptedPromptLostAckAdapter struct {
	herdr.Adapter
}

func (a acceptedPromptLostAckAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	result, err := a.Adapter.Call(ctx, method, params)
	if method == "agent.prompt" && err == nil {
		return nil, errors.New("simulated lost acknowledgment after Herdr accepted the prompt")
	}
	return result, err
}

type crashAfterAcceptedPromptAdapter struct {
	*herdr.Fake
	marker string
}

func (a crashAfterAcceptedPromptAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	result, err := a.Fake.Call(ctx, method, params)
	if method == "agent.prompt" && err == nil {
		if err := os.WriteFile(a.marker, []byte("accepted\n"), 0o600); err != nil {
			exitMessageCrashHelper(85)
		}
		exitMessageCrashHelper(86)
	}
	return result, err
}

func TestMessageDeliveryCrashHelperProcess(t *testing.T) {
	if os.Getenv("POSSE_MESSAGE_CRASH_HELPER") != "1" {
		return
	}
	ctx := context.Background()
	db, err := store.Open(os.Getenv("POSSE_MESSAGE_CRASH_HOME"))
	if err != nil {
		exitMessageCrashHelper(2)
	}
	projectID, _ := strconv.ParseInt(os.Getenv("POSSE_MESSAGE_CRASH_PROJECT"), 10, 64)
	project, err := db.ProjectByID(ctx, projectID)
	if err != nil {
		exitMessageCrashHelper(3)
	}
	task, err := db.Task(ctx, projectID, "t1")
	if err != nil {
		exitMessageCrashHelper(4)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "working"}}}
	service := testService(os.Getenv("POSSE_MESSAGE_CRASH_HOME"), crashAfterAcceptedPromptAdapter{Fake: fake, marker: os.Getenv("POSSE_MESSAGE_CRASH_MARKER")})
	if err := service.deliverQueuedMessages(ctx, db, project, fake.SnapshotValue); err != nil {
		exitMessageCrashHelper(5)
	}
	exitMessageCrashHelper(0)
}

func TestConcurrentMessageDeliveryClaimsBeforePrompt(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Fix", LandingMode: "local", PaneID: "w2:p1", PaneLabel: "posse:shop:t1", HerdrWorkspaceID: "w2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	messageID, err := db.QueueMessage(ctx, taskID, "Please check the tests", false)
	if err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{FocusedPaneID: "w1:p1", Panes: []herdr.Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude", AgentStatus: "working", Focused: true},
		{PaneID: "w2:p1", WorkspaceID: "w2", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "idle"},
	}}
	adapter := &promptGateAdapter{Adapter: fake, entered: make(chan struct{}, 2), release: make(chan struct{})}
	service := testService(home, adapter)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- service.deliverQueuedMessages(ctx, db, project, fake.SnapshotValue)
		}()
	}
	close(start)
	select {
	case <-adapter.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("no queued message prompt started")
	}
	select {
	case err := <-results:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("second message delivery did not return while the first prompt was blocked")
	}
	close(adapter.release)
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("agent.prompt") != 1 {
		t.Fatalf("concurrent message delivery prompted more than once: calls=%#v", fake.Calls)
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM messages WHERE id=?`, messageID).Scan(&status); err != nil || status != "delivered" {
		t.Fatalf("message state = %q, %v", status, err)
	}
}

func TestMessageDeliveryDoesNotDuplicateAfterOwnerCrash(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Fix", LandingMode: "local", PaneID: "w2:p1", PaneLabel: "posse:shop:t1", HerdrWorkspaceID: "w2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.QueueMessage(ctx, taskID, "Do not duplicate this instruction", false); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "prompt-accepted")
	command := exec.Command(os.Args[0], "-test.run=^TestMessageDeliveryCrashHelperProcess$")
	command.Env = append(os.Environ(),
		"POSSE_MESSAGE_CRASH_HELPER=1",
		"POSSE_MESSAGE_CRASH_HOME="+home,
		"POSSE_MESSAGE_CRASH_PROJECT="+strconv.FormatInt(project.ID, 10),
		"POSSE_MESSAGE_CRASH_MARKER="+marker,
	)
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 86 {
		t.Fatalf("delivery owner did not crash after prompt acceptance: err=%v output=%s", err, output)
	}
	if contents, err := os.ReadFile(marker); err != nil || string(contents) != "accepted\n" {
		t.Fatalf("Herdr prompt was not accepted before owner crash: contents=%q err=%v", contents, err)
	}

	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE messages SET claimed_at=? WHERE task_id=?`, time.Now().Add(-deliveryClaimTimeout-time.Second).UnixMilli(), taskID); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w2:p1", WorkspaceID: "w2", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "working"}}}
	service := testService(home, fake)
	for range 3 {
		if err := service.deliverQueuedMessages(ctx, db, project, fake.SnapshotValue); err != nil {
			t.Fatal(err)
		}
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM messages WHERE task_id=?`, taskID).Scan(&status); err != nil || status != "submitting" || fake.CallCount("agent.prompt") != 0 {
		t.Fatalf("recovery retried accepted instruction: status=%q err=%v prompts=%d", status, err, fake.CallCount("agent.prompt"))
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "message_delivery_uncertain" || !strings.Contains(notices[0].Summary, "Do not duplicate this instruction") {
		t.Fatalf("recovery did not raise one instruction-specific Notice: %#v, %v", notices, err)
	}
}

func TestNoticeClaimRollsBackWhenPromptFails(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project, _ = db.ProjectByID(ctx, project.ID)
	if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "ready"}); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"}}}
	fake.Errors["agent.prompt"] = errors.New("prompt failed")
	service := testService(home, fake)
	if err := service.deliverNotices(ctx, db, project); err == nil {
		t.Fatal("prompt failure was not returned")
	}
	var claims int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE project_id=? AND claim_token<>''`, project.ID).Scan(&claims); err != nil || claims != 0 {
		t.Fatalf("failed prompt left %d Notice claims, %v", claims, err)
	}
	fake.Errors["agent.prompt"] = nil
	if err := service.deliverNotices(ctx, db, project); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("agent.prompt") != 2 {
		t.Fatalf("retry prompt calls = %d", fake.CallCount("agent.prompt"))
	}
	undelivered, err := db.UndeliveredNotices(ctx, project.ID)
	if err != nil || len(undelivered) != 0 {
		t.Fatalf("retry did not deliver Notice: %#v, %v", undelivered, err)
	}
}

func TestLegacyClaimMigrationRaisesDeliveryNotice(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Fix", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := db.QueueMessage(ctx, taskID, "Legacy claimed instruction", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE messages SET status='claimed',claim_token='legacy',claimed_at=? WHERE id=?`, time.Now().Add(-deliveryClaimTimeout-time.Second).UnixMilli(), messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM goose_db_version WHERE version_id=21`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	message, err := db.MessageByID(ctx, messageID)
	if err != nil || message.Status != "submitting" {
		t.Fatalf("legacy claim migration did not make the message uncertain: %#v, %v", message, err)
	}
	service := testService(home, herdr.NewFake())
	created, err := service.raiseExpiredMessageDeliveryNotices(ctx, db, project)
	if err != nil || !created {
		t.Fatalf("migration recovery did not create its Notice: created=%t err=%v", created, err)
	}
	created, err = service.raiseExpiredMessageDeliveryNotices(ctx, db, project)
	if err != nil || created {
		t.Fatalf("migration recovery duplicated its Notice: created=%t err=%v", created, err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "message_delivery_uncertain" || !strings.Contains(notices[0].Summary, "#"+strconv.FormatInt(messageID, 10)) || !strings.Contains(notices[0].DataJSON, message.Body) {
		t.Fatalf("legacy migration Notice did not identify the instruction: %#v, %v", notices, err)
	}
}

func TestMessageClaimDoesNotRetryAfterAmbiguousPromptError(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Fix", LandingMode: "local", PaneID: "w2:p1", PaneLabel: "posse:shop:t1", HerdrWorkspaceID: "w2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	messageID, err := db.QueueMessage(ctx, taskID, "Please check the tests", false)
	if err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w2:p1", WorkspaceID: "w2", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "idle"}}}
	service := testService(home, acceptedPromptLostAckAdapter{Adapter: fake})
	sendErr := service.deliverQueuedMessages(ctx, db, project, fake.SnapshotValue)
	var failure *axi.Error
	if !errors.As(sendErr, &failure) || failure.Code != "message_delivery_uncertain" {
		t.Fatalf("ambiguous prompt error = %v, want message_delivery_uncertain", sendErr)
	}
	var status, token string
	if err := db.QueryRowContext(ctx, `SELECT status,claim_token FROM messages WHERE id=?`, messageID).Scan(&status, &token); err != nil || status != "submitting" || token == "" {
		t.Fatalf("uncertain prompt lost its durable no-retry state %q/%q, %v", status, token, err)
	}
	recoveredDB, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer recoveredDB.Close()
	recoveredService := testService(home, fake)
	for range 3 {
		if err := recoveredService.deliverQueuedMessages(ctx, recoveredDB, project, fake.SnapshotValue); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT status,claim_token FROM messages WHERE id=?`, messageID).Scan(&status, &token); err != nil || status != "submitting" || token == "" || fake.CallCount("agent.prompt") != 1 {
		t.Fatalf("uncertain message was retried: status=%q token=%q err=%v prompts=%d", status, token, err, fake.CallCount("agent.prompt"))
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "message_delivery_uncertain" || !strings.Contains(notices[0].Summary, "#"+strconv.FormatInt(messageID, 10)) || !strings.Contains(notices[0].DataJSON, "Please check the tests") {
		t.Fatalf("ambiguous prompt error did not create an instruction-specific Notice: %#v, %v", notices, err)
	}
	uncertain, err := db.UncertainTaskMessages(ctx, project.ID, taskID, currentTime()-deliveryClaimTimeout.Milliseconds())
	if err != nil || len(uncertain) != 1 || uncertain[0].ID != messageID {
		t.Fatalf("Task inspection omitted a fresh ambiguous instruction: %#v, %v", uncertain, err)
	}
}

func (a *promptGateAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method == "agent.prompt" {
		a.entered <- struct{}{}
		select {
		case <-a.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return a.Adapter.Call(ctx, method, params)
}

func TestConcurrentNoticeDeliveryClaimsBatchBeforePrompt(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "ready"}); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"}}}
	adapter := &promptGateAdapter{Adapter: fake, entered: make(chan struct{}, 2), release: make(chan struct{})}
	service := testService(home, adapter)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- service.deliverNotices(ctx, db, project)
		}()
	}
	close(start)
	select {
	case <-adapter.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("no Notice prompt started")
	}
	select {
	case err := <-results:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("second Notice delivery did not return while the first prompt was blocked")
	}
	close(adapter.release)
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if got := fake.CallCount("agent.prompt"); got != 1 {
		t.Fatalf("Lead prompt calls = %d, want one", got)
	}
	notices, err := db.UndeliveredNotices(ctx, project.ID)
	if err != nil || len(notices) != 0 {
		t.Fatalf("delivered Notice batch remains pending: %#v, %v", notices, err)
	}
}
