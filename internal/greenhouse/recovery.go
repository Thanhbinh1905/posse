package greenhouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

var ErrRecoveryNotReady = errors.New("wait for the interrupted executor to exit before retrying recovery")

type RecoveredReview struct {
	Grant  string
	RunID  string
	Status string
	Detail string
}

func (d *Daemon) RecoverReview(ctx context.Context, round Round, head, recoveryID string) (RecoveredReview, error) {
	pending, err := readPending(round.Pending)
	if err != nil {
		return RecoveredReview{}, err
	}
	if pending == nil || !strings.HasPrefix(pending.Grant, round.Host.ID+"/r") {
		return RecoveredReview{}, fmt.Errorf("no pending Greenhouse grant for this Task Member")
	}
	var request struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(pending.Request, &request); err != nil || request.RunID == "" {
		return RecoveredReview{}, fmt.Errorf("pending Greenhouse grant does not identify a run")
	}
	if _, err := d.Ensure(ctx); err != nil {
		return RecoveredReview{}, err
	}
	v, err := d.axi(ctx, round.Worktree, []byte(`{"version":2,"op":"inspect"}`))
	if err != nil {
		return RecoveredReview{}, err
	}
	if err := v.refusal("inspect"); err != nil {
		return RecoveredReview{}, err
	}
	if v.Run == nil || v.Run.Host == nil || *v.Run.Host != round.Host || v.RunID != request.RunID || v.Run.Candidate != head ||
		v.HostResult == nil || !v.HostResult.Current || v.HostResult.Head != head || v.HostResult.Grant != pending.Grant {
		return RecoveredReview{}, fmt.Errorf("the Greenhouse run does not match this Task Member's pending grant and exact head")
	}
	const choice = "discard"
	action := pending.Grant + "/recover/" + recoveryID
	if v.Run.Recovery == nil {
		if recovered, ok := recordedRecovery(v, pending.Grant, recoveryID, action); ok {
			return recovered, nil
		}
	}
	if v.Run.Status != string(RecoveryRequired) || v.Run.Recovery == nil || v.Run.Recovery.ID != recoveryID || v.Run.Recovery.Kind != "review" {
		return RecoveredReview{}, fmt.Errorf("no matching recovery-required Greenhouse review")
	}
	if !slices.Contains(v.Run.Recovery.Options, choice) {
		return RecoveredReview{}, ErrRecoveryNotReady
	}
	v, err = d.call(ctx, round, v, pending.Grant, "recover", map[string]any{"choice": choice, "action_id": action})
	if err != nil {
		return RecoveredReview{}, err
	}
	if recovered, ok := recordedRecovery(v, pending.Grant, recoveryID, action); ok {
		return recovered, nil
	}
	return RecoveredReview{}, fmt.Errorf("missing Greenhouse outcome for recovery %s", recoveryID)
}

func (d *Daemon) FollowReview(ctx context.Context, round Round, grant string) (RoundResult, error) {
	if _, err := d.Ensure(ctx); err != nil {
		return RoundResult{}, err
	}
	v, err := d.axi(ctx, round.Worktree, []byte(`{"version":2,"op":"inspect"}`))
	if err != nil {
		return RoundResult{}, err
	}
	if err := v.refusal("inspect"); err != nil {
		return RoundResult{}, err
	}
	if v.Run == nil || v.Run.Host == nil || *v.Run.Host != round.Host || v.HostResult == nil || v.HostResult.Grant != grant {
		return RoundResult{Grant: grant}, fmt.Errorf("no restarted Greenhouse review for grant %s", grant)
	}
	return d.wait(ctx, round, grant)
}

func recordedRecovery(v response, grant, id, action string) (RecoveredReview, bool) {
	if v.Run != nil {
		for _, recovery := range v.Run.Recoveries {
			if recovery.ID == id && recovery.Answer.Action == action {
				status := "restarted"
				if v.Run.Status == "recovery-discarded" {
					status = "restart-refused"
				}
				return RecoveredReview{Grant: grant, RunID: v.RunID, Status: status, Detail: recovery.Outcome}, true
			}
		}
	}
	return RecoveredReview{}, false
}
