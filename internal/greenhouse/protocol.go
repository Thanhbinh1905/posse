// Package greenhouse is Posse's host adapter for Greenhouse review in host mode.
// It speaks AXI protocol version 2 to `greenhouse axi`, one request per
// invocation, against a daemon that Posse starts and places itself.
package greenhouse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Protocol is the only AXI envelope version the adapter speaks. It never
// downgrades: host mode does not exist below version 2.
const Protocol = 2

// Range is an inclusive protocol version range.
type Range struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

func (r Range) Includes(version int) bool { return r.Min <= version && version <= r.Max }

func (r Range) String() string {
	if r.Min == r.Max {
		return fmt.Sprint(r.Min)
	}
	return fmt.Sprintf("%d-%d", r.Min, r.Max)
}

// PosseRange is the protocol range this adapter accepts.
var PosseRange = Range{Min: Protocol, Max: Protocol}

// ProtocolError refuses a daemon whose range excludes the adapter's version.
type ProtocolError struct {
	Daemon Range
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("the Greenhouse daemon speaks protocol %s and Posse needs protocol %s; Posse does not downgrade. Let its work settle, stop it and install a Greenhouse that speaks protocol %d", e.Daemon, PosseRange, Protocol)
}

// Host identifies the Posse host run, one per Task Member.
type Host struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// Finding is one Greenhouse finding as host_result reports it.
type Finding struct {
	ID          string `json:"id"`
	Priority    string `json:"priority"`
	Origin      string `json:"origin"`
	File        string `json:"file"`
	Start       int    `json:"start"`
	End         int    `json:"end"`
	Explanation string `json:"explanation"`
	Action      string `json:"action"`
	Rule        string `json:"rule"`
	Head        string `json:"head"`
}

// HostResult is the exact-head review result of a host run.
type HostResult struct {
	Grant        string    `json:"grant"`
	ReviewID     string    `json:"review_id"`
	Head         string    `json:"head"`
	Target       string    `json:"target"`
	ReviewBase   string    `json:"review_base"`
	Status       string    `json:"status"`
	Verdict      string    `json:"verdict"`
	Current      bool      `json:"current"`
	Findings     []Finding `json:"findings"`
	RulesDigest  string    `json:"rules_digest"`
	ConfigDigest string    `json:"config_digest"`
	InputDigest  string    `json:"input_digest"`
	Backend      string    `json:"backend"`
	Model        string    `json:"model"`
	Reason       string    `json:"reason"`
}

// Recovery is set while a run waits for recovery after an interruption.
type Recovery struct {
	ID      string   `json:"id"`
	Reason  string   `json:"reason"`
	Options []string `json:"options"`
}

type run struct {
	ID         string    `json:"id"`
	Status     string    `json:"status"`
	Candidate  string    `json:"candidate"`
	HostGrants []string  `json:"host_grants"`
	ReviewUsed int       `json:"review_used"`
	Recovery   *Recovery `json:"recovery"`
}

type defaults struct {
	Rules           []string `json:"rules"`
	ReviewerBackend string   `json:"reviewer_backend"`
	ReviewerModel   string   `json:"reviewer_model"`
}

type response struct {
	Version    int         `json:"version"`
	Outcome    string      `json:"outcome"`
	Error      string      `json:"error"`
	RunID      string      `json:"run_id"`
	InputID    string      `json:"input_id"`
	GateID     string      `json:"gate_id"`
	Token      string      `json:"token"`
	RulesReady bool        `json:"rules_ready"`
	Run        *run        `json:"run"`
	Defaults   *defaults   `json:"defaults"`
	HostResult *HostResult `json:"host_result"`
	Protocol   *Range      `json:"protocol"`
	Stream     string      `json:"stream"`
	Sequence   uint64      `json:"sequence"`
}

func (v response) refusal(op string) error {
	if v.Outcome == "ok" {
		return nil
	}
	if v.Protocol != nil && !v.Protocol.Includes(Protocol) {
		return &ProtocolError{Daemon: *v.Protocol}
	}
	return fmt.Errorf("greenhouse %s %s: %s", op, v.Outcome, v.Error)
}

var errUnavailable = errors.New("greenhouse daemon unavailable")

func (d *Daemon) axi(ctx context.Context, dir string, request []byte) (response, error) {
	command := exec.CommandContext(ctx, d.Binary, "axi")
	command.Dir = dir
	command.Env = d.environment()
	command.Stdin = bytes.NewReader(request)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	runErr := command.Run()
	var v response
	if err := json.Unmarshal(stdout.Bytes(), &v); err != nil {
		if runErr != nil {
			return v, fmt.Errorf("greenhouse axi: %w: %s", runErr, strings.TrimSpace(stderr.String()))
		}
		return v, fmt.Errorf("greenhouse axi returned no envelope: %w", err)
	}
	if v.Outcome == "error" && strings.Contains(v.Error, "daemon unavailable") {
		return v, fmt.Errorf("%w: %s", errUnavailable, v.Error)
	}
	if v.Outcome == "error" {
		return v, fmt.Errorf("greenhouse axi error: %s", v.Error)
	}
	return v, nil
}
