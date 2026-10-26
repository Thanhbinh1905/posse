package store

import (
	"context"
	"database/sql"
)

// WatchRecovery stores retry state independently of the short-lived Lead
// watcher process, so a Notice-triggered re-arm cannot reset its backoff.
type WatchRecovery struct {
	PaneID       string
	RetryAt      int64
	FailedStarts int
	NoticeRaised bool
}

func (db *DB) WatchRecovery(ctx context.Context, projectID int64) (WatchRecovery, error) {
	var state WatchRecovery
	var noticeRaised int
	err := db.QueryRowContext(ctx, `SELECT lookout_pane_id,lookout_retry_at,lookout_failed_starts,lookout_notice_raised FROM project_runtime WHERE project_id=?`, projectID).
		Scan(&state.PaneID, &state.RetryAt, &state.FailedStarts, &noticeRaised)
	if IsNotFound(err) {
		return WatchRecovery{}, nil
	}
	state.NoticeRaised = noticeRaised != 0
	return state, err
}

func (db *DB) SetWatchRecovery(ctx context.Context, projectID int64, state WatchRecovery) error {
	noticeRaised := 0
	if state.NoticeRaised {
		noticeRaised = 1
	}
	_, err := db.ExecContext(ctx, `INSERT INTO project_runtime(project_id,server_started_at,lookout_pane_id,lookout_retry_at,lookout_failed_starts,lookout_notice_raised) VALUES(?,'',?,?,?,?) ON CONFLICT(project_id) DO UPDATE SET lookout_pane_id=excluded.lookout_pane_id,lookout_retry_at=excluded.lookout_retry_at,lookout_failed_starts=excluded.lookout_failed_starts,lookout_notice_raised=excluded.lookout_notice_raised`, projectID, state.PaneID, state.RetryAt, state.FailedStarts, noticeRaised)
	return err
}

func (db *DB) ResetWatchRecovery(ctx context.Context, projectID int64) error {
	_, err := db.ExecContext(ctx, `UPDATE project_runtime SET lookout_pane_id='',lookout_retry_at=0,lookout_failed_starts=0,lookout_notice_raised=0 WHERE project_id=?`, projectID)
	return err
}

func (db *DB) ProjectServerStartedAt(ctx context.Context, projectID int64) (string, error) {
	var startedAt string
	err := db.QueryRowContext(ctx, `SELECT server_started_at FROM project_runtime WHERE project_id=?`, projectID).Scan(&startedAt)
	if IsNotFound(err) {
		return "", nil
	}
	return startedAt, err
}

func (db *DB) RememberProjectServerStartedAt(ctx context.Context, projectID int64, startedAt string) error {
	if startedAt == "" {
		return nil
	}
	known, err := db.ProjectServerStartedAt(ctx, projectID)
	if err != nil || known != "" {
		return err
	}
	return db.withBusyTimeout(ctx, reconcileWriteBusyTimeoutMillis, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO project_runtime(project_id,server_started_at) VALUES(?,?) ON CONFLICT(project_id) DO UPDATE SET server_started_at=excluded.server_started_at WHERE project_runtime.server_started_at=''`, projectID, startedAt)
		return err
	})
}

func (db *DB) SetProjectServerStartedAt(ctx context.Context, projectID int64, startedAt string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO project_runtime(project_id,server_started_at) VALUES(?,?) ON CONFLICT(project_id) DO UPDATE SET server_started_at=excluded.server_started_at`, projectID, startedAt)
	return err
}

// LeadAgent is the agent kind and session of the Project's last known Lead.
type LeadAgent struct {
	Kind    string
	Session string
}

func (db *DB) ProjectLeadAgent(ctx context.Context, projectID int64) (LeadAgent, error) {
	var lead LeadAgent
	err := db.QueryRowContext(ctx, `SELECT lead_agent_kind,lead_agent_session FROM project_runtime WHERE project_id=?`, projectID).Scan(&lead.Kind, &lead.Session)
	if IsNotFound(err) {
		return LeadAgent{}, nil
	}
	return lead, err
}

// SetProjectLeadAgent records the Lead a launch starts, replacing any earlier
// session.
func (db *DB) SetProjectLeadAgent(ctx context.Context, projectID int64, lead LeadAgent) error {
	_, err := db.ExecContext(ctx, `INSERT INTO project_runtime(project_id,server_started_at,lead_agent_kind,lead_agent_session) VALUES(?,'',?,?) ON CONFLICT(project_id) DO UPDATE SET lead_agent_kind=excluded.lead_agent_kind,lead_agent_session=excluded.lead_agent_session`, projectID, lead.Kind, lead.Session)
	return err
}

// ObserveProjectLeadAgent records the Lead seen running in Herdr. A missing
// session keeps the recorded one for the same kind, since harnesses report
// their session only after startup. Every reconcile observes the Lead, so an
// unchanged observation only reads: an upsert takes the write lock even when
// its WHERE skips the update.
func (db *DB) ObserveProjectLeadAgent(ctx context.Context, projectID int64, lead LeadAgent) error {
	if lead.Kind == "" {
		return nil
	}
	recorded, err := db.ProjectLeadAgent(ctx, projectID)
	if err != nil {
		return err
	}
	if recorded.Kind == lead.Kind && (lead.Session == "" || lead.Session == recorded.Session) {
		return nil
	}
	return db.withBusyTimeout(ctx, reconcileWriteBusyTimeoutMillis, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO project_runtime(project_id,server_started_at,lead_agent_kind,lead_agent_session) VALUES(?,'',?,?) ON CONFLICT(project_id) DO UPDATE SET lead_agent_session=CASE WHEN excluded.lead_agent_session<>'' OR excluded.lead_agent_kind<>project_runtime.lead_agent_kind THEN excluded.lead_agent_session ELSE project_runtime.lead_agent_session END,lead_agent_kind=excluded.lead_agent_kind WHERE project_runtime.lead_agent_kind<>excluded.lead_agent_kind OR (excluded.lead_agent_session<>'' AND project_runtime.lead_agent_session<>excluded.lead_agent_session)`, projectID, lead.Kind, lead.Session)
		return err
	})
}
