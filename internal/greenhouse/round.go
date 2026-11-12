package greenhouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/atomicfile"
)

// Settings selects the rules and reviewer passed to Greenhouse `apply`. Empty
// fields take the daemon's defaults.
type Settings struct {
	Rules           []string
	ReviewerBackend string
	ReviewerModel   string
}

// Round asks Greenhouse for one exact-head review of a Task Member.
type Round struct {
	Worktree string
	Host     Host
	// GrantPrefix is "<project>/t<seq>/<member>"; round n uses GrantPrefix/r<n>.
	GrantPrefix string
	Intent      string
	Target      string
	Settings    Settings
	// Pending is the write-ahead record of the review request in flight, so a
	// restarted Posse replays the same request instead of spending a new grant.
	Pending string
	Wait    time.Duration
	// Checkpoint, when set, runs at named steps so tests can stop Posse there.
	Checkpoint func(step string)
}

type Outcome string

const (
	// Settled: the grant has a verdict; Result.Current says whether it still binds the head.
	Settled Outcome = "settled"
	// Stale: the head moved, so the result binds no head.
	Stale Outcome = "stale"
	// Paused: a backend pause; the same grant resumes later.
	Paused Outcome = "paused"
	// RecoveryRequired: the run parked after an interruption; no new grant until it is resolved.
	RecoveryRequired Outcome = "recovery-required"
	// Running: the wait ended before the review settled.
	Running Outcome = "running"
)

type RoundResult struct {
	Outcome  Outcome
	Grant    string
	RunID    string
	Result   *HostResult
	Recovery *Recovery
	// Replayed is true when the review request was replayed for its receipt.
	Replayed bool
}

type pendingReview struct {
	Grant   string          `json:"grant"`
	Request json.RawMessage `json:"request"`
}

// Review continues the Member's round in flight, or starts the next one.
func (d *Daemon) Review(ctx context.Context, round Round) (RoundResult, error) {
	if _, err := d.Ensure(ctx); err != nil {
		return RoundResult{}, err
	}
	v, err := d.attach(ctx, round)
	if err != nil {
		return RoundResult{}, err
	}
	pending, err := readPending(round.Pending)
	if err != nil {
		return RoundResult{}, err
	}
	if v.Run != nil && v.Run.Recovery != nil {
		grant := ""
		if pending != nil {
			grant = pending.Grant
		}
		return RoundResult{Outcome: RecoveryRequired, Grant: grant, RunID: v.RunID, Recovery: v.Run.Recovery}, nil
	}
	if pending != nil {
		return d.continueReview(ctx, round, v, *pending)
	}
	return d.startReview(ctx, round, v)
}

// ClearPending ends the round in flight once Posse has acted on its result.
func ClearPending(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (d *Daemon) continueReview(ctx context.Context, round Round, v response, pending pendingReview) (RoundResult, error) {
	delivered := v.HostResult != nil && v.HostResult.Grant == pending.Grant
	replay, err := d.axi(ctx, round.Worktree, pending.Request)
	if err != nil {
		return RoundResult{}, err
	}
	if replay.Outcome != "ok" && !delivered {
		// The request never reached the daemon and its envelope is stale.
		if err := ClearPending(round.Pending); err != nil {
			return RoundResult{}, err
		}
		return d.startReview(ctx, round, v)
	}
	if delivered && v.HostResult.Status == "paused" {
		if _, err := d.call(ctx, round, v, pending.Grant, "resume", nil); err != nil {
			return RoundResult{}, err
		}
	}
	result, err := d.wait(ctx, round, pending.Grant)
	result.Replayed = delivered && replay.Outcome == "ok"
	return result, err
}

func (d *Daemon) startReview(ctx context.Context, round Round, v response) (RoundResult, error) {
	used := 0
	if v.Run != nil {
		used = len(v.Run.HostGrants)
	}
	grant := fmt.Sprintf("%s/r%d", round.GrantPrefix, used+1)
	head, err := worktreeHead(ctx, round.Worktree)
	if err != nil {
		return RoundResult{}, err
	}
	if v.HostResult != nil || v.Run == nil || v.Run.Candidate != head {
		if v, err = d.call(ctx, round, v, grant, "new", nil); err != nil {
			return RoundResult{}, err
		}
	}
	selected := round.Settings
	if v.Defaults != nil {
		if len(selected.Rules) == 0 {
			selected.Rules = v.Defaults.Rules
		}
		if selected.ReviewerBackend == "" {
			selected.ReviewerBackend, selected.ReviewerModel = v.Defaults.ReviewerBackend, v.Defaults.ReviewerModel
		}
	}
	// Host mode ignores the limit, but apply refuses one below consumed usage.
	limit := 1
	if v.Run != nil {
		limit += v.Run.ReviewUsed
	}
	settings := map[string]any{
		"mode": "ask", "preset": "one-pass", "limit": limit, "rules": selected.Rules,
		"reviewer_backend": selected.ReviewerBackend, "reviewer_model": selected.ReviewerModel,
		"fixer_backend": selected.ReviewerBackend, "fixer_model": selected.ReviewerModel,
	}
	if v, err = d.call(ctx, round, v, grant, "apply", map[string]any{"settings": settings}); err != nil {
		return RoundResult{}, err
	}
	if v, err = d.call(ctx, round, v, grant, "resolve-rules", nil); err != nil {
		return RoundResult{}, err
	}
	if !v.RulesReady {
		return RoundResult{}, fmt.Errorf("greenhouse rules %v did not load", selected.Rules)
	}
	if v, err = d.call(ctx, round, v, grant, "prepare", map[string]any{"intent": round.Intent, "target": round.Target}); err != nil {
		return RoundResult{}, err
	}
	request, err := json.Marshal(d.request(round, v, grant, "review", map[string]any{"grant": grant}))
	if err != nil {
		return RoundResult{}, err
	}
	record, err := json.Marshal(pendingReview{Grant: grant, Request: request})
	if err != nil {
		return RoundResult{}, err
	}
	if err := atomicfile.Write(round.Pending, record, 0o600); err != nil {
		return RoundResult{}, err
	}
	reviewed, err := d.axi(ctx, round.Worktree, request)
	if err != nil {
		return RoundResult{}, err
	}
	if err := reviewed.refusal("review"); err != nil {
		_ = ClearPending(round.Pending)
		return RoundResult{}, err
	}
	if round.Checkpoint != nil {
		round.Checkpoint("review-sent")
	}
	return d.wait(ctx, round, grant)
}

// wait follows the run until the grant settles, pauses, parks or goes stale.
// A daemon that disappears is started again and followed from its store.
func (d *Daemon) wait(ctx context.Context, round Round, grant string) (RoundResult, error) {
	deadline := time.Now().Add(round.Wait)
	stream, sequence := "", uint64(0)
	for {
		v, err := d.attach(ctx, round)
		if errors.Is(err, errUnavailable) {
			if _, err := d.Ensure(ctx); err != nil {
				return RoundResult{}, err
			}
			continue
		}
		if err != nil {
			return RoundResult{}, err
		}
		result := RoundResult{Grant: grant, RunID: v.RunID, Result: v.HostResult}
		if v.Run != nil && v.Run.Recovery != nil {
			result.Outcome, result.Recovery = RecoveryRequired, v.Run.Recovery
			return result, nil
		}
		if hr := v.HostResult; hr != nil && hr.Grant == grant {
			switch {
			case !hr.Current && hr.Status == "running":
				// Greenhouse refuses a successor while the stale review runs. A
				// refused cancel means the review settled meanwhile.
				request, _ := json.Marshal(d.request(round, v, grant, "cancel", nil))
				if _, err := d.axi(ctx, round.Worktree, request); err != nil && !errors.Is(err, errUnavailable) {
					return RoundResult{}, err
				}
				continue
			case !hr.Current && hr.Status == "cancelling":
			case !hr.Current:
				result.Outcome = Stale
				return result, nil
			case hr.Verdict != "":
				result.Outcome = Settled
				return result, nil
			case hr.Status == "paused":
				result.Outcome = Paused
				return result, nil
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			result.Outcome = Running
			return result, nil
		}
		if stream == "" {
			stream, sequence = v.Stream, v.Sequence
		}
		wait := min(remaining, 2*time.Second)
		request, _ := json.Marshal(map[string]any{"version": Protocol, "op": "watch", "stream": stream, "after": sequence, "wait_ms": wait.Milliseconds()})
		watched, err := d.axi(ctx, round.Worktree, request)
		if err != nil || watched.Stream == "" {
			time.Sleep(100 * time.Millisecond)
			stream = ""
			continue
		}
		stream, sequence = watched.Stream, watched.Sequence
	}
}

func (d *Daemon) attach(ctx context.Context, round Round) (response, error) {
	request, _ := json.Marshal(map[string]any{"version": Protocol, "op": "attach", "host": round.Host})
	v, err := d.axi(ctx, round.Worktree, request)
	if err != nil {
		return v, err
	}
	return v, v.refusal("attach")
}

// call sends one host mutation. Its action ID names the grant, the operation
// and the run token it expects, so an identical retry replays its receipt.
func (d *Daemon) call(ctx context.Context, round Round, v response, grant, op string, extra map[string]any) (response, error) {
	request, err := json.Marshal(d.request(round, v, grant, op, extra))
	if err != nil {
		return v, err
	}
	next, err := d.axi(ctx, round.Worktree, request)
	if err != nil {
		return next, err
	}
	return next, next.refusal(op)
}

func (d *Daemon) request(round Round, v response, grant, op string, extra map[string]any) map[string]any {
	q := map[string]any{
		"version": Protocol, "op": op, "host": round.Host,
		"run_id": v.RunID, "input_id": v.InputID, "gate_id": v.GateID, "expected": v.Token,
		"action_id": grant + "/" + op + "/" + v.Token, "reason": "Posse host review " + grant,
	}
	for key, value := range extra {
		q[key] = value
	}
	return q
}

func readPending(path string) (*pendingReview, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pending pendingReview
	if err := json.Unmarshal(data, &pending); err != nil || pending.Grant == "" || len(pending.Request) == 0 {
		return nil, fmt.Errorf("unreadable Greenhouse round record %s", path)
	}
	return &pending, nil
}

func worktreeHead(ctx context.Context, worktree string) (string, error) {
	output, err := exec.CommandContext(ctx, "git", "-C", worktree, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("read the head of %s: %w", worktree, err)
	}
	return strings.TrimSpace(string(output)), nil
}
