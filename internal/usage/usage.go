// Package usage reads token counts from recorded harness sessions.
package usage

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Counts contains token totals. Known is false unless the session contains a
// supported usage record.
type Counts struct {
	Input      int64 `json:"input_tokens,omitempty"`
	Output     int64 `json:"output_tokens,omitempty"`
	CacheRead  int64 `json:"cache_read_tokens,omitempty"`
	CacheWrite int64 `json:"cache_write_tokens,omitempty"`
	Known      bool  `json:"known"`
}

// Read resolves the recorded session reference inside the selected harness's
// session store and reads that session only. cwd identifies Claude's project
// directory without scanning the session store.
func Read(kind, session, cwd string) Counts {
	kind = strings.ToLower(kind)
	switch kind {
	case "claude-code":
		kind = "claude"
	case "codex-cli":
		kind = "codex"
	}
	if session == "" {
		return Counts{}
	}
	path := session
	if !filepath.IsAbs(path) {
		path = sessionFile(kind, session, cwd)
	}
	if path == "" {
		return Counts{}
	}
	file, err := os.Open(path)
	if err != nil {
		return Counts{}
	}
	defer file.Close()
	counts, err := parse(kind, file)
	if err != nil {
		return Counts{}
	}
	return counts
}

func sessionFile(kind, id, cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	var root string
	switch kind {
	case "claude":
		root = os.Getenv("CLAUDE_CONFIG_DIR")
		if root == "" {
			root = filepath.Join(home, ".claude")
		}
		if cwd == "" {
			return ""
		}
		project := strings.ReplaceAll(filepath.Clean(cwd), string(os.PathSeparator), "-")
		return filepath.Join(root, "projects", project, id+".jsonl")
	case "codex":
		root = os.Getenv("CODEX_HOME")
		if root == "" {
			root = filepath.Join(home, ".codex")
		}
	case "pi":
		root = os.Getenv("PI_CODING_AGENT_DIR")
		if root == "" {
			root = filepath.Join(home, ".pi", "agent")
		}
		pattern := filepath.Join(root, "sessions", "*_"+id+".jsonl")
		matches, _ := filepath.Glob(pattern)
		if len(matches) == 1 {
			return matches[0]
		}
		return ""
	default:
		return ""
	}
	pattern := filepath.Join(root, "sessions", "*", "*", "*", "rollout-*-"+id+".jsonl")
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

func parse(kind string, file *os.File) (Counts, error) {
	var counts Counts
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 32<<20)
	claudeMessages := map[string]Counts{}
	claudeOrder := []string{}
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Bytes()
		switch kind {
		case "claude":
			var row struct {
				Type    string `json:"type"`
				Message *struct {
					ID    string `json:"id"`
					Usage *struct {
						Input  int64 `json:"input_tokens"`
						Output int64 `json:"output_tokens"`
						Read   int64 `json:"cache_read_input_tokens"`
						Write  int64 `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &row) == nil && row.Type == "assistant" && row.Message != nil && row.Message.Usage != nil {
				id := row.Message.ID
				if id == "" {
					id = stringInt(lineNumber)
				}
				if _, exists := claudeMessages[id]; !exists {
					claudeOrder = append(claudeOrder, id)
				}
				u := row.Message.Usage
				claudeMessages[id] = Counts{Input: u.Input, Output: u.Output, CacheRead: u.Read, CacheWrite: u.Write, Known: true}
			}
		case "codex":
			var row struct {
				Type    string `json:"type"`
				Payload struct {
					Type string `json:"type"`
					Info *struct {
						Total *struct {
							Input      int64 `json:"input_tokens"`
							Cached     int64 `json:"cached_input_tokens"`
							CacheWrite int64 `json:"cache_write_input_tokens"`
							Output     int64 `json:"output_tokens"`
						} `json:"total_token_usage"`
					} `json:"info"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &row) == nil && row.Type == "event_msg" && row.Payload.Type == "token_count" && row.Payload.Info != nil && row.Payload.Info.Total != nil {
				u := row.Payload.Info.Total
				counts = Counts{Input: u.Input, Output: u.Output, CacheRead: u.Cached, CacheWrite: u.CacheWrite, Known: true}
			}
		case "pi":
			var row struct {
				Type    string `json:"type"`
				Message *struct {
					Role  string `json:"role"`
					Usage *struct {
						Input  int64 `json:"input"`
						Output int64 `json:"output"`
						Read   int64 `json:"cacheRead"`
						Write  int64 `json:"cacheWrite"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &row) == nil && row.Type == "message" && row.Message != nil && row.Message.Role == "assistant" && row.Message.Usage != nil {
				u := row.Message.Usage
				counts.Input += u.Input
				counts.Output += u.Output
				counts.CacheRead += u.Read
				counts.CacheWrite += u.Write
				counts.Known = true
			}
		default:
			return Counts{}, nil
		}
	}
	if kind == "claude" {
		for _, id := range claudeOrder {
			u := claudeMessages[id]
			counts.Input += u.Input
			counts.Output += u.Output
			counts.CacheRead += u.CacheRead
			counts.CacheWrite += u.CacheWrite
			counts.Known = true
		}
	}
	return counts, scanner.Err()
}

func stringInt(value int) string {
	return strconv.Itoa(value)
}
