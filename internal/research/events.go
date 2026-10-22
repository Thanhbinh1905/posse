package research

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
)

// Progress is what an adapter learned from its child's event stream.
type Progress struct {
	ChildID string // native thread or session identity, once announced
	Model   string
	Usage   Usage
	Result  string // the child's last complete answer
	Error   string // the child's last reported failure
}

// EventParser consumes one JSON line of a child's event stream at a time.
type EventParser interface {
	Consume(line []byte)
	Progress() Progress
}

// NewEventParser returns the stream parser for a process-launched harness.
func NewEventParser(harness Harness) EventParser {
	switch harness {
	case Codex:
		return &codexEvents{}
	case Pi:
		return &piEvents{}
	default:
		return nil
	}
}

// codexEvents reads `codex exec --json` events.
type codexEvents struct{ progress Progress }

func (p *codexEvents) Progress() Progress { return p.progress }

func (p *codexEvents) Consume(line []byte) {
	var event struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
		Message  string `json:"message"`
		Usage    *struct {
			InputTokens       int64 `json:"input_tokens"`
			CachedInputTokens int64 `json:"cached_input_tokens"`
			OutputTokens      int64 `json:"output_tokens"`
		} `json:"usage"`
		Item *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(line, &event) != nil {
		return
	}
	switch event.Type {
	case "thread.started":
		if event.ThreadID != "" {
			p.progress.ChildID = event.ThreadID
		}
	case "item.completed":
		if event.Item != nil && event.Item.Type == "agent_message" {
			p.progress.Result = event.Item.Text
		}
	case "turn.completed":
		if event.Usage != nil {
			p.progress.Usage = p.progress.Usage.Add(Usage{InputTokens: event.Usage.InputTokens, CacheReadTokens: event.Usage.CachedInputTokens, OutputTokens: event.Usage.OutputTokens, Turns: 1})
		}
	case "turn.failed":
		if event.Error != nil {
			p.progress.Error = event.Error.Message
		}
	case "error":
		p.progress.Error = event.Message
	}
}

// piEvents reads `pi --mode json` events.
type piEvents struct{ progress Progress }

func (p *piEvents) Progress() Progress { return p.progress }

func (p *piEvents) Consume(line []byte) {
	var event struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Message *struct {
			Role         string `json:"role"`
			Model        string `json:"model"`
			StopReason   string `json:"stopReason"`
			ErrorMessage string `json:"errorMessage"`
			Content      []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Usage *struct {
				Input      int64 `json:"input"`
				Output     int64 `json:"output"`
				CacheRead  int64 `json:"cacheRead"`
				CacheWrite int64 `json:"cacheWrite"`
				Cost       struct {
					Total float64 `json:"total"`
				} `json:"cost"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &event) != nil {
		return
	}
	switch event.Type {
	case "session":
		if event.ID != "" {
			p.progress.ChildID = event.ID
		}
	case "message_end":
		message := event.Message
		if message == nil || message.Role != "assistant" {
			return
		}
		if message.Model != "" {
			p.progress.Model = message.Model
		}
		if message.Usage != nil {
			p.progress.Usage = p.progress.Usage.Add(Usage{InputTokens: message.Usage.Input, OutputTokens: message.Usage.Output, CacheReadTokens: message.Usage.CacheRead, CacheWriteTokens: message.Usage.CacheWrite, CostUSD: message.Usage.Cost.Total, Turns: 1})
		}
		if message.StopReason == "error" || message.StopReason == "aborted" {
			p.progress.Error = strings.TrimSpace(message.ErrorMessage)
			if p.progress.Error == "" {
				p.progress.Error = "child stopped: " + message.StopReason
			}
			return
		}
		var text []string
		for _, part := range message.Content {
			if part.Type == "text" && part.Text != "" {
				text = append(text, part.Text)
			}
		}
		if len(text) > 0 {
			p.progress.Result = strings.Join(text, "\n")
		}
	}
}

// ClaudeTranscriptUsage sums assistant usage in a Claude Code transcript. The
// harness repeats a message's usage on each of its content lines, so the last
// line per message ID counts once.
func ClaudeTranscriptUsage(path string) (Usage, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return Usage{}, "", err
	}
	defer file.Close()
	return claudeUsage(file)
}

func claudeUsage(reader io.Reader) (Usage, string, error) {
	type entry struct {
		Type    string `json:"type"`
		Message *struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				InputTokens         int64 `json:"input_tokens"`
				OutputTokens        int64 `json:"output_tokens"`
				CacheReadTokens     int64 `json:"cache_read_input_tokens"`
				CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	byMessage := map[string]Usage{}
	order := []string{}
	model := ""
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for scanner.Scan() {
		var line entry
		if json.Unmarshal(scanner.Bytes(), &line) != nil || line.Type != "assistant" || line.Message == nil || line.Message.Usage == nil {
			continue
		}
		if line.Message.Model != "" {
			model = line.Message.Model
		}
		id := line.Message.ID
		if id == "" {
			id = "line-" + strconv.Itoa(len(order))
		}
		if _, seen := byMessage[id]; !seen {
			order = append(order, id)
		}
		usage := line.Message.Usage
		byMessage[id] = Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CacheReadTokens: usage.CacheReadTokens, CacheWriteTokens: usage.CacheCreationTokens, Turns: 1}
	}
	var total Usage
	for _, id := range order {
		total = total.Add(byMessage[id])
	}
	return total, model, scanner.Err()
}
