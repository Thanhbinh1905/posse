package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

const claudeStopWait = 4 * time.Minute

// RunClaudeStop is a Claude-owned asyncRewake Stop hook, not a detached relay.
// Every invocation expires before the harness's five-minute hook deadline. Exit
// 2 asks Claude to handle a Notice or finish a quiet renewal turn; its next Stop
// owns the next wait. SIGTERM and session teardown never ask for another turn.
func (s *Service) RunClaudeStop(input io.Reader, output io.Writer, args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := s.claudeStop(ctx, input, output, args); err != nil {
		if ctx.Err() != nil {
			return 0
		}
		if errors.Is(err, errClaudeRewake) {
			return 2
		}
		fmt.Fprintf(output, "Posse Notice wait failed: %v. Inspect the Project; do not start a background Watch. The next Stop hook renews the wait.\n", err)
		return 2
	}
	return 0
}

type claudeStopInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	HookEventName  string `json:"hook_event_name"`
	AgentID        string `json:"agent_id"`
}

func (s *Service) claudeStop(ctx context.Context, input io.Reader, output io.Writer, args []string) error {
	parsed, err := parseArgs("_claude-stop", args, map[string]flagSpec{"timeout": {}})
	if err != nil || len(parsed.Positionals) != 0 {
		return axi.Usage("_claude-stop accepts only --timeout ms")
	}
	wait := claudeStopWait
	if value := parsed.Flags["timeout"]; value != "" {
		ms, err := strconv.ParseInt(value, 10, 64)
		if err != nil || ms < 1 || ms > claudeStopWait.Milliseconds() {
			return axi.Usage("_claude-stop timeout must be between 1 and 240000 milliseconds")
		}
		wait = time.Duration(ms) * time.Millisecond
	}
	bounded, cancel := context.WithTimeout(ctx, wait+5*time.Second)
	defer cancel()
	var event claudeStopInput
	if err := json.NewDecoder(io.LimitReader(input, 1<<20)).Decode(&event); err != nil {
		return fmt.Errorf("read Claude Stop input: %w", err)
	}
	if event.HookEventName != "Stop" || event.AgentID != "" {
		return nil // Project hooks also run in subagents; they cannot own this wait.
	}
	if event.SessionID == "" || event.TranscriptPath == "" {
		return errors.New("claude Stop input is missing its session or transcript")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	defer watchStopReason(home, os.Getpid()) // consume update-stop markers on cancellation too
	project, err := s.projectForCWD(bounded, db)
	if err != nil {
		return err
	}
	if err := s.requireLead(bounded, db, project); err != nil {
		return err
	}
	if project.Status == "down" {
		return nil
	}
	// A new User turn can finish while the preceding async hook still waits.
	// A process-owned flock excludes that overlap without an orphanable lease.
	lock, err := os.OpenFile(filepath.Join(home, "projects", project.Name, "claude-stop.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil
		}
		return err
	}
	destination := "claude:" + event.SessionID
	if err := reconcileClaudeReceipt(bounded, db, project.ID, destination, event.TranscriptPath); err != nil {
		return err
	}
	// The outer deadline also bounds preparation and PR maintenance, not only
	// the idle loop. Existing durable poll claims survive cancellation/restart.
	var result bytes.Buffer
	commandCtx := &axi.Context{Context: bounded, JSON: true, Out: &result, ErrOut: io.Discard}
	err = s.wait(commandCtx, []string{"--timeout", strconv.FormatInt(wait.Milliseconds(), 10), "--handoff", "--destination", destination})
	if ctx.Err() != nil {
		return nil
	}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var response struct {
		State    string `json:"state"`
		Wake     string `json:"wake"`
		Warning  string `json:"warning"`
		Delivery struct {
			ID string `json:"delivery_id"`
		} `json:"delivery"`
	}
	if err == nil {
		if err := json.Unmarshal(result.Bytes(), &response); err != nil {
			return err
		}
	}
	if response.State == "stopped" {
		return nil
	}
	message := "Posse Notice wait renewed. No Notice arrived. End this turn without tools or a User update; the next Stop hook owns the next bounded wait."
	if response.State == "uncertain" {
		marker := "Posse uncertain receipt: " + response.Delivery.ID
		present, transcriptErr := claudeTranscriptHasReceipt(bounded, event.TranscriptPath, marker)
		if present {
			return nil // Already surfaced: wait for explicit receipt resolution, not another model turn.
		}
		if transcriptErr != nil {
			// Without a readable transcript we cannot know whether the warning
			// was already shown. Bound repeated warnings to one wait interval.
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return nil
			case <-bounded.Done():
			case <-timer.C:
			}
		}
		message = marker + "\n" + response.Warning + "\nThe wait pauses after this reminder is recorded. Resolve the receipt explicitly, then end the turn to start the next Stop-owned wait."
	} else if response.Delivery.ID != "" {
		message = claudeReceiptMarker(response.Delivery.ID) + "\n" + response.Wake
	}
	// Exit 2 is the asyncRewake protocol. Keep the receipt printed until a
	// subsequent Stop observes this exact reminder in the persisted transcript.
	if _, err := fmt.Fprintln(output, message); err != nil {
		return err
	}
	return errClaudeRewake
}

var errClaudeRewake = errors.New("claude asyncRewake")

func claudeReceiptMarker(id string) string { return "Posse batch receipt: " + id }

func reconcileClaudeReceipt(ctx context.Context, db *store.DB, projectID int64, destination, transcript string) error {
	delivery, found, err := db.NoticeDeliveryForDestination(ctx, projectID, destination)
	if err != nil || !found || delivery.State != "printed" {
		return err
	}
	present, err := claudeTranscriptHasReceipt(ctx, transcript, claudeReceiptMarker(delivery.DeliveryID))
	outcome := "uncertain"
	if err == nil && present {
		outcome = "accepted"
	}
	// A missing or unreadable transcript is not proof of rejection. Never
	// repeat a possibly consumed Notice, including across replacement sessions.
	if delivery.LeaseUntil <= currentTime() {
		token, err := newDeliveryClaimToken()
		if err != nil {
			return err
		}
		renewed, claimed, err := db.ClaimNoticeDelivery(ctx, projectID, delivery.NoticeIDs, destination, delivery.Generation, token, currentTime(), noticeDeliveryLeaseTimeout.Milliseconds())
		if err != nil {
			return err
		}
		if !claimed {
			return store.ErrStateRace
		}
		delivery = renewed
	}
	return db.ResolveNoticeDelivery(ctx, projectID, delivery.DeliveryID, delivery.OwnerToken, outcome, currentTime())
}

func claudeTranscriptHasReceipt(ctx context.Context, path, marker string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return false, err
		}
		if entry.Type != "user" {
			continue
		}
		var text string
		if json.Unmarshal(entry.Message.Content, &text) == nil && strings.Contains(text, marker+"\n") {
			return true, nil
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(entry.Message.Content, &blocks) == nil {
			for _, block := range blocks {
				if block.Type == "text" && strings.Contains(block.Text, marker+"\n") {
					return true, nil
				}
			}
		}
	}
	return false, scanner.Err()
}

func (s *Service) writeLeadClaudeStop(home string, project store.Project) (string, error) {
	binary, err := os.Executable()
	if err != nil {
		return "", err
	}
	root := filepath.Join(home, "projects", project.Name, "lead-claude-watch")
	command := "cd " + shellQuote(project.Root) + " && exec env POSSE_HOME=" + shellQuote(home) + " " + shellQuote(binary) + " _claude-stop"
	settings := map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{
		"type": "command", "command": command, "timeout": 300, "asyncRewake": true,
	}}}}}}
	data, err := json.Marshal(settings)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, "hooks", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := writeFile(path, data); err != nil {
		return "", err
	}
	return root, nil
}
