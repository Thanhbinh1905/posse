// Package usage reads token counts from recorded harness sessions.
package usage

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Counts contains token totals. Known distinguishes a readable session from an
// unsupported kind, missing session, or unavailable session file.
type Counts struct {
	Input      int64 `json:"input_tokens,omitempty"`
	Output     int64 `json:"output_tokens,omitempty"`
	CacheRead  int64 `json:"cache_read_tokens,omitempty"`
	CacheWrite int64 `json:"cache_write_tokens,omitempty"`
	Known      bool  `json:"known"`
}

// Read resolves the recorded session reference inside the selected harness's
// session store and reads that session only.
func Read(kind, session string) Counts {
	kind = strings.ToLower(kind)
	if kind == "claude-code" {
		kind = "claude"
	} else if kind == "codex-cli" {
		kind = "codex"
	}
	if session == "" {
		return Counts{}
	}
	path := session
	if !filepath.IsAbs(path) {
		matches := sessionFiles(kind, session)
		if len(matches) != 1 {
			return Counts{}
		}
		path = matches[0]
	}
	file, err := os.Open(path)
	if err != nil {
		return Counts{}
	}
	defer file.Close()
	counts, err := parse(kind, file)
	if err != nil || !counts.Known {
		return Counts{}
	}
	return counts
}

func sessionFiles(kind, id string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var root string
	switch kind {
	case "claude", "claude-code":
		root = os.Getenv("CLAUDE_CONFIG_DIR")
		if root == "" {
			root = filepath.Join(home, ".claude")
		}
		root = filepath.Join(root, "projects")
	case "codex", "codex-cli":
		root = os.Getenv("CODEX_HOME")
		if root == "" {
			root = filepath.Join(home, ".codex")
		}
		root = filepath.Join(root, "sessions")
	case "pi":
		root = os.Getenv("PI_CODING_AGENT_DIR")
		if root == "" {
			root = filepath.Join(home, ".pi", "agent")
		}
		root = filepath.Join(root, "sessions")
	default:
		return nil
	}
	var matches []string
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if kind == "pi" {
			if strings.HasSuffix(base, "_"+id+".jsonl") {
				matches = append(matches, path)
			}
		} else if base == id+".jsonl" || kind == "codex" && strings.HasSuffix(base, "-"+id+".jsonl") {
			matches = append(matches, path)
		}
		return nil
	})
	sort.Strings(matches)
	return matches
}

func parse(kind string, file *os.File) (Counts, error) {
	counts := Counts{Known: true}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 32<<20)
	claudeMessages := map[string]Counts{}
	claudeOrder := []string{}
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Bytes()
		switch kind {
		case "claude", "claude-code":
			var row struct {
				Type    string `json:"type"`
				Message struct {
					ID    string `json:"id"`
					Usage struct {
						Input  int64 `json:"input_tokens"`
						Output int64 `json:"output_tokens"`
						Read   int64 `json:"cache_read_input_tokens"`
						Write  int64 `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &row) == nil && row.Type == "assistant" {
				id := row.Message.ID
				if id == "" {
					id = strconv.Itoa(lineNumber)
				}
				if _, exists := claudeMessages[id]; !exists {
					claudeOrder = append(claudeOrder, id)
				}
				claudeMessages[id] = Counts{Input: row.Message.Usage.Input, Output: row.Message.Usage.Output, CacheRead: row.Message.Usage.Read, CacheWrite: row.Message.Usage.Write, Known: true}
			}
		case "codex", "codex-cli":
			var row struct {
				Type  string `json:"type"`
				Usage struct {
					Input  int64 `json:"input_tokens"`
					Cached int64 `json:"cached_input_tokens"`
					Output int64 `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(line, &row) == nil && row.Type == "turn.completed" {
				counts.Input += row.Usage.Input
				counts.CacheRead += row.Usage.Cached
				counts.Output += row.Usage.Output
			}
		case "pi":
			var row struct {
				Type    string `json:"type"`
				Message struct {
					Role  string `json:"role"`
					Usage struct {
						Input  int64 `json:"input"`
						Output int64 `json:"output"`
						Read   int64 `json:"cacheRead"`
						Write  int64 `json:"cacheWrite"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &row) == nil && row.Type == "message_end" && row.Message.Role == "assistant" {
				counts.Input += row.Message.Usage.Input
				counts.Output += row.Message.Usage.Output
				counts.CacheRead += row.Message.Usage.Read
				counts.CacheWrite += row.Message.Usage.Write
			}
		default:
			return Counts{}, nil
		}
	}
	if kind == "claude" || kind == "claude-code" {
		for _, id := range claudeOrder {
			message := claudeMessages[id]
			counts.Input += message.Input
			counts.Output += message.Output
			counts.CacheRead += message.CacheRead
			counts.CacheWrite += message.CacheWrite
		}
	}
	return counts, scanner.Err()
}
