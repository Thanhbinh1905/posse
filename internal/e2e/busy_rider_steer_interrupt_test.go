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
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "config", "set", "profiles.deep.kind", "pi"); !strings.Contains(output, "pi") {
		t.Fatalf("configure isolated Pi Profile: %s", output)
	}
	brief := filepath.Join(fixture.root, "busy-pi.md")
	briefText := "---\ntype: ship\ntitle: Busy Pi Rider\ndone_when: interruption verified\n---\nStay busy until interrupted.\n"
	if err := os.WriteFile(brief, []byte(briefText), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "simulate-auto-compacting"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "ride", "--brief", brief, "--name", "busy-pi-rider")
	task := fixture.mustTask(t, "t1")
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
				return pane.Agent == "pi" && pane.AgentStatus == "working" && !pane.Focused
			}
		}
		return false
	}) {
		t.Fatal("fake Pi Rider did not become busy and unfocused")
	}

	if _, err := client.Run(context.Background(), "pane", "report-agent", task.PaneID, "--source", "posse.e2e", "--agent", "pi", "--state", "unknown"); err != nil {
		t.Fatalf("simulate Pi's unclassified auto-compaction busy state: %v", err)
	}
	queued := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Wait until the current turn ends", "--queue")
	if !strings.Contains(queued, "waiting_for_idle (--queue)") {
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

	steering := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Send-time steering")
	if !strings.Contains(steering, "state: delivered") || !strings.Contains(steering, "reason: steered") {
		t.Fatalf("send-time steering did not report the sent message: %s", steering)
	}
	inputLog := filepath.Join(fixture.root, "fake-agent-input.log")
	if !waitForCondition(5*time.Second, func() bool {
		contents, err := os.ReadFile(inputLog)
		return err == nil && strings.Contains(string(contents), "Background steering") && strings.Contains(string(contents), "Send-time steering")
	}) {
		contents, _ := os.ReadFile(inputLog)
		t.Fatalf("fake Pi did not receive both steering messages while busy: %s", contents)
	}

	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "unsend", strconv.FormatInt(pending.ID, 10)); !strings.Contains(output, "state: withdrawn") {
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
	if _, err := fixture.db.ClaimMessage(context.Background(), claimed.ID, "e2e-claimed", time.Now().UnixMilli()); err != nil {
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

	interrupt := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "--interrupt", "Stop and redirect this turn")
	if !strings.Contains(interrupt, "state: delivered") || !strings.Contains(interrupt, "reason: delivered_after_interrupt") {
		t.Fatalf("send --interrupt did not verify and deliver the redirect: %s", interrupt)
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
	contents, err := os.ReadFile(inputLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, notDelivered := range []string{"Wait until the current turn ends", "Keep this queued", "Older steerable queue item"} {
		if strings.Contains(string(contents), notDelivered) {
			t.Errorf("queued instruction was delivered out of order: %s", contents)
		}
	}
}

func TestBuiltInInterruptKeysVerifyStoppedRiderForEachKindE2E(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	client := herdr.NewWithEnv("herdr", fixture.env)
	keyByKind := map[string]string{"claude": "ctrl+c", "codex": "escape", "pi": "escape", "opencode": "escape"}
	for i, kind := range []string{"claude", "codex", "pi", "opencode"} {
		fakeAgent := filepath.Join(filepath.Dir(fixture.binary), kind)
		if err := os.WriteFile(fakeAgent, []byte(fakeBusyHarness), 0o700); err != nil {
			t.Fatal(err)
		}
		runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "config", "set", "profiles.deep.kind", kind)
		seq := i + 1
		label := "posse:shop:t" + strconv.Itoa(seq)
		rider, err := createTab(client, fixture.project.HerdrWorkspaceID, fixture.repo, label)
		if err != nil {
			t.Fatalf("create isolated %s Rider pane: %v", kind, err)
		}
		if _, err := client.Call(context.Background(), "pane.rename", map[string]any{"pane_id": rider.RootPane.PaneID, "label": label}); err != nil {
			t.Fatalf("label isolated %s Rider pane: %v", kind, err)
		}
		taskID, err := fixture.db.CreateTask(context.Background(), fixture.project.ID, store.Task{
			Seq: seq, Type: "ship", Title: "Interrupt " + kind, ShortName: "interrupt-" + kind,
			Profile: "deep", DispatchRule: "dispatch.default", LandingMode: "pr",
			Branch: "posse/interrupt-" + kind, BaseRef: "main", WorktreePath: fixture.repo,
			HerdrWorkspaceID: fixture.project.HerdrWorkspaceID, PaneID: rider.RootPane.PaneID, PaneLabel: label,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.db.Transition(context.Background(), taskID, store.StateSpawning, store.StateWorking, "cli", "fake Rider started"); err != nil {
			t.Fatal(err)
		}
		task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t"+strconv.Itoa(seq))
		if err != nil {
			t.Fatal(err)
		}
		command := "POSSE_TEST_BUSY_NOW=1 exec " + shellQuote(fakeAgent)
		if _, err := client.Run(context.Background(), "pane", "run", task.PaneID, command); err != nil {
			t.Fatalf("start busy fake %s Rider: %v", kind, err)
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
			t.Fatalf("fake %s Rider did not become busy and unfocused: %#v", kind, currentPane(t, client, task.PaneID))
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
				t.Fatalf("%s send did not reach its busy Rider: %s", kind, output)
			}
			if !waitForCondition(5*time.Second, func() bool {
				contents, err := os.ReadFile(filepath.Join(fixture.root, "fake-agent-input.log"))
				return err == nil && strings.Contains(string(contents), kind+":") && strings.Contains(string(contents), "Steer this turn now")
			}) {
				contents, _ := os.ReadFile(filepath.Join(fixture.root, "fake-agent-input.log"))
				t.Fatalf("%s fake Rider did not receive the steering message: %s", kind, contents)
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
