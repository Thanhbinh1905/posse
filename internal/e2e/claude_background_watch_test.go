//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/execgroup"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestRealClaudeBackgroundWatchWakesOnlyForNotice(t *testing.T) {
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("real Claude executable is not installed")
	}
	f := newPRLifecycleFixtureWithLead(t, true)
	defer f.db.Close()
	stopBackgroundWatch(t, f)
	defer stopFixtureLeadWatches(t, f.root, f.binary, f.home)

	instructionsPath := filepath.Join(f.home, "projects", f.project.Name, "lead.md")
	instructions, err := os.ReadFile(instructionsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"background command", "--handoff", "CLAUDE_CODE_SESSION_ID", "without a timeout", "`Read` on the output file", "posse ack"} {
		if !strings.Contains(string(instructions), expected) {
			t.Fatalf("Lead instructions omitted %q: %s", expected, instructions)
		}
	}
	if strings.Contains(string(instructions), "Stop hook owns") {
		t.Fatalf("Lead instructions still require a quiet Stop-hook renewal: %s", instructions)
	}

	const quietPeriod = 4*time.Minute + 10*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	type modelRequest struct {
		call int
		body string
	}
	requests := make(chan modelRequest, 8)
	var noticeID, secondNoticeID atomic.Int64
	firstWatchStarted := filepath.Join(f.root, "claude-watch-started")
	rearmedWatchStarted := filepath.Join(f.root, "claude-watch-rearmed")
	watchCommand := func(marker string) string {
		return fmt.Sprintf("touch %s && exec %s watch --json --quiet-routine --handoff --destination \"claude:${CLAUDE_CODE_SESSION_ID}\"", shellQuote(marker), shellQuote(f.binary))
	}
	server := claudeLocalModel(t, func(call int, body string) []map[string]any {
		requests <- modelRequest{call: call, body: body}
		switch call {
		case 1:
			return []map[string]any{{
				"type": "tool_use", "id": "quiet_watch", "name": "Bash",
				"input": map[string]any{"command": watchCommand(firstWatchStarted), "run_in_background": true, "timeout": 600000, "description": "Wait for the next Posse Notice"},
			}}
		case 2:
			return []map[string]any{{"type": "text", "text": "The background Watch is running. I will wait for it to return."}}
		case 3, 7:
			path := backgroundWatchOutputPath(body)
			return []map[string]any{{
				"type": "tool_use", "id": fmt.Sprintf("read_watch_%d", call), "name": "Read",
				"input": map[string]any{"file_path": path},
			}}
		case 4:
			if !waitForCondition(time.Second, func() bool { return noticeID.Load() > 0 }) {
				return []map[string]any{{"type": "text", "text": "The Notice ID was not available."}}
			}
			command := fmt.Sprintf("exec %s ack %d", shellQuote(f.binary), noticeID.Load())
			return []map[string]any{{
				"type": "tool_use", "id": "ack_notice", "name": "Bash",
				"input": map[string]any{"command": command, "description": "Acknowledge the handled Notice"},
			}}
		case 5:
			return []map[string]any{{
				"type": "tool_use", "id": "rearm_watch", "name": "Bash",
				"input": map[string]any{"command": watchCommand(rearmedWatchStarted), "run_in_background": true, "timeout": 600000, "description": "Wait for the next Posse Notice"},
			}}
		case 6:
			return []map[string]any{{"type": "text", "text": "The replacement background Watch is running."}}
		case 8:
			if !waitForCondition(time.Second, func() bool { return secondNoticeID.Load() > 0 }) {
				return []map[string]any{{"type": "text", "text": "The second Notice ID was not available."}}
			}
			command := fmt.Sprintf("exec %s ack %d", shellQuote(f.binary), secondNoticeID.Load())
			return []map[string]any{{
				"type": "tool_use", "id": "ack_second_notice", "name": "Bash",
				"input": map[string]any{"command": command, "description": "Acknowledge the next handled Notice"},
			}}
		default:
			return []map[string]any{{"type": "text", "text": "Notice acknowledged."}}
		}
	})
	command := execgroup.CommandContext(ctx, claude, "-p", "--dangerously-skip-permissions", "--setting-sources", "", "--model", "claude-sonnet-4-6", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--append-system-prompt-file", instructionsPath)
	command.Dir, command.Env = f.repo, realClaudeEnv(f, server.URL)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(f.root, "claude-background-watch.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	defer func() {
		_ = stdin.Close()
		if !finished {
			_ = command.Cancel()
			_ = command.Wait()
		}
	}()
	_, _ = fmt.Fprintln(stdin, `{"type":"user","message":{"role":"user","content":"Start the Notice wait according to your Lead instructions."}}`)
	select {
	case first := <-requests:
		if first.call != 1 {
			t.Fatalf("first Claude model request = %d: %s", first.call, first.body)
		}
	case <-ctx.Done():
		t.Fatalf("Claude did not start the background Watch: %s", readTestFile(t, log.Name()))
	}
	if !waitForCondition(10*time.Second, func() bool {
		_, err := os.Stat(firstWatchStarted)
		return err == nil
	}) {
		t.Fatalf("Claude did not start its background Watch: %s", readTestFile(t, log.Name()))
	}
	select {
	case started := <-requests:
		if started.call != 2 || !strings.Contains(started.body, "Command running in background") {
			t.Fatalf("Claude did not finish the background-command start turn: call=%d body=%s", started.call, started.body)
		}
	case <-ctx.Done():
		t.Fatalf("Claude did not report that its background Watch started: %s", readTestFile(t, log.Name()))
	}

	quietTimer := time.NewTimer(quietPeriod)
	defer quietTimer.Stop()
	select {
	case unexpected := <-requests:
		t.Fatalf("Claude used another model turn during a quiet period: call=%d body=%s", unexpected.call, unexpected.body)
	case <-quietTimer.C:
	case <-ctx.Done():
		t.Fatalf("quiet period ended with Claude session error: %s", readTestFile(t, log.Name()))
	}

	id, err := f.db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, Kind: "task_done", Summary: "wake after a quiet period longer than one renewal", DataJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	noticeID.Store(id)
	select {
	case completed := <-requests:
		if completed.call != 3 || !strings.Contains(completed.body, "status>completed") || !strings.Contains(completed.body, "<output-file>") {
			t.Fatalf("completed Watch did not wake Claude with its output path: call=%d body=%s", completed.call, completed.body)
		}
	case <-ctx.Done():
		t.Fatalf("Notice did not wake Claude: %s", readTestFile(t, log.Name()))
	}
	select {
	case delivered := <-requests:
		if delivered.call != 4 || !strings.Contains(delivered.body, "wake after a quiet period longer than one renewal") || !strings.Contains(delivered.body, "delivery_id") {
			t.Fatalf("Claude did not read the Notice batch and receipt: call=%d body=%s", delivered.call, delivered.body)
		}
	case <-ctx.Done():
		t.Fatalf("Claude did not read the returned Notice batch: %s", readTestFile(t, log.Name()))
	}
	select {
	case acknowledged := <-requests:
		if acknowledged.call != 5 {
			t.Fatalf("Claude did not continue after acknowledging the Notice: call=%d body=%s", acknowledged.call, acknowledged.body)
		}
	case <-ctx.Done():
		t.Fatalf("Claude did not acknowledge the Notice: %s", readTestFile(t, log.Name()))
	}
	if !waitForCondition(10*time.Second, func() bool {
		_, err := os.Stat(rearmedWatchStarted)
		return err == nil
	}) {
		t.Fatalf("Claude did not rearm its background Watch: %s", readTestFile(t, log.Name()))
	}
	select {
	case started := <-requests:
		if started.call != 6 || !strings.Contains(started.body, "Command running in background") {
			t.Fatalf("Claude did not finish the replacement Watch start turn: call=%d body=%s", started.call, started.body)
		}
	case <-ctx.Done():
		t.Fatalf("Claude did not report that the replacement Watch started: %s", readTestFile(t, log.Name()))
	}
	if !waitForCondition(10*time.Second, func() bool {
		var state string
		err := f.db.QueryRowContext(context.Background(), `SELECT state FROM notice_delivery_receipts WHERE project_id=?`, f.project.ID).Scan(&state)
		return err == nil && state == "accepted"
	}) {
		t.Fatal("acknowledging the delivered Notice did not accept its receipt")
	}
	var deliveredAt, ackedAt int64
	if err := f.db.QueryRowContext(context.Background(), `SELECT delivered_at,acked_at FROM notices WHERE id=?`, id).Scan(&deliveredAt, &ackedAt); err != nil || deliveredAt == 0 || ackedAt == 0 {
		t.Fatalf("receipt settlement lost Notice acknowledgment: delivered=%d acked=%d err=%v", deliveredAt, ackedAt, err)
	}

	secondID, err := f.db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, Kind: "task_done", Summary: "replacement Watch still wakes for the next Notice", DataJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	secondNoticeID.Store(secondID)
	select {
	case completed := <-requests:
		if completed.call != 7 || !strings.Contains(completed.body, "status>completed") || !strings.Contains(completed.body, "<output-file>") {
			t.Fatalf("replacement Watch did not wake Claude with its output path: call=%d body=%s", completed.call, completed.body)
		}
	case <-ctx.Done():
		t.Fatalf("replacement Watch did not deliver the next Notice: %s", readTestFile(t, log.Name()))
	}
	select {
	case delivered := <-requests:
		if delivered.call != 8 || !strings.Contains(delivered.body, "replacement Watch still wakes for the next Notice") || !strings.Contains(delivered.body, "delivery_id") {
			t.Fatalf("Claude did not read the replacement Watch batch: call=%d body=%s", delivered.call, delivered.body)
		}
	case <-ctx.Done():
		t.Fatalf("Claude did not read the replacement Watch batch: %s", readTestFile(t, log.Name()))
	}
	select {
	case acknowledged := <-requests:
		if acknowledged.call != 9 {
			t.Fatalf("Claude did not continue after acknowledging the second Notice: call=%d body=%s", acknowledged.call, acknowledged.body)
		}
	case <-ctx.Done():
		t.Fatalf("Claude did not acknowledge the second Notice: %s", readTestFile(t, log.Name()))
	}
	if !waitForCondition(10*time.Second, func() bool {
		var state string
		err := f.db.QueryRowContext(context.Background(), `SELECT state FROM notice_delivery_receipts WHERE project_id=? ORDER BY created_at DESC LIMIT 1`, f.project.ID).Scan(&state)
		return err == nil && state == "accepted"
	}) {
		t.Fatal("acknowledging the second Notice did not accept its receipt")
	}
	if err := f.db.QueryRowContext(context.Background(), `SELECT delivered_at,acked_at FROM notices WHERE id=?`, secondID).Scan(&deliveredAt, &ackedAt); err != nil || deliveredAt == 0 || ackedAt == 0 {
		t.Fatalf("second receipt settlement lost Notice acknowledgment: delivered=%d acked=%d err=%v", deliveredAt, ackedAt, err)
	}
	var receipts, accepted int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*),SUM(CASE WHEN state='accepted' THEN 1 ELSE 0 END) FROM notice_delivery_receipts WHERE project_id=?`, f.project.ID).Scan(&receipts, &accepted); err != nil || receipts != 2 || accepted != 2 {
		t.Fatalf("background Watch cycles did not settle exactly two receipts: receipts=%d accepted=%d err=%v", receipts, accepted, err)
	}

	_ = stdin.Close()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatalf("Claude graceful shutdown: %v\n%s", err, readTestFile(t, log.Name()))
		}
	case <-time.After(8 * time.Second):
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		finished = true
		t.Fatal("Claude did not stop its owned background Watch")
	}
	t.Log("real Claude background Bash: no model turns for longer than one renewal, Notice woke the Lead, receipt accepted, acknowledgment preserved")
}

func backgroundWatchOutputPath(body string) string {
	const startTag, endTag = "<output-file>", "</output-file>"
	start := strings.LastIndex(body, startTag)
	if start < 0 {
		return ""
	}
	start += len(startTag)
	end := strings.Index(body[start:], endTag)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(body[start : start+end])
}

func stopFixtureLeadWatches(t *testing.T, root, binary, home string) {
	t.Helper()
	pids := fixtureLeadWatchPIDs(root, binary, home)
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			t.Errorf("stop fixture Lead Watch %d: %v", pid, err)
		}
	}
	if waitForCondition(5*time.Second, func() bool { return len(fixtureLeadWatchPIDs(root, binary, home)) == 0 }) {
		return
	}
	pids = fixtureLeadWatchPIDs(root, binary, home)
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			t.Errorf("kill fixture Lead Watch %d: %v", pid, err)
		}
	}
	if !waitForCondition(2*time.Second, func() bool { return len(fixtureLeadWatchPIDs(root, binary, home)) == 0 }) {
		t.Errorf("fixture Lead Watches remain after shutdown: %v", fixtureLeadWatchPIDs(root, binary, home))
	}
}

func fixtureLeadWatchPIDs(root, binary, home string) []int {
	entries, _ := os.ReadDir("/proc")
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		processRoot := filepath.Join("/proc", entry.Name())
		stat, err := os.ReadFile(filepath.Join(processRoot, "stat"))
		if err != nil {
			continue
		}
		if closeParen := strings.LastIndexByte(string(stat), ')'); closeParen >= 0 {
			fields := strings.Fields(string(stat[closeParen+1:]))
			if len(fields) > 0 && fields[0] == "Z" {
				continue
			}
		}
		commandLine, err := os.ReadFile(filepath.Join(processRoot, "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(commandLine), "\x00")
		if len(args) < 2 || args[0] != binary || args[1] != "watch" {
			continue
		}
		environment, err := os.ReadFile(filepath.Join(processRoot, "environ"))
		if err != nil || !strings.Contains("\x00"+string(environment), "\x00POSSE_HOME="+home+"\x00") || !strings.Contains(string(environment), root) {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}
