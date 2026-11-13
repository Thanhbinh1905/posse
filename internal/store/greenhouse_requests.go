package store

import (
	"context"
	"database/sql"
	"errors"
)

type GreenhouseReviewRequest struct {
	ID            int64
	ProjectID     int64
	TaskID        int64
	Repository    string
	RequestedHead string
	Provider      string
	RequestedAt   int64
	SettledAt     int64
	Outcome       string
	VerdictID     *int64
	Grant         string
}

func (db *DB) RecordGreenhouseReviewRequest(ctx context.Context, request GreenhouseReviewRequest) error {
	if request.Provider != GreenhouseReviewProvider || request.TaskID < 1 || request.ProjectID < 1 || request.Repository == "" || len(request.RequestedHead) != 40 && len(request.RequestedHead) != 64 || request.RequestedAt < 1 || request.SettledAt < request.RequestedAt {
		return errors.New("invalid Greenhouse review request")
	}
	if request.Outcome != "verdict" && request.Outcome != "refused" && request.Outcome != "failed" && request.Outcome != "head_mismatch" {
		return errors.New("invalid Greenhouse review request outcome")
	}
	var belongs bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=? AND project_id=?)`, request.TaskID, request.ProjectID).Scan(&belongs); err != nil {
		return err
	}
	if !belongs {
		return errors.New("greenhouse request Task does not belong to its Project")
	}
	var verdictID any
	if request.Grant != "" {
		var id int64
		err := db.QueryRowContext(ctx, `SELECT id FROM verdicts WHERE grant_id=? AND task_id=? AND repo=? AND provider='greenhouse' AND mode='parallel'`, request.Grant, request.TaskID, request.Repository).Scan(&id)
		if err == nil {
			verdictID = id
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if request.Outcome == "verdict" && verdictID == nil {
		return errors.New("greenhouse verdict request has no linked Verdict")
	}
	_, err := db.ExecContext(ctx, `INSERT INTO greenhouse_review_requests(project_id,task_id,repo,requested_head,provider,requested_at,settled_at,outcome,verdict_id,grant_id) VALUES(?,?,?,?,?,?,?,?,?,NULLIF(?,'')) ON CONFLICT(grant_id) DO NOTHING`,
		request.ProjectID, request.TaskID, request.Repository, request.RequestedHead, request.Provider, request.RequestedAt, request.SettledAt, request.Outcome, verdictID, request.Grant)
	return err
}

func (db *DB) GreenhouseReviewRequests(ctx context.Context, projectID int64) ([]GreenhouseReviewRequest, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,project_id,task_id,repo,requested_head,provider,requested_at,settled_at,outcome,verdict_id,COALESCE(grant_id,'') FROM greenhouse_review_requests WHERE project_id=? ORDER BY requested_at,id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var requests []GreenhouseReviewRequest
	for rows.Next() {
		var request GreenhouseReviewRequest
		if err := rows.Scan(&request.ID, &request.ProjectID, &request.TaskID, &request.Repository, &request.RequestedHead, &request.Provider, &request.RequestedAt, &request.SettledAt, &request.Outcome, &request.VerdictID, &request.Grant); err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	return requests, rows.Err()
}
