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
	if taskID < 1 || !oneOf(action, "accept", "defer", "withdraw", "reclassify") || !oneOf(blockAt, "P0", "P1", "P2") {
		return fmt.Errorf("invalid Review disposition")
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
			if err := rows.Scan(&f.ID, &f.Seq, &f.PatternID, &f.Severity, &f.Origin, &inScope, &f.State, &inherited); err != nil {
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
			if err := addRows(`SELECT id,seq,pattern_id,severity,origin,in_scope,state,COALESCE(inherited_from,0) FROM review_findings WHERE ship_task_id=? AND seq=?`, taskID, seq); err != nil {
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
			if err := addRows(`SELECT id,seq,pattern_id,severity,origin,in_scope,state,COALESCE(inherited_from,0) FROM review_findings WHERE ship_task_id=? AND pattern_id=? OR ship_task_id=? AND pattern_id IN (SELECT id FROM review_patterns WHERE merged_into=?)`, taskID, patternID, taskID, patternID); err != nil {
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
		blocking := f.Origin == "introduced" && f.InScope && oneOf(f.State, "open", "fix_claimed", "unverified", "disputed", "regressed") &&
			(f.Severity == "unclassified" || map[string]int{"P0": 0, "P1": 1, "P2": 2, "P3": 3}[f.Severity] <= threshold)
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
		case "defer":
			state = "deferred"
		case "withdraw":
			state = "withdrawn"
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
		} else if _, err := tx.ExecContext(ctx, `UPDATE review_findings SET state=?,follow_up_url=CASE WHEN ?='defer' THEN ? ELSE follow_up_url END WHERE id=?`, state, action, issue, id); err != nil {
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
