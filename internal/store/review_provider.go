package store

import (
	"context"
	"database/sql"
	"time"
)

const BuiltinReviewProvider = "builtin"

// Verdict is an immutable review result bound to one repository head and target.
type Verdict struct {
	TaskID       int64
	Repository   string
	Provider     string
	Protocol     string
	Grant        string
	Head         string
	Target       string
	ReviewBase   string
	Outcome      string
	Blocking     int
	RulesDigest  string
	ConfigDigest string
	InputDigest  string
	Backend      string
	Model        string
	FindingsHash string
	FindingsPath string
	Mode         string
	RecordedAt   int64
}

// VerdictTx is the transaction boundary used when a provider records its result.
type VerdictTx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// ReviewProvider persists a provider's result within the review completion transaction.
type ReviewProvider interface {
	RecordVerdict(context.Context, VerdictTx, Verdict) error
}

type builtinReviewProvider struct{}

// VerdictsFor returns the immutable provider results recorded for a Task.
func (db *DB) VerdictsFor(ctx context.Context, taskID int64) ([]Verdict, error) {
	rows, err := db.QueryContext(ctx, `SELECT task_id,repo,provider,protocol,COALESCE(grant_id,''),head_sha,target_sha,review_base_sha,verdict,blocking,rules_digest,config_digest,input_digest,backend,model,findings_digest,findings_path,mode,recorded_at FROM verdicts WHERE task_id=? ORDER BY recorded_at,id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var verdicts []Verdict
	for rows.Next() {
		var verdict Verdict
		if err := rows.Scan(&verdict.TaskID, &verdict.Repository, &verdict.Provider, &verdict.Protocol, &verdict.Grant, &verdict.Head, &verdict.Target, &verdict.ReviewBase, &verdict.Outcome, &verdict.Blocking, &verdict.RulesDigest, &verdict.ConfigDigest, &verdict.InputDigest, &verdict.Backend, &verdict.Model, &verdict.FindingsHash, &verdict.FindingsPath, &verdict.Mode, &verdict.RecordedAt); err != nil {
			return nil, err
		}
		verdicts = append(verdicts, verdict)
	}
	return verdicts, rows.Err()
}

func (builtinReviewProvider) RecordVerdict(ctx context.Context, tx VerdictTx, verdict Verdict) error {
	if verdict.Provider != BuiltinReviewProvider {
		return sql.ErrNoRows
	}
	return insertVerdict(ctx, tx, verdict)
}

func insertVerdict(ctx context.Context, tx VerdictTx, verdict Verdict) error {
	if verdict.RecordedAt == 0 {
		verdict.RecordedAt = time.Now().UnixMilli()
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO verdicts(task_id,repo,provider,protocol,grant_id,head_sha,target_sha,review_base_sha,verdict,blocking,rules_digest,config_digest,input_digest,backend,model,findings_digest,findings_path,mode,recorded_at) VALUES(?,?,?,?,NULLIF(?,''),?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		verdict.TaskID, verdict.Repository, verdict.Provider, verdict.Protocol, verdict.Grant,
		verdict.Head, verdict.Target, verdict.ReviewBase, verdict.Outcome, verdict.Blocking,
		verdict.RulesDigest, verdict.ConfigDigest, verdict.InputDigest, verdict.Backend, verdict.Model,
		verdict.FindingsHash, verdict.FindingsPath, verdict.Mode, verdict.RecordedAt)
	return err
}

// Qualifies reports whether this verdict meets the exact-head gate rule.
func (verdict Verdict) Qualifies(head string) bool {
	return verdict.Mode == "gate" && verdict.Head == head &&
		(verdict.Outcome == "clean" || verdict.Outcome == "findings" && verdict.Blocking == 0)
}

func countBlockingReviewFindingsTx(ctx context.Context, tx *writeTx, taskID int64, repository, blockAt string) (int, error) {
	threshold := map[string]int{"P0": 0, "P1": 1, "P2": 2}[blockAt]
	rows, err := tx.QueryContext(ctx, `SELECT severity FROM review_findings WHERE ship_task_id=? AND repository=? AND origin='introduced' AND in_scope=1 AND state IN ('open','fix_claimed','unverified','disputed','regressed')`, taskID, repository)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	blocking := 0
	for rows.Next() {
		var severity string
		if err := rows.Scan(&severity); err != nil {
			return 0, err
		}
		rank, known := map[string]int{"P0": 0, "P1": 1, "P2": 2, "P3": 3}[severity]
		if !known || rank <= threshold {
			blocking++
		}
	}
	return blocking, rows.Err()
}
