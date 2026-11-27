package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var ErrReviewDispositionRequiresUser = errors.New("blocking Review disposition requires User approval")

var ErrReviewCapDecisionRequired = errors.New("blocking Review finding at the spent Review cap requires the User's review_cap Decision")

// ApplyReviewDisposition appends the disposition event and updates the current
// finding projection in one transaction.
func (db *DB) ApplyReviewDisposition(ctx context.Context, taskID int64, findingSeqs, patternSeqs []int, action, severity, issue, reason, quote, actor, blockAt string, capSpent bool) error {
	resolveFollowup := oneOf(action, "followup_fixed", "followup_obsolete")
	deferFollowup := action == "defer_followup"
	if taskID < 1 || !(resolveFollowup || deferFollowup || oneOf(action, "accept", "defer", "withdraw", "reclassify")) || !oneOf(blockAt, "P0", "P1", "P2") {
		return fmt.Errorf("invalid Review disposition")
	}
	if resolveFollowup && (len(findingSeqs) != 1 || len(patternSeqs) != 0 || strings.TrimSpace(reason) == "" || !oneOf(actor, "lead", "user")) {
		return fmt.Errorf("follow-up resolution requires one finding and a reason from the Lead or User")
	}
	if action == "reclassify" && !oneOf(severity, "P0", "P1", "P2", "P3") {
		return fmt.Errorf("reclassify requires a severity P0, P1, P2 or P3")
	}
	if len(findingSeqs)+len(patternSeqs) == 0 {
		return fmt.Errorf("review disposition requires findings or patterns")
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	selected := map[int64]ReviewFinding{}
	addRows := func(query string, args ...any) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var f ReviewFinding
			var inScope int
			var inherited int64
			if err := rows.Scan(&f.ID, &f.Seq, &f.PatternID, &f.Severity, &f.Origin, &inScope, &f.State, &inherited, &f.FollowUpURL); err != nil {
				rows.Close()
				return err
			}
			f.InScope, f.InheritedFrom = inScope == 1, inherited
			selected[f.ID] = f
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		return rows.Close()
	}
	if len(findingSeqs) > 0 {
		for _, seq := range findingSeqs {
			if seq < 1 {
				return fmt.Errorf("invalid finding sequence")
			}
			if err := addRows(`SELECT id,seq,pattern_id,severity,origin,in_scope,state,COALESCE(inherited_from,0),COALESCE(follow_up_url,'') FROM review_findings WHERE ship_task_id=? AND seq=?`, taskID, seq); err != nil {
				return err
			}
		}
	}
	if len(patternSeqs) > 0 {
		for _, seq := range patternSeqs {
			if seq < 1 {
				return fmt.Errorf("invalid pattern sequence")
			}
			var patternID int64
			if err := tx.QueryRowContext(ctx, `SELECT id FROM review_patterns WHERE ship_task_id=? AND seq=?`, taskID, seq).Scan(&patternID); err != nil {
				return err
			}
			if err := addRows(`SELECT id,seq,pattern_id,severity,origin,in_scope,state,COALESCE(inherited_from,0),COALESCE(follow_up_url,'') FROM review_findings WHERE ship_task_id=? AND pattern_id=? OR ship_task_id=? AND pattern_id IN (SELECT id FROM review_patterns WHERE merged_into=?)`, taskID, patternID, taskID, patternID); err != nil {
				return err
			}
		}
	}
	if len(selected) == 0 {
		return fmt.Errorf("review disposition did not select any findings")
	}
	threshold := map[string]int{"P0": 0, "P1": 1, "P2": 2}[blockAt]
	ids := make([]int64, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		f := selected[id]
		blockingClass := f.Origin == "introduced" && f.InScope &&
			(f.Severity == "unclassified" || map[string]int{"P0": 0, "P1": 1, "P2": 2, "P3": 3}[f.Severity] <= threshold)
		if (resolveFollowup || deferFollowup) && (blockingClass || f.InheritedFrom > 0 || !oneOf(f.State, "open", "deferred")) {
			return fmt.Errorf("finding F-%d is not an open or deferred non-blocking follow-up", f.Seq)
		}
		if deferFollowup && f.FollowUpURL != "" {
			return fmt.Errorf("finding F-%d already has a follow-up issue", f.Seq)
		}
		blocking := blockingClass && oneOf(f.State, "open", "fix_claimed", "unverified", "disputed", "regressed")
		if capSpent && blocking && (action != "reclassify" || map[string]int{"P0": 0, "P1": 1, "P2": 2, "P3": 3}[severity] > threshold) {
			return fmt.Errorf("%w: F-%d", ErrReviewCapDecisionRequired, f.Seq)
		}
		if blocking && action != "reclassify" && strings.TrimSpace(quote) == "" {
			return fmt.Errorf("%w: blocking Review findings require --user-approved <quote>", ErrReviewDispositionRequiresUser)
		}
		state, event, note := f.State, action, strings.TrimSpace(reason)
		switch action {
		case "accept":
			state = "accepted"
		case "defer", "defer_followup":
			state, event = "deferred", "defer"
		case "withdraw", "followup_obsolete":
			state = "withdrawn"
		case "followup_fixed":
			state = "fixed"
		case "reclassify":
			state = f.State
			note = "severity " + severity
			if blocking && map[string]int{"P0": 0, "P1": 1, "P2": 2, "P3": 3}[severity] > threshold && strings.TrimSpace(quote) == "" {
				return fmt.Errorf("%w: reclassifying a blocking finding below block_at requires --user-approved <quote>", ErrReviewDispositionRequiresUser)
			}
		}
		if action == "reclassify" {
			if _, err := tx.ExecContext(ctx, `UPDATE review_findings SET severity=? WHERE id=?`, severity, id); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE review_findings SET state=?,follow_up_url=CASE WHEN ?='defer' THEN ? ELSE follow_up_url END WHERE id=?`, state, event, issue, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_finding_events(finding_id,round,event,actor,note,user_quote,at) VALUES(?,0,?,?,?,?,?)`, id, event, actor, note, quote, time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

type FollowupResolution struct {
	Outcome string `json:"outcome"`
	Actor   string `json:"actor"`
	Reason  string `json:"reason"`
	At      int64  `json:"at"`
}

func (db *DB) LastFollowupResolutions(ctx context.Context, taskID int64) (map[int]FollowupResolution, error) {
	rows, err := db.QueryContext(ctx, `SELECT f.seq,e.event,e.actor,e.note,e.at
FROM review_findings f JOIN review_finding_events e ON e.finding_id=f.id
WHERE f.ship_task_id=? AND e.event IN ('followup_fixed','followup_obsolete') ORDER BY e.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	resolutions := map[int]FollowupResolution{}
	for rows.Next() {
		var seq int
		var resolution FollowupResolution
		if err := rows.Scan(&seq, &resolution.Outcome, &resolution.Actor, &resolution.Reason, &resolution.At); err != nil {
			return nil, err
		}
		resolution.Outcome = strings.TrimPrefix(resolution.Outcome, "followup_")
		resolutions[seq] = resolution
	}
	return resolutions, rows.Err()
}

// MergeReviewPatterns folds each later pattern into the first selected pattern
// and records the operation in the Finding Ledger event stream.
func (db *DB) MergeReviewPatterns(ctx context.Context, taskID int64, patterns []int, actor string) error {
	if taskID < 1 || len(patterns) < 2 {
		return fmt.Errorf("pattern merge requires at least two patterns")
	}
	seen := map[int]bool{}
	for _, seq := range patterns {
		if seq < 1 || seen[seq] {
			return fmt.Errorf("invalid or duplicate pattern P-%d", seq)
		}
		seen[seq] = true
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var primary int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM review_patterns WHERE ship_task_id=? AND seq=? AND merged_into IS NULL`, taskID, patterns[0]).Scan(&primary); err != nil {
		return err
	}
	for _, seq := range patterns[1:] {
		var patternID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM review_patterns WHERE ship_task_id=? AND seq=? AND merged_into IS NULL`, taskID, seq).Scan(&patternID); err != nil {
			return err
		}
		if patternID == primary {
			return fmt.Errorf("a merged pattern cannot be merged again")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_patterns SET merged_into=? WHERE id=? AND ship_task_id=?`, primary, patternID, taskID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM review_findings WHERE ship_task_id=? AND pattern_id=?`, taskID, patternID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO review_finding_events(finding_id,round,event,actor,note,at) VALUES(?,0,'pattern_merged',?,?,?)`, id, actor, fmt.Sprintf("merged into P-%d", patterns[0]), time.Now().UnixMilli()); err != nil {
				rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}
