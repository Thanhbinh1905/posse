//go:build e2e

package e2e

import (
	"context"
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

func TestBusyPiSteerPastQueuedMessageAndInterruptE2E(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()

	client := herdr.NewWithEnv("herdr", fixture.env)
	if err := os.MkdirAll(filepath.Join(fixture.root, "pi", "extensions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "integration", "install", "pi"); err != nil {
		t.Fatalf("install Pi integration in isolated Herdr: %v", err)
	}
	fakeAgent := filepath.Join(filepath.Dir(fixture.binary), "pi")
	if err := os.WriteFile(fakeAgent, []byte(fakeBusyHarness), 0o700); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.deep.kind", "pi"); !strings.Contains(output, "pi") {
		t.Fatalf("configure isolated Pi Profile: %s", output)
	}
	brief := filepath.Join(fixture.root, "busy-pi.md")
	briefText := "---\ntype: change\ntitle: Busy Pi Worker\ndone_when: interruption verified\n---\nStay busy until interrupted.\n"
	if err := os.WriteFile(brief, []byte(briefText), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "simulate-auto-compacting"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", brief, "--name", "busy-pi-worker")
	task := fixture.mustTask(t, "t1")
	stopBackgroundWatch(t, fixture)
	prURL := "https://github.com/acme/shop/pull/17"
	head := strings.TrimSpace(gitTest(t, fixture.env, fixture.repo, "rev-parse", "HEAD"))
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET pr_url=?,landing_mode='pr',gated_sha=? WHERE id=?`, prURL, head, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.PersistTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	task = fixture.mustTask(t, "t1")
	blockedForge := filepath.Join(fixture.root, "blocked-pr-refresh")
	releaseForge := filepath.Join(fixture.root, "release-pr-refresh")
	ghPath := filepath.Join(fixture.root, "bin", "gh")
	ghRealPath := filepath.Join(fixture.root, "bin", "gh-real")
	if err := os.Rename(ghPath, ghRealPath); err != nil {
		t.Fatal(err)
	}
	wrapper := "#!/bin/sh\nif [ \"$1 $2\" = 'api graphql' ]; then : > " + shellQuote(blockedForge) + "; while [ ! -e " + shellQuote(releaseForge) + " ]; do sleep 0.02; done; fi\nexec " + shellQuote(ghRealPath) + " \"$@\"\n"
	if err := os.WriteFile(ghPath, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(releaseForge, []byte("release\n"), 0o600)
		_, _ = client.Call(context.Background(), "agent.send_keys", map[string]any{"target": task.PaneID, "keys": []string{"esc"}})
	})
	if !waitForCondition(15*time.Second, func() bool {
		snapshot, err := client.Snapshot(context.Background())
		if err != nil {
			return false
		}
		for _, pane := range snapshot.Panes {
			if pane.PaneID == task.PaneID {
				return pane.Agent == "pi" && pane.AgentStatus == "working" && !pane.Focused
			}
		}
		return false
	}) {
		t.Fatal("fake Pi Worker did not become busy and unfocused")
	}
	if task.State == store.StateWorking {
		if err := fixture.db.Transition(context.Background(), task.ID, store.StateWorking, store.StateDone, "worker", "E2E PR refresh setup"); err != nil {
			t.Fatal(err)
		}
		task = fixture.mustTask(t, "t1")
	}

	if _, err := client.Run(context.Background(), "pane", "report-agent", task.PaneID, "--source", "posse.e2e", "--agent", "pi", "--state", "unknown"); err != nil {
		t.Fatalf("simulate Pi's unclassified auto-compaction busy state: %v", err)
	}
	queued := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Wait until the current turn ends", "--queue")
	if !strings.Contains(queued, "state: queued") || !strings.Contains(queued, "reason: waiting_for_idle") {
		t.Fatalf("explicitly queued message was not held: %s", queued)
	}
	pending, err := fixture.db.OldestQueuedMessage(context.Background(), task.ID)
	if err != nil || pending.Body != "Wait until the current turn ends" || !pending.WaitForIdle {
		t.Fatalf("missing wait-for-idle head: message=%#v err=%v", pending, err)
	}

	backgroundID, err := fixture.db.QueueMessage(context.Background(), task.ID, "Background steering", false)
	if err != nil {
		t.Fatal(err)
	}
	backgroundEnv := setEnv(fixture.leadEnv, "HERDR_PLUGIN_EVENT_JSON", `{"event":"pane.focused","data":{"pane_id":"`+task.PaneID+`"}}`)
	backgroundOutput := runPosse(t, fixture.binary, fixture.repo, backgroundEnv, "_ingest")
	backgroundMessage, err := fixture.db.MessageByID(context.Background(), backgroundID)
	if err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(5*time.Second, func() bool {
		message, err := fixture.db.MessageByID(context.Background(), backgroundID)
		if err != nil {
			return false
		}
		backgroundMessage = message
		return message.Status == "delivered"
	}) {
		screen, _ := client.Call(context.Background(), "pane.read", map[string]any{"pane_id": task.PaneID, "source": "recent_unwrapped", "lines": 80})
		input, _ := os.ReadFile(filepath.Join(fixture.root, "fake-agent-input.log"))
		logs, _ := client.Call(context.Background(), "plugin.log.list", map[string]any{"plugin_id": "posse.herdr", "limit": 20})
		notices, _ := fixture.db.Notices(context.Background(), fixture.project.ID, false)
		t.Fatalf("background delivery did not steer past the wait-for-idle head: output=%s message=%#v pane=%#v screen=%s input=%q logs=%s notices=%#v", backgroundOutput, backgroundMessage, currentPane(t, client, task.PaneID), screen, input, logs, notices)
	}

	// The independent Watch is stopped so it cannot race this foreground send.
	steering := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Send-time steering")
	if !strings.Contains(steering, "state: delivered") || !strings.Contains(steering, "reason: steered") {
		t.Fatalf("send-time steering did not report the sent message: %s", steering)
	}
	if _, err := os.Stat(blockedForge); err != nil {
		t.Fatalf("posse send did not attempt the bounded PR refresh: stat err=%v", err)
	}
	inputLog := filepath.Join(fixture.root, "fake-agent-input.log")
	if !waitForCondition(5*time.Second, func() bool {
		contents, err := os.ReadFile(inputLog)
		return err == nil && strings.Contains(string(contents), "Background steering") && strings.Contains(string(contents), "Send-time steering")
	}) {
		contents, _ := os.ReadFile(inputLog)
		t.Fatalf("fake Pi did not receive both steering messages while busy: %s", contents)
	}
	if _, err := client.Run(context.Background(), "pane", "report-agent", task.PaneID, "--source", "posse.e2e", "--agent", "pi", "--state", "idle"); err != nil {
		t.Fatalf("mark Pi idle to drain --queue message: %v", err)
	}
	idleEnv := setEnv(fixture.leadEnv, "HERDR_PLUGIN_EVENT_JSON", `{"event":"pane_agent_status_changed","data":{"pane_id":"`+task.PaneID+`","workspace_id":"`+task.HerdrWorkspaceID+`","agent_status":"idle","agent":"pi"}}`)
	runPosse(t, fixture.binary, fixture.repo, idleEnv, "_ingest")
	if !waitForCondition(5*time.Second, func() bool {
		message, err := fixture.db.MessageByID(context.Background(), pending.ID)
		return err == nil && message.Status == "delivered"
	}) {
		message, _ := fixture.db.MessageByID(context.Background(), pending.ID)
		contents, _ := os.ReadFile(inputLog)
		t.Fatalf("--queue message was not delivered after idle: message=%#v input=%s", message, contents)
	}
	contents, err := os.ReadFile(inputLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Background steering", "Send-time steering", "Wait until the current turn ends"} {
		if count := strings.Count(string(contents), expected); count != 1 {
			t.Errorf("message %q reached fake Pi %d times, want exactly once: %s", expected, count, contents)
		}
	}
	backgroundAt := strings.Index(string(contents), "Background steering")
	sendAt := strings.Index(string(contents), "Send-time steering")
	queuedAt := strings.Index(string(contents), "Wait until the current turn ends")
	if backgroundAt < 0 || sendAt < backgroundAt || queuedAt < sendAt {
		t.Errorf("--queue message was not delivered once, in order after both steering messages: %s", contents)
	}

	if _, err := client.Run(context.Background(), "pane", "report-agent", task.PaneID, "--source", "posse.e2e", "--agent", "pi", "--state", "working"); err != nil {
		t.Fatalf("mark Pi busy before testing unsend: %v", err)
	}
	unsendQueued := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Withdraw this queued message", "--queue")
	if !strings.Contains(unsendQueued, "state: queued") {
		t.Fatalf("could not queue a message for unsend: %s", unsendQueued)
	}
	unsendMessage, err := fixture.db.OldestQueuedMessage(context.Background(), task.ID)
	if err != nil || unsendMessage.Body != "Withdraw this queued message" {
		t.Fatalf("queued message for unsend = %#v, %v", unsendMessage, err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "unsend", strconv.FormatInt(unsendMessage.ID, 10)); !strings.Contains(output, "state: withdrawn") {
		t.Fatalf("unsend did not withdraw the wait-for-idle message: %s", output)
	}
	queuedAgain := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Keep this queued", "--queue")
	if !strings.Contains(queuedAgain, "state: queued") {
		t.Fatalf("could not create a message to test claimed refusal: %s", queuedAgain)
	}
	claimed, err := fixture.db.OldestQueuedMessage(context.Background(), task.ID)
	if err != nil || claimed.Body != "Keep this queued" {
		t.Fatalf("queued message for claim test = %#v, %v", claimed, err)
	}
	if _, err := fixture.db.ClaimMessage(context.Background(), claimed.ID, "e2e-claimed", time.Now().UnixMilli(), time.Now().Add(-2*time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	refused := exec.Command(fixture.binary, "unsend", strconv.FormatInt(claimed.ID, 10))
	refused.Dir, refused.Env = fixture.repo, fixture.leadEnv
	if output, err := refused.CombinedOutput(); err == nil || !strings.Contains(string(output), "message_not_queued") {
		t.Fatalf("unsend did not refuse a claimed message: err=%v output=%s", err, output)
	}
	if err := fixture.db.RollbackMessageClaim(context.Background(), claimed.ID, "e2e-claimed"); err != nil {
		t.Fatal(err)
	}
	steerableID, err := fixture.db.QueueMessage(context.Background(), task.ID, "Older steerable queue item", false)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(blockedForge)
	_ = os.Remove(releaseForge)

	interrupt := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "--interrupt", "Stop and redirect this turn")
	if !strings.Contains(interrupt, "state: delivered") || !strings.Contains(interrupt, "reason: delivered_after_interrupt") {
		t.Fatalf("send --interrupt did not verify and deliver the redirect: %s", interrupt)
	}
	if _, err := os.Stat(blockedForge); !os.IsNotExist(err) {
		t.Fatalf("posse send --interrupt waited for or started the blocked PR refresh: stat err=%v", err)
	}
	remaining, err := fixture.db.OldestQueuedMessage(context.Background(), task.ID)
	if err != nil || remaining.ID != claimed.ID || remaining.Body != "Keep this queued" {
		t.Fatalf("interrupt reordered or duplicated a queued message: %#v, %v", remaining, err)
	}
	steerable, err := fixture.db.MessageByID(context.Background(), steerableID)
	if err != nil || steerable.Status != "queued" {
		t.Fatalf("interrupt pre-delivered an older steerable message: %#v, %v", steerable, err)
	}
	if !waitForCondition(5*time.Second, func() bool {
		contents, err := os.ReadFile(inputLog)
		return err == nil && strings.Contains(string(contents), "Stop and redirect this turn")
	}) {
		contents, _ := os.ReadFile(inputLog)
		t.Fatalf("fake Pi did not receive the interrupt redirect: %s", contents)
	}
	keyLog, err := os.ReadFile(filepath.Join(fixture.root, "fake-agent-keys.log"))
	if err != nil || !strings.Contains(string(keyLog), "pi:escape") {
		t.Errorf("Pi abort key log = %q, err=%v; want Escape", keyLog, err)
	}
	contents, err = os.ReadFile(inputLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, notDelivered := range []string{"Withdraw this queued message", "Keep this queued", "Older steerable queue item"} {
		if strings.Contains(string(contents), notDelivered) {
			t.Errorf("queued instruction was delivered out of order: %s", contents)
		}
	}
}

func TestExpiredUncertainSubmissionDoesNotBlockFreshDeliveryE2E(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	client := herdr.NewWithEnv("herdr", fixture.env)
	if err := os.MkdirAll(filepath.Join(fixture.root, "pi", "extensions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "integration", "install", "pi"); err != nil {
		t.Fatalf("install Pi integration in isolated Herdr: %v", err)
	}
	fakeAgent := filepath.Join(filepath.Dir(fixture.binary), "pi")
	if err := os.WriteFile(fakeAgent, []byte(fakeBusyHarness), 0o700); err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.deep.kind", "pi")
	brief := filepath.Join(fixture.root, "uncertain.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Expired uncertain delivery\ndone_when: fresh messages are accepted\n---\nWait for a follow-up.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", brief, "--name", "expired-uncertain")
	task := fixture.mustTask(t, "t1")
	if !waitForCondition(15*time.Second, func() bool {
		snapshot, err := client.Snapshot(context.Background())
		if err != nil {
			return false
		}
		for _, pane := range snapshot.Panes {
			if pane.PaneID == task.PaneID {
				return pane.Agent == "pi" && pane.AgentStatus == "working" && !pane.Focused
			}
		}
		return false
	}) {
		t.Fatal("isolated Pi Worker did not become busy for the stale-submission reproduction")
	}
	ctx := context.Background()
	originalID, err := fixture.db.QueueMessage(ctx, task.ID, "Ambiguous original must never replay", false)
	if err != nil {
		t.Fatal(err)
	}
	claimToken := "e2e-uncertain-original"
	if claimed, err := fixture.db.ClaimMessage(ctx, originalID, claimToken, time.Now().Add(-3*time.Minute).UnixMilli(), time.Now().Add(-5*time.Minute).UnixMilli()); err != nil || !claimed {
		t.Fatalf("claim ambiguous original: claimed=%t err=%v", claimed, err)
	}
	if err := fixture.db.MarkMessageSubmitting(ctx, originalID, claimToken); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.CreateMessageDeliveryUncertainNotice(ctx, fixture.project.ID, task.ID, originalID, "Message may not have reached the Worker", "Ambiguous original must never replay"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE messages SET claimed_at=? WHERE id=?`, time.Now().Add(-3*time.Minute).UnixMilli(), originalID); err != nil {
		t.Fatal(err)
	}

	plainCommand := exec.Command(fixture.binary, "send", "t1", "Fresh plain instruction")
	plainCommand.Dir, plainCommand.Env = fixture.repo, fixture.leadEnv
	plainOutput, plainErr := plainCommand.CombinedOutput()
	backgroundID, err := fixture.db.QueueMessage(ctx, task.ID, "Fresh background instruction", false)
	if err != nil {
		t.Fatal(err)
	}
	backgroundEnv := setEnv(fixture.leadEnv, "HERDR_PLUGIN_EVENT_JSON", `{"event":"pane.focused","data":{"pane_id":"`+task.PaneID+`"}}`)
	backgroundOutput := runPosse(t, fixture.binary, fixture.repo, backgroundEnv, "_ingest")
	if !waitForCondition(5*time.Second, func() bool {
		message, err := fixture.db.MessageByID(ctx, backgroundID)
		if err != nil || message.Status != "delivered" {
			return false
		}
		input, err := os.ReadFile(filepath.Join(fixture.root, "fake-agent-input.log"))
		return err == nil && strings.Contains(string(input), "Fresh background instruction")
	}) {
		background, _ := fixture.db.MessageByID(ctx, backgroundID)
		input, _ := os.ReadFile(filepath.Join(fixture.root, "fake-agent-input.log"))
		t.Fatalf("background instruction did not settle before interrupt: message=%#v output=%s input=%s", background, backgroundOutput, input)
	}
	interruptCommand := exec.Command(fixture.binary, "send", "t1", "--interrupt", "Fresh stop and redirect")
	interruptCommand.Dir, interruptCommand.Env = fixture.repo, fixture.leadEnv
	interruptOutput, interruptErr := interruptCommand.CombinedOutput()
	var plainID int64
	if err := fixture.db.QueryRowContext(ctx, `SELECT id FROM messages WHERE task_id=? AND body='Fresh plain instruction'`, task.ID).Scan(&plainID); err != nil {
		t.Fatal(err)
	}
	plain, plainReadErr := fixture.db.MessageByID(ctx, plainID)
	background, backgroundReadErr := fixture.db.MessageByID(ctx, backgroundID)
	original, originalReadErr := fixture.db.MessageByID(ctx, originalID)
	var uncertainNotices int
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='message_delivery_uncertain' AND acked_at IS NULL`, task.ID).Scan(&uncertainNotices); err != nil {
		t.Fatal(err)
	}
	var redirectStatus string
	redirectReadErr := fixture.db.QueryRowContext(ctx, `SELECT status FROM messages WHERE task_id=? AND body='Fresh stop and redirect'`, task.ID).Scan(&redirectStatus)
	input, _ := os.ReadFile(filepath.Join(fixture.root, "fake-agent-input.log"))
	if plainErr != nil || !strings.Contains(string(plainOutput), "state: delivered") || plainReadErr != nil || plain.Status != "delivered" || backgroundReadErr != nil || background.Status != "delivered" || !strings.Contains(string(input), "Fresh background instruction") || interruptErr != nil || !strings.Contains(string(interruptOutput), "state: delivered") || redirectReadErr != nil || redirectStatus != "delivered" || originalReadErr != nil || original.Status != "submitting" || uncertainNotices != 1 || strings.Contains(string(input), "Ambiguous original must never replay") {
		t.Fatalf("expired uncertainty blocked safe fresh delivery or was replayed: plain=%#v plainErr=%v plainOutput=%s background=%#v backgroundErr=%v backgroundOutput=%s redirect=%s redirectErr=%v interruptOutput=%s original=%#v originalErr=%v notices=%d input=%s", plain, plainErr, plainOutput, background, backgroundReadErr, backgroundOutput, redirectStatus, redirectReadErr, interruptOutput, original, originalReadErr, uncertainNotices, input)
	}
}

func TestUnsendDoesNotDeliverQueuedMessageE2E(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()

	client := herdr.NewWithEnv("herdr", fixture.env)
	if err := os.MkdirAll(filepath.Join(fixture.root, "pi", "extensions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "integration", "install", "pi"); err != nil {
		t.Fatalf("install Pi integration in isolated Herdr: %v", err)
	}
	fakeAgent := filepath.Join(filepath.Dir(fixture.binary), "pi")
	if err := os.WriteFile(fakeAgent, []byte(fakeBusyHarness), 0o700); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.deep.kind", "pi"); !strings.Contains(output, "pi") {
		t.Fatalf("configure isolated Pi Profile: %s", output)
	}
	brief := filepath.Join(fixture.root, "unsend.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Unsend delivery regression\ndone_when: unsend leaves the queue untouched\n---\nWait for a follow-up.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", brief, "--name", "unsend-delivery")
	task := fixture.mustTask(t, "t1")
	if !waitForCondition(15*time.Second, func() bool {
		snapshot, err := client.Snapshot(context.Background())
		if err != nil {
			return false
		}
		for _, pane := range snapshot.Panes {
			if pane.PaneID == task.PaneID {
				return pane.Agent == "pi" && pane.AgentStatus == "working" && !pane.Focused
			}
		}
		return false
	}) {
		t.Fatal("isolated Pi Worker did not become ready for the unsend reproduction")
	}
	// The poll-only Watch is a valid competing delivery owner. Stop it so
	// this regression measures unsend's own preparation, not that race.
	stopBackgroundWatch(t, fixture)
	if _, err := client.Run(context.Background(), "pane", "report-agent", task.PaneID, "--source", "posse.e2e", "--agent", "pi", "--state", "idle"); err != nil {
		t.Fatalf("mark Pi idle for the unsend reproduction: %v", err)
	}
	inputBefore, err := os.ReadFile(filepath.Join(fixture.root, "fake-agent-input.log"))
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := fixture.db.QueueMessage(context.Background(), task.ID, "This queued instruction must not be delivered by unsend", false)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(fixture.binary, "unsend", strconv.FormatInt(messageID, 10))
	command.Dir, command.Env = fixture.repo, fixture.leadEnv
	output, err := command.CombinedOutput()
	message, readErr := fixture.db.MessageByID(context.Background(), messageID)
	input, _ := os.ReadFile(filepath.Join(fixture.root, "fake-agent-input.log"))
	if err != nil || !strings.Contains(string(output), "state: withdrawn") || readErr != nil || message.Status != "withdrawn" || string(input) != string(inputBefore) {
		t.Fatalf("unsend prompted the Worker before withdrawing the message: err=%v output=%s message=%#v readErr=%v before=%s after=%s", err, output, message, readErr, inputBefore, input)
	}
}

func TestBuiltInInterruptKeysVerifyStoppedWorkerForEachKindE2E(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	client := herdr.NewWithEnv("herdr", fixture.env)
	// Exercise direct send delivery without a poll-only Watch racing to claim it.
	stopBackgroundWatch(t, fixture)
	keyByKind := map[string]string{"claude": "ctrl+c", "codex": "escape", "pi": "escape", "opencode": "escape"}
	for i, kind := range []string{"claude", "codex", "pi", "opencode"} {
		fakeAgent := filepath.Join(filepath.Dir(fixture.binary), kind)
		if err := os.WriteFile(fakeAgent, []byte(fakeBusyHarness), 0o700); err != nil {
			t.Fatal(err)
		}
		runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.deep.kind", kind)
		seq := i + 1
		label := "posse:shop:t" + strconv.Itoa(seq)
		workerRoot := filepath.Join(fixture.root, "worker-"+kind)
		if output, err := gitCommand(fixture.env, fixture.repo, "worktree", "add", "-b", "posse/interrupt-"+kind, workerRoot, "main"); err != nil {
			t.Fatalf("create isolated %s Worker worktree: %v %s", kind, err, output)
		}
		worker, err := createTab(client, fixture.project.HerdrWorkspaceID, workerRoot, label)
		if err != nil {
			t.Fatalf("create isolated %s Worker pane: %v", kind, err)
		}
		if _, err := client.Call(context.Background(), "pane.rename", map[string]any{"pane_id": worker.RootPane.PaneID, "label": label}); err != nil {
			t.Fatalf("label isolated %s Worker pane: %v", kind, err)
		}
		taskID, err := fixture.db.CreateTask(context.Background(), fixture.project.ID, store.Task{
			Seq: seq, Type: "change", Title: "Interrupt " + kind, ShortName: "interrupt-" + kind,
			Profile: "deep", DispatchRule: "dispatch.default", MergeMode: "pr",
			Branch: "posse/interrupt-" + kind, BaseRef: "main", WorktreePath: workerRoot,
			HerdrWorkspaceID: fixture.project.HerdrWorkspaceID, PaneID: worker.RootPane.PaneID, PaneLabel: label,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.db.Transition(context.Background(), taskID, store.StateSpawning, store.StateWorking, "cli", "fake Worker started"); err != nil {
			t.Fatal(err)
		}
		task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t"+strconv.Itoa(seq))
		if err != nil {
			t.Fatal(err)
		}
		command := "POSSE_TEST_BUSY_NOW=1 exec " + shellQuote(fakeAgent)
		if _, err := client.Run(context.Background(), "pane", "run", task.PaneID, command); err != nil {
			t.Fatalf("start busy fake %s Worker: %v", kind, err)
		}
		t.Cleanup(func() {
			_, _ = client.Call(context.Background(), "agent.send_keys", map[string]any{"target": task.PaneID, "keys": []string{"esc"}})
		})
		if !waitForCondition(15*time.Second, func() bool {
			snapshot, err := client.Snapshot(context.Background())
			if err != nil {
				return false
			}
			for _, pane := range snapshot.Panes {
				if pane.PaneID == task.PaneID {
					return pane.Agent == kind && pane.AgentStatus == "working" && !pane.Focused
				}
			}
			return false
		}) {
			t.Fatalf("fake %s Worker did not become busy and unfocused: %#v", kind, currentPane(t, client, task.PaneID))
		}
		taskName := "t" + strconv.Itoa(seq)
		if kind == "opencode" {
			output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", taskName, "Steer this turn now")
			if !strings.Contains(output, "state: queued") || !strings.Contains(output, "kind_does_not_support_steering") {
				t.Fatalf("OpenCode send did not explain why it waits: %s", output)
			}
		} else {
			output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", taskName, "Steer this turn now")
			if !strings.Contains(output, "state: delivered") {
				t.Fatalf("%s send did not reach its busy Worker: %s", kind, output)
			}
			if !waitForCondition(5*time.Second, func() bool {
				contents, err := os.ReadFile(filepath.Join(fixture.root, "fake-agent-input.log"))
				return err == nil && strings.Contains(string(contents), kind+":") && strings.Contains(string(contents), "Steer this turn now")
			}) {
				contents, _ := os.ReadFile(filepath.Join(fixture.root, "fake-agent-input.log"))
				t.Fatalf("%s fake Worker did not receive the steering message: %s", kind, contents)
			}
		}
		output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "interrupt", taskName)
		if !strings.Contains(output, "verified: true") || !strings.Contains(output, "abort_keys") {
			t.Fatalf("interrupt did not report a verified status change for %s: %s", kind, output)
		}
		if !waitForCondition(5*time.Second, func() bool {
			snapshot, err := client.Snapshot(context.Background())
			if err != nil {
				return false
			}
			for _, pane := range snapshot.Panes {
				if pane.PaneID == task.PaneID {
					return pane.AgentStatus != "working"
				}
			}
			return false
		}) {
			t.Fatalf("%s remained working after interrupt: %#v", kind, currentPane(t, client, task.PaneID))
		}
		keys, err := os.ReadFile(filepath.Join(fixture.root, "fake-agent-keys.log"))
		if err != nil || !strings.Contains(string(keys), kind+":"+keyByKind[kind]) {
			t.Fatalf("%s abort key log = %q, err=%v; want %s", kind, keys, err, keyByKind[kind])
		}
	}
}

func currentPane(t *testing.T, client *herdr.Client, paneID string) any {
	t.Helper()
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		return err
	}
	for _, pane := range snapshot.Panes {
		if pane.PaneID == paneID {
			return pane
		}
	}
	return "pane missing"
}

const fakeBusyHarness = `#!/usr/bin/env python3
import os
import select
import subprocess
import sys
import termios
import time
import tty

kind = os.path.basename(sys.argv[0])
pane = os.environ["HERDR_PANE_ID"]
root = os.environ["POSSE_TEST_ROOT"]
input_log = os.path.join(root, "fake-agent-input.log")
key_log = os.path.join(root, "fake-agent-keys.log")
fd = sys.stdin.fileno()


def report(state):
    subprocess.run(["herdr", "pane", "report-agent", pane, "--source", "posse.test", "--agent", kind, "--state", state], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)


def append(path, value):
    with open(path, "a", encoding="utf-8") as output:
        output.write(value + "\n")


report("idle")
old = termios.tcgetattr(fd)
tty.setraw(fd)
line = bytearray()
busy = os.environ.get("POSSE_TEST_BUSY_NOW") == "1"
if busy:
    report("working")
if kind == "pi" and os.path.exists(os.path.join(root, "simulate-auto-compacting")):
    print("Auto-compacting...")
try:
    while True:
        value = os.read(fd, 1)
        if not value:
            break
        if value == b"\x03":
            append(key_log, kind + ":ctrl+c")
            if kind == "claude":
                busy = False
                report("idle")
                time.sleep(0.4)
                line.clear()
            continue
        if value == b"\x1b":
            ready, _, _ = select.select([fd], [], [], 0.01)
            if ready:
                following = os.read(fd, 1)
                if following == b"[":
                    sequence = bytearray()
                    while True:
                        part = os.read(fd, 1)
                        sequence.extend(part)
                        if part == b"~":
                            break
                    if sequence in (b"[200~", b"[201~"):
                        continue
                    line.extend(b"\x1b" + sequence)
                    continue
                line.extend(b"\x1b" + following)
                continue
            append(key_log, kind + ":escape")
            if kind != "claude":
                busy = False
                report("idle")
                time.sleep(0.4)
                line.clear()
            continue
        if value in (b"\r", b"\n"):
            text = line.decode("utf-8", errors="replace").strip()
            line.clear()
            if text:
                append(input_log, kind + ":" + text)
                if not busy:
                    busy = True
                    report("working")
            continue
        line.extend(value)
finally:
    termios.tcsetattr(fd, termios.TCSADRAIN, old)
`
