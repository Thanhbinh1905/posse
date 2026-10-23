// Package research defines bounded native research attempts: read-only child
// runs of a harness that Posse registers with their parent before it accepts
// any of their results. Adapters for each harness share these types; the store
// persists them and the app package wires them to hooks and processes.
package research

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Harness names a supported native research adapter.
type Harness string

const (
	Claude Harness = "claude"
	Codex  Harness = "codex"
	Pi     Harness = "pi"
)

// Harnesses lists every adapter in a stable order.
var Harnesses = []Harness{Claude, Codex, Pi}

func ParseHarness(value string) (Harness, error) {
	for _, harness := range Harnesses {
		if strings.EqualFold(strings.TrimSpace(value), string(harness)) {
			return harness, nil
		}
	}
	return "", fmt.Errorf("unsupported research harness %q; use claude, codex or pi", value)
}

// State is where an attempt is in its lifecycle.
type State string

const (
	// Registered: Posse admitted the child before it started; no identity yet
	// for harness-launched children.
	Registered State = "registered"
	// Running: the child identity is bound and results may be accepted.
	Running State = "running"
	// Completed: the child finished and its result was accepted.
	Completed State = "completed"
	// Interrupted: the child or its parent stopped before completion. The
	// attempt says whether its native session can be resumed.
	Interrupted State = "interrupted"
	// Cancelled: Posse cancelled the exact child; later output is not accepted.
	Cancelled State = "cancelled"
	// Failed: the child exited with an error or exceeded a bound.
	Failed State = "failed"
	// Refused: admission or verification refused the child. Nothing ran, or
	// what ran was stopped before its result was accepted.
	Refused State = "refused"
)

// Active reports whether the attempt still counts against the parent's budget.
func (s State) Active() bool { return s == Registered || s == Running }

// Terminal reports whether no further transition except resume is possible.
func (s State) Terminal() bool { return !s.Active() && s != Interrupted }

// Access is the effect scope of a child. Only read-only is supported.
const AccessReadOnly = "read-only"

// ParentRole is who launched the child.
const (
	ParentLead   = "lead"
	ParentWorker = "worker"
)

// Usage is what a child consumed, as its harness reported it.
type Usage struct {
	InputTokens      int64   `json:"input_tokens,omitempty"`
	OutputTokens     int64   `json:"output_tokens,omitempty"`
	CacheReadTokens  int64   `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64   `json:"cache_write_tokens,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
	Turns            int     `json:"turns,omitempty"`
}

func (u Usage) Add(other Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens + other.InputTokens,
		OutputTokens:     u.OutputTokens + other.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens + other.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + other.CacheWriteTokens,
		CostUSD:          u.CostUSD + other.CostUSD,
		Turns:            u.Turns + other.Turns,
	}
}

func (u Usage) JSON() string {
	data, _ := json.Marshal(u)
	return string(data)
}

func ParseUsage(value string) Usage {
	var usage Usage
	_ = json.Unmarshal([]byte(value), &usage)
	return usage
}

// Summary renders usage in one short line for list output.
func (u Usage) Summary() string {
	parts := []string{}
	if u.Turns > 0 {
		parts = append(parts, fmt.Sprintf("turns=%d", u.Turns))
	}
	if u.InputTokens > 0 || u.OutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("in=%d out=%d", u.InputTokens, u.OutputTokens))
	}
	if u.CacheReadTokens > 0 || u.CacheWriteTokens > 0 {
		parts = append(parts, fmt.Sprintf("cache_read=%d cache_write=%d", u.CacheReadTokens, u.CacheWriteTokens))
	}
	if u.CostUSD > 0 {
		parts = append(parts, fmt.Sprintf("cost=$%.4f", u.CostUSD))
	}
	if len(parts) == 0 {
		return "unreported"
	}
	return strings.Join(parts, " ")
}

// AttemptID renders a stored attempt ID as the CLI reference r<id>.
func AttemptID(id int64) string { return "r" + strconv.FormatInt(id, 10) }

// ParseAttemptID accepts r<id> or a bare positive number.
func ParseAttemptID(value string) (int64, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(value), "r")
	id, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("research attempt %q is not r<number>", value)
	}
	return id, nil
}
