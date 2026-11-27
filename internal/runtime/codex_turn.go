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

func codexIdleFailure(ctx context.Context, db *store.DB, task store.Task, pane herdr.Pane, now time.Time) (string, error) {
	if pane.Agent != "codex" || pane.PaneID != task.PaneID {
		return "", nil
	}
	session := sessionref.Sanitize("codex", string(pane.AgentSession))
	if session == "" || filepath.IsAbs(session) {
		return "", nil
	}
	messageID, submittedAt, err := db.CurrentSteerableInstructionSubmission(ctx, task.ID)
	if err != nil || messageID == 0 {
		return "", err
	}
	turn := latestCodexInterruptFailure(session)
	at := time.UnixMilli(submittedAt)
	if turn.id == "" || turn.started.After(at) || turn.completed.Before(at) || turn.completed.After(now) {
		return "", nil
	}
	return fmt.Sprintf("%s is idle without a Signal: Codex recorded a failed turn (HTTP 400: %s). Herdr accepted instruction #%d; execution is unconfirmed. Inspect `posse peek t%d` before resending.", task.Title, codexInterruptRejected, messageID, task.Seq), nil
}

type codexTurnFailure struct {
	id                 string
	started, completed time.Time
}

func latestCodexInterruptFailure(session string) codexTurnFailure {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return codexTurnFailure{}
		}
		home = filepath.Join(userHome, ".codex")
	}
	root, err := os.OpenRoot(filepath.Join(home, "sessions"))
	if err != nil {
		return codexTurnFailure{}
	}
	defer root.Close()
	matches, err := fs.Glob(root.FS(), "*/*/*/rollout-*-"+session+".jsonl")
	if err != nil || len(matches) != 1 {
		return codexTurnFailure{}
	}
	info, err := root.Lstat(matches[0])
	if err != nil || !info.Mode().IsRegular() {
		return codexTurnFailure{}
	}
	file, err := root.Open(matches[0])
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
