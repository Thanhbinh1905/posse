package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

type ProviderFindingDetail struct {
	FindingSeq  int
	PatternSeq  int
	PatternKey  string
	Title       string
	Rule        string
	FixGuidance string
	Repository  string
	HeadSHA     string
	Path        string
	Line        int
	Excerpt     string
	Severity    string
	Origin      string
	State       string
}

func providerFindingKey(finding ProviderFinding) string {
	if key := strings.TrimSpace(finding.Key); key != "" {
		return key
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{finding.Rule, finding.Path, fmt.Sprint(finding.Line), finding.Excerpt}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func providerPatternKey(finding ProviderFinding) string {
	key := normalizeReviewPatternKey(finding.Rule)
	if key == "" {
		key = normalizeReviewPatternKey(finding.Origin)
	}
	if key == "" {
		key = "unclassified"
	}
	return key
}

func recordProviderFindingsTx(ctx context.Context, tx *sql.Tx, verdictID int64, verdict Verdict) error {
	for _, finding := range verdict.Findings {
		if finding.Head != "" && finding.Head != verdict.Head {
			return fmt.Errorf("provider finding %q is bound to head %s, not Verdict head %s", finding.Key, finding.Head, verdict.Head)
		}
		if strings.TrimSpace(finding.Path) == "" || finding.Line < 1 {
			return fmt.Errorf("provider finding %q has no path or positive line", finding.Key)
		}
		key := providerPatternKey(finding)
		var patternSeq int
		err := tx.QueryRowContext(ctx, `SELECT seq FROM provider_finding_patterns WHERE task_id=? AND key=?`, verdict.TaskID, key).Scan(&patternSeq)
		if err == sql.ErrNoRows {
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM provider_finding_patterns WHERE task_id=?`, verdict.TaskID).Scan(&patternSeq); err != nil {
				return err
			}
			title := strings.TrimSpace(finding.Rule)
			if title == "" {
				title = strings.TrimSpace(finding.Guidance)
			}
			if title == "" {
				title = "Provider finding pattern"
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO provider_finding_patterns(task_id,seq,key,title,rule,fix_guidance) VALUES(?,?,?,?,?,?)`, verdict.TaskID, patternSeq, key, title, finding.Rule, finding.Guidance); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO provider_findings(verdict_id,task_id,provider,repo,head_sha,finding_key,pattern_seq,path,line,excerpt,severity,origin,state) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'open')`,
			verdictID, verdict.TaskID, verdict.Provider, verdict.Repository, verdict.Head, providerFindingKey(finding), patternSeq, finding.Path, finding.Line, finding.Excerpt, finding.Severity, finding.Origin); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) ProviderFindingsForTask(ctx context.Context, taskID int64) ([]ProviderFindingDetail, error) {
	rows, err := db.QueryContext(ctx, `SELECT f.id,f.pattern_seq,p.key,p.title,p.rule,p.fix_guidance,f.repo,f.head_sha,f.path,f.line,f.excerpt,f.severity,f.origin,f.state
FROM provider_findings f JOIN provider_finding_patterns p ON p.task_id=f.task_id AND p.seq=f.pattern_seq
JOIN verdicts v ON v.id=f.verdict_id
WHERE f.task_id=? AND v.provider=f.provider AND v.id=(SELECT id FROM verdicts latest WHERE latest.task_id=f.task_id AND latest.repo=f.repo AND latest.provider=f.provider ORDER BY latest.recorded_at DESC,latest.id DESC LIMIT 1)
ORDER BY f.repo,f.pattern_seq,f.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var findings []ProviderFindingDetail
	for rows.Next() {
		var finding ProviderFindingDetail
		if err := rows.Scan(&finding.FindingSeq, &finding.PatternSeq, &finding.PatternKey, &finding.Title, &finding.Rule, &finding.FixGuidance,
			&finding.Repository, &finding.HeadSHA, &finding.Path, &finding.Line, &finding.Excerpt, &finding.Severity, &finding.Origin, &finding.State); err != nil {
			return nil, err
		}
		findings = append(findings, finding)
	}
	return findings, rows.Err()
}

func bindProviderFixRoundTx(ctx context.Context, tx *writeTx, roundID, taskID int64, expectedHead string, members []ValidationFixMemberHead, patternSeqs []int) error {
	for _, patternSeq := range patternSeqs {
		if len(members) == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO fix_round_provider_findings(fix_round_id,finding_id,pattern_seq)
SELECT ?,f.id,f.pattern_seq FROM provider_findings f
WHERE f.task_id=? AND f.head_sha=? AND f.pattern_seq=? AND f.state IN ('open','disputed')
AND f.verdict_id=(SELECT id FROM verdicts v WHERE v.task_id=f.task_id AND v.repo=f.repo AND v.provider=f.provider ORDER BY v.recorded_at DESC,v.id DESC LIMIT 1)`, roundID, taskID, expectedHead, patternSeq); err != nil {
				return err
			}
		} else {
			for _, member := range members {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO fix_round_provider_findings(fix_round_id,finding_id,pattern_seq)
SELECT ?,f.id,f.pattern_seq FROM provider_findings f
WHERE f.task_id=? AND f.repo=? AND f.head_sha=? AND f.pattern_seq=? AND f.state IN ('open','disputed')
AND f.verdict_id=(SELECT id FROM verdicts v WHERE v.task_id=f.task_id AND v.repo=f.repo AND v.provider=f.provider ORDER BY v.recorded_at DESC,v.id DESC LIMIT 1)`, roundID, taskID, member.Repository, member.ExpectedBaseHead, patternSeq); err != nil {
					return err
				}
			}
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM fix_round_provider_findings WHERE fix_round_id=? AND pattern_seq=?`, roundID, patternSeq).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return fmt.Errorf("provider finding pattern P-%d has no current findings for the requested head", patternSeq)
		}
	}
	return nil
}

func applyProviderFixAnswersTx(ctx context.Context, tx *writeTx, taskID int64, roundID int64, selected map[int]bool, answers map[int]ReviewFixResolution) error {
	for patternSeq := range selected {
		answer, found := answers[patternSeq]
		if !found {
			return fmt.Errorf("%w: missing answer for pattern P-%d", ErrReviewResolutionsRequired, patternSeq)
		}
		state, event := "fix_claimed", "fix_claimed"
		if answer.Status == "disputed" {
			state, event = "disputed", "disputed"
		}
		rows, err := tx.QueryContext(ctx, `SELECT f.id FROM provider_findings f JOIN fix_round_provider_findings r ON r.finding_id=f.id WHERE r.fix_round_id=? AND r.pattern_seq=? AND f.task_id=? AND f.state IN ('open','disputed')`, roundID, patternSeq, taskID)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return fmt.Errorf("%w: pattern P-%d has no open Greenhouse findings", ErrReviewResolutionsInvalid, patternSeq)
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE provider_findings SET state=? WHERE id=?`, state, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO provider_finding_events(finding_id,event,note,at) VALUES(?,?,?,?)`, id, event, answer.Note, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
	}
	return nil
}
