package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/sessionref"
	"github.com/thanhbinh1905/posse/internal/store"
)

const codexInterruptRejected = "unsupported websocket request type: response.interrupt"
const codexRolloutTailBytes = 2 << 20

func observeCodexIdleFailure(ctx context.Context, db *store.DB, task store.Task, pane herdr.Pane, idleSince int64, now time.Time) (*store.Notice, bool, error) {
	if pane.Agent != "codex" || pane.PaneID != task.PaneID {
		return nil, false, nil
	}
	messageID, submittedAt, err := db.CurrentSteerableInstructionSubmission(ctx, task.ID)
	if err != nil || messageID == 0 {
		return nil, false, err
	}
	reportedIdleSince, err := db.CodexInterruptRejectedNoticeIdleSince(ctx, task.ID, messageID)
	if err != nil || reportedIdleSince != 0 {
		return nil, reportedIdleSince == idleSince, err
	}
	session := sessionref.Sanitize("codex", string(pane.AgentSession))
	if session == "" {
		return nil, false, nil
	}
	turn := latestCodexInterruptFailure(session)
	at := time.UnixMilli(submittedAt)
	if turn.id == "" || turn.started.After(at) || turn.completed.Before(at) || turn.completed.After(now) {
		return nil, false, nil
	}
	summary := fmt.Sprintf("%s is idle without a Signal: Codex recorded a failed turn (HTTP 400: %s). Herdr accepted instruction #%d; execution is unconfirmed. Inspect `posse peek t%d` before resending.", task.Title, codexInterruptRejected, messageID, task.Seq)
	notice, err := db.CreateCodexInterruptRejectedNotice(ctx, task, messageID, idleSince, turn.id, summary, now)
	return notice, err == nil, err
}

type codexTurnFailure struct {
	id                 string
	started, completed time.Time
}

func latestCodexInterruptFailure(session string) codexTurnFailure {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		home = ".codex"
	}
	if !filepath.IsAbs(home) {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return codexTurnFailure{}
		}
		home = filepath.Join(userHome, home)
	}
	sessions := filepath.Join(home, "sessions")
	root, err := os.OpenRoot(sessions)
	if err != nil {
		return codexTurnFailure{}
	}
	defer root.Close()
	path, err := codexRolloutPath(root, sessions, session)
	if err != nil || path == "" {
		return codexTurnFailure{}
	}
	info, err := root.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return codexTurnFailure{}
	}
	file, err := root.Open(path)
	if err != nil {
		return codexTurnFailure{}
	}
	defer file.Close()
	start := max(0, info.Size()-codexRolloutTailBytes)
	reader := bufio.NewReader(io.NewSectionReader(file, start, info.Size()-start))
	if start > 0 {
		if _, err := reader.ReadBytes('\n'); err != nil {
			return codexTurnFailure{}
		}
	}
	return parseCodexInterruptFailure(reader)
}

func codexRolloutPath(root *os.Root, sessions, session string) (string, error) {
	if filepath.IsAbs(session) {
		return filepath.Rel(sessions, session)
	}
	matches, err := fs.Glob(root.FS(), "*/*/*/rollout-*-"+session+".jsonl")
	if err != nil || len(matches) != 1 {
		return "", err
	}
	return matches[0], nil
}

func parseCodexInterruptFailure(reader io.Reader) codexTurnFailure {
	var active, failed codexTurnFailure
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 16<<10), codexRolloutTailBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.Contains(line, []byte(`"event_msg"`)) {
			continue
		}
		var row struct {
			Type      string    `json:"type"`
			Timestamp time.Time `json:"timestamp"`
			Payload   struct {
				Type       string `json:"type"`
				TurnID     string `json:"turn_id"`
				RootTurnID string `json:"root_turn_id"`
				Error      *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &row) != nil || row.Type != "event_msg" {
			continue
		}
		event := row.Payload
		if event.RootTurnID != "" && event.RootTurnID != event.TurnID {
			continue
		}
		switch event.Type {
		case "task_started":
			active, failed = codexTurnFailure{id: event.TurnID, started: row.Timestamp}, codexTurnFailure{}
		case "task_complete", "turn_aborted":
			if event.TurnID != active.id && event.TurnID != "" && active.id != "" {
				continue
			}
			failed = codexTurnFailure{}
			if event.Type == "task_complete" && event.TurnID != "" && active.id == event.TurnID && !active.started.IsZero() && !row.Timestamp.Before(active.started) && event.Error != nil && codexRejectedInterrupt(event.Error.Message) {
				failed = active
				failed.completed = row.Timestamp
			}
			active = codexTurnFailure{}
		}
	}
	if scanner.Err() != nil {
		return codexTurnFailure{}
	}
	return failed
}

func codexRejectedInterrupt(message string) bool {
	var providerError struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
		Error  struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	return json.Unmarshal([]byte(message), &providerError) == nil && providerError.Type == "error" && providerError.Status == 400 && providerError.Error.Message == codexInterruptRejected
}
