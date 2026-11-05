package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// ReviewPassNextStep is the instruction a passing Review Round queues to its
// done full-pipeline Change Worker: validate and promote the reviewed private
// candidates.
type ReviewPassNextStep struct {
	ChangeTaskID int64
	Candidates   []ReviewPassCandidate
	Body         string
	Summary      string
}

// ReviewPassCandidate is one reviewed private candidate the next step names.
type ReviewPassCandidate struct {
	ID         int64  `json:"candidate_id"`
	Repository string `json:"repository,omitempty"`
	SHA        string `json:"head_sha"`
}

// ValidationCandidateQueued reports whether Review queued this candidate for validation.
func (db *DB) ValidationCandidateQueued(ctx context.Context, projectID, taskID, candidateID int64, candidateSHA string) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT data_json FROM notices WHERE project_id=? AND task_id=? AND kind='candidate_validation_queued' ORDER BY id DESC`, projectID, taskID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		var data struct {
			Candidates []ReviewPassCandidate `json:"candidates"`
		}
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			continue
		}
		for _, candidate := range data.Candidates {
			if candidate.ID == candidateID && candidate.SHA == candidateSHA {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}

// queueReviewPassNextStepTx queues step and raises its Notice when the Change
// Task is still done and every named candidate is still the active reviewed
// head. Otherwise the Worker is already acting, or a new head needs Review.
func queueReviewPassNextStepTx(ctx context.Context, tx *writeTx, projectID, changeTaskID int64, step ReviewPassNextStep) error {
	if step.ChangeTaskID != changeTaskID || len(step.Candidates) == 0 || strings.TrimSpace(step.Body) == "" {
		return nil
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id=? AND type='change'`, changeTaskID).Scan(&state); err != nil {
		return err
	}
	if State(state) != StateDone {
		return nil
	}
	for _, candidate := range step.Candidates {
		var current bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM validation_candidates WHERE id=? AND task_id=? AND state='active' AND candidate_sha=?)`, candidate.ID, changeTaskID, candidate.SHA).Scan(&current); err != nil {
			return err
		}
		if !current {
			return nil
		}
	}
	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `INSERT INTO messages(task_id,body,created_at,status,wait_for_idle) VALUES(?,?,?,'queued',1)`, changeTaskID, strings.TrimSpace(step.Body), now)
	if err != nil {
		return err
	}
	messageID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	data, err := json.Marshal(map[string]any{"message_id": messageID, "candidates": step.Candidates})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at) VALUES(?,?,'candidate_validation_queued',?,?,?)`, projectID, changeTaskID, step.Summary, string(data), now)
	return err
}

// QueueReviewNextStep queues step for a Change Task whose reviewed heads were
// settled outside a Review verdict, such as by a disposition.
func (db *DB) QueueReviewNextStep(ctx context.Context, projectID int64, step ReviewPassNextStep) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := queueReviewPassNextStepTx(ctx, tx, projectID, step.ChangeTaskID, step); err != nil {
		return err
	}
	return tx.Commit()
}
