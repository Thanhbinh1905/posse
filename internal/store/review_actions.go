package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/taskplan"
)

// InheritReviewLedger copies a superseded Task's patterns and findings into a
// new Change Task as non-blocking inherited entries that suppress repeat reports.
func (db *DB) InheritReviewLedger(ctx context.Context, sourceTaskID, destinationTaskID int64) error {
	if sourceTaskID < 1 || destinationTaskID < 1 || sourceTaskID == destinationTaskID {
		return fmt.Errorf("invalid Review ledger inheritance")
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sourceProject, destinationProject int64
	var sourceSeq int
	if err := tx.QueryRowContext(ctx, `SELECT project_id,seq FROM tasks WHERE id=? AND type='change'`, sourceTaskID).Scan(&sourceProject, &sourceSeq); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id=? AND type='change'`, destinationTaskID).Scan(&destinationProject); err != nil {
		return err
	}
	if sourceProject != destinationProject {
		return fmt.Errorf("superseded Change Task belongs to another Project")
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_findings WHERE ship_task_id=?`, destinationTaskID).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		var inherited int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_findings WHERE ship_task_id=? AND inherited_from IS NOT NULL`, destinationTaskID).Scan(&inherited); err != nil {
			return err
		}
		if existing == inherited {
			return tx.Commit()
		}
		return fmt.Errorf("destination Change Task already has a non-inherited Finding Ledger")
	}
	type pattern struct {
		id                                       int64
		seq                                      int
		key, title, rule, rootCause, fixGuidance string
		merged                                   sql.NullInt64
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,seq,key,title,rule,root_cause,fix_guidance,merged_into FROM review_patterns WHERE ship_task_id=? ORDER BY seq`, sourceTaskID)
	if err != nil {
		return err
	}
	var patterns []pattern
	for rows.Next() {
		var item pattern
		if err := rows.Scan(&item.id, &item.seq, &item.key, &item.title, &item.rule, &item.rootCause, &item.fixGuidance, &item.merged); err != nil {
			rows.Close()
			return err
		}
		patterns = append(patterns, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	patternIDs := make(map[int64]int64, len(patterns))
	for _, item := range patterns {
		result, err := tx.ExecContext(ctx, `INSERT INTO review_patterns(ship_task_id,seq,key,title,rule,root_cause,fix_guidance,merged_into,created_round) VALUES(?,?,?,?,?,?,?,NULL,1)`, destinationTaskID, item.seq, item.key, item.title, item.rule, item.rootCause, item.fixGuidance)
		if err != nil {
			return err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return err
		}
		patternIDs[item.id] = id
	}
	for _, item := range patterns {
		if item.merged.Valid {
			if _, err := tx.ExecContext(ctx, `UPDATE review_patterns SET merged_into=? WHERE id=?`, patternIDs[item.merged.Int64], patternIDs[item.id]); err != nil {
				return err
			}
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT id,seq,pattern_id,fingerprint,round,repository,path,symbol,excerpt,line_at_report,head_sha,severity,origin,in_scope,follow_up_url FROM review_findings WHERE ship_task_id=? ORDER BY seq`, sourceTaskID)
	if err != nil {
		return err
	}
	type finding struct {
		id                                int64
		seq                               int
		pattern                           int64
		fingerprint                       string
		round                             int
		repository, path, symbol, excerpt string
		line                              int
		head, severity, origin            string
		inScope                           int
		issue                             string
	}
	var findings []finding
	for rows.Next() {
		var item finding
		if err := rows.Scan(&item.id, &item.seq, &item.pattern, &item.fingerprint, &item.round, &item.repository, &item.path, &item.symbol, &item.excerpt, &item.line, &item.head, &item.severity, &item.origin, &item.inScope, &item.issue); err != nil {
			rows.Close()
			return err
		}
		findings = append(findings, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range findings {
		if item.round < 1 {
			item.round = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_findings(ship_task_id,seq,pattern_id,fingerprint,round,repository,path,symbol,excerpt,line_at_report,head_sha,severity,origin,in_scope,state,follow_up_url,inherited_from) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,'deferred',?,?)`, destinationTaskID, item.seq, patternIDs[item.pattern], item.fingerprint, item.round, item.repository, item.path, item.symbol, item.excerpt, item.line, item.head, item.severity, item.origin, item.inScope, item.issue, item.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_finding_events(finding_id,round,event,actor,note,at) VALUES((SELECT id FROM review_findings WHERE ship_task_id=? AND seq=?),0,'inherited','system',?,?)`, destinationTaskID, item.seq, fmt.Sprintf("inherited from t%d F-%d", sourceSeq, item.seq), time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, destinationTaskID)
}

// ReviewFindingsForTask returns the immutable pattern descriptions and current
// finding states used to prepare a whole-pattern fix instruction.
func (db *DB) ReviewFindingsForTask(ctx context.Context, taskID int64) ([]ReviewFindingDetail, error) {
	rows, err := db.QueryContext(ctx, `SELECT f.seq,COALESCE(parent.seq,p.seq),COALESCE(parent.key,p.key),COALESCE(parent.title,p.title),COALESCE(parent.rule,p.rule),COALESCE(parent.root_cause,p.root_cause),COALESCE(parent.fix_guidance,p.fix_guidance),f.repository,f.path,f.symbol,f.excerpt,f.severity,f.origin,f.in_scope,f.state,COALESCE(f.inherited_from,0),COALESCE(f.follow_up_url,'')
FROM review_findings f JOIN review_patterns p ON p.id=f.pattern_id LEFT JOIN review_patterns parent ON parent.id=p.merged_into
WHERE f.ship_task_id=? ORDER BY COALESCE(parent.seq,p.seq),f.seq`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var findings []ReviewFindingDetail
	for rows.Next() {
		var detail ReviewFindingDetail
		var inScope int
		if err := rows.Scan(&detail.FindingSeq, &detail.PatternSeq, &detail.PatternKey, &detail.Title, &detail.Rule, &detail.RootCause, &detail.FixGuidance,
			&detail.Repository, &detail.Path, &detail.Symbol, &detail.Excerpt, &detail.Severity, &detail.Origin, &inScope, &detail.State, &detail.InheritedFrom, &detail.FollowUpURL); err != nil {
			return nil, err
		}
		detail.InScope = inScope == 1
		findings = append(findings, detail)
	}
	return findings, rows.Err()
}

// ReviewFixRoundForTask returns the latest still-applicable whole-pattern fix
// instruction, if one was delivered after the Worker's previous done Signal.
func (db *DB) ReviewFixRoundForTask(ctx context.Context, taskID int64) (ValidationFixRound, bool, error) {
	round, err := scanReviewFixRound(db.QueryRowContext(ctx, deliveredReviewFixRoundSQL, taskID, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return ValidationFixRound{}, false, nil
	}
	return round, err == nil, err
}

const deliveredReviewFixRoundSQL = `SELECT r.id,r.ship_task_id,r.kind,r.seq,r.expected_base_head,r.dedupe_key,r.message_id,r.created_at,r.member_heads_json
FROM validation_fix_rounds r JOIN messages m ON m.id=r.message_id
WHERE r.ship_task_id=? AND r.kind='review' AND m.status='delivered'
AND m.created_at>COALESCE((SELECT MAX(at) FROM signals WHERE task_id=? AND verb='done'),0)
ORDER BY r.id DESC LIMIT 1`

func scanReviewFixRound(row *sql.Row) (ValidationFixRound, error) {
	var round ValidationFixRound
	var memberHeads string
	if err := row.Scan(&round.ID, &round.ChangeTaskID, &round.Kind, &round.Seq, &round.ExpectedBaseHead, &round.DedupeKey, &round.MessageID, &round.CreatedAt, &memberHeads); err != nil {
		return ValidationFixRound{}, err
	}
	var err error
	round.MemberHeads, err = unmarshalValidationMemberHeads(memberHeads)
	return round, err
}

// ReviewFixInstructionPending reports whether an unanswered Review fix message
// has been queued or delivered since the Worker's last done Signal.
func (db *DB) ReviewFixInstructionPending(ctx context.Context, taskID int64) (bool, error) {
	var pending bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM validation_fix_rounds r JOIN messages m ON m.id=r.message_id
WHERE r.ship_task_id=? AND r.kind='review' AND m.status IN ('queued','claimed','interrupting','submitting','delivered')
AND m.created_at>COALESCE((SELECT MAX(at) FROM signals WHERE task_id=? AND verb='done'),0))`, taskID, taskID).Scan(&pending)
	return pending, err
}

// ReviewCapUsage combines verdict-recorded rounds with Review fix
// instructions. CI fix rounds and provider cancellation reruns never count.
func (db *DB) ReviewCapUsage(ctx context.Context, taskID int64) (ReviewCapUsage, error) {
	var usage ReviewCapUsage
	if err := db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT round) FROM review_rounds WHERE ship_task_id=? AND status='verdict'`, taskID).Scan(&usage.ReviewRounds); err != nil {
		return usage, err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='review'`, taskID).Scan(&usage.RepairRounds); err != nil {
		return usage, err
	}
	return usage, nil
}

// ReviewEvidenceFor reports whether the targets are covered by one pass, with
// any target outside that pass covered by a proved carry or an exact-head User
// disposition. Passes from different Review Rounds are never
// combined: a workspace pass binds the Member heads its Reviewer saw together,
// so it covers exactly those heads, or a subset of them. Carries and
// dispositions never imply test, docs, lint or CI evidence.
func (db *DB) ReviewEvidenceFor(ctx context.Context, taskID int64, targets []ReviewTarget) (ReviewEvidence, error) {
	var evidence ReviewEvidence
	if len(targets) == 0 {
		return evidence, nil
	}
	verdicts, err := db.ReviewVerdictsFor(ctx, taskID)
	if err != nil {
		return evidence, err
	}
	carried := make([]bool, len(targets))
	disposed := make([]bool, len(targets))
	for index, target := range targets {
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_carries WHERE ship_task_id=? AND repository=? AND to_head=?)`, taskID, target.Repository, target.Commit).Scan(&carried[index]); err != nil {
			return evidence, err
		}
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_dispositions WHERE ship_task_id=? AND head_sha=?)`, taskID, target.Commit).Scan(&disposed[index]); err != nil {
			return evidence, err
		}
	}
	// coveredBy reports whether reviewed, plus carries and dispositions, covers
	// every target, and records which evidence it needed.
	coveredBy := func(reviewed []ReviewTarget) (ReviewEvidence, bool) {
		result := ReviewEvidence{Complete: true}
		for index, target := range targets {
			if slices.Contains(reviewed, target) {
				result.Pass = true
				continue
			}
			if !carried[index] && !disposed[index] {
				return ReviewEvidence{}, false
			}
			result.Carry = result.Carry || carried[index]
			result.Disposition = result.Disposition || disposed[index]
		}
		return result, true
	}
	for _, verdict := range verdicts {
		if verdict.Verdict != ReviewPass {
			continue
		}
		if result, ok := coveredBy(verdict.Targets); ok && result.Pass {
			return result, nil
		}
	}
	if result, ok := coveredBy(nil); ok {
		return result, nil
	}
	for index := range targets {
		evidence.Carry = evidence.Carry || carried[index]
		evidence.Disposition = evidence.Disposition || disposed[index]
	}
	return evidence, nil
}

// RecordReviewCarry persists proof that a target merge changes no reviewed code.
func (db *DB) RecordReviewCarry(ctx context.Context, carry ReviewCarry) error {
	if carry.ChangeTaskID < 1 || carry.Repository == "" || !validValidationSHA(carry.FromHead) || !validValidationSHA(carry.ToHead) || !validValidationSHA(carry.TargetCommitSHA) || !validValidationSHA(carry.Tree) {
		return fmt.Errorf("invalid Review carry")
	}
	if carry.CreatedAt == 0 {
		carry.CreatedAt = time.Now().UnixMilli()
	}
	_, err := db.ExecContext(ctx, `INSERT INTO review_carries(ship_task_id,repository,from_head,to_head,target_commit_sha,tree,created_at)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(ship_task_id,repository,from_head,to_head) DO NOTHING`,
		carry.ChangeTaskID, carry.Repository, carry.FromHead, carry.ToHead, carry.TargetCommitSHA, carry.Tree, carry.CreatedAt)
	if err != nil {
		return err
	}
	return db.PersistTask(ctx, carry.ChangeTaskID)
}

// RecordReviewDisposition binds an explicit User acceptance to exact heads,
// one per changed repository, and appends its finding-state events atomically.
func (db *DB) RecordReviewDisposition(ctx context.Context, taskID, decisionID int64, heads []string, quote, blockAt string) error {
	if taskID < 1 || len(heads) == 0 || strings.TrimSpace(quote) == "" || !oneOf(blockAt, "P0", "P1", "P2") {
		return fmt.Errorf("invalid Review disposition")
	}
	for _, head := range heads {
		if !validValidationSHA(head) {
			return fmt.Errorf("invalid Review disposition")
		}
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertReviewDispositionsTx(ctx, tx, taskID, decisionID, heads, quote); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT f.id,f.seq,f.severity,f.origin,f.in_scope,f.state FROM review_findings f WHERE f.ship_task_id=? ORDER BY f.seq`, taskID)
	if err != nil {
		return err
	}
	type finding struct {
		id                      int64
		seq                     int
		severity, origin, state string
		inScope                 bool
	}
	var findings []finding
	for rows.Next() {
		var value finding
		var inScope int
		if err := rows.Scan(&value.id, &value.seq, &value.severity, &value.origin, &inScope, &value.state); err != nil {
			rows.Close()
			return err
		}
		value.inScope = inScope == 1
		findings = append(findings, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	threshold := map[string]int{"P0": 0, "P1": 1, "P2": 2}[blockAt]
	for _, value := range findings {
		if !oneOf(value.state, "open", "fix_claimed", "unverified", "disputed", "regressed") {
			continue
		}
		state, event := "deferred", "deferred"
		if value.origin == "introduced" && value.inScope && (value.severity == "unclassified" || map[string]int{"P0": 0, "P1": 1, "P2": 2, "P3": 3}[value.severity] <= threshold) {
			state, event = "accepted", "accepted"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_findings SET state=? WHERE id=? AND ship_task_id=?`, state, value.id, taskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_finding_events(finding_id,round,event,actor,note,user_quote,at) VALUES(?,0,?,'user','accepted at review cap',?,?)`, value.id, event, quote, time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func insertReviewDispositionsTx(ctx context.Context, tx *writeTx, taskID, decisionID int64, heads []string, quote string) error {
	for _, head := range heads {
		var existing bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_dispositions WHERE ship_task_id=? AND head_sha=?)`, taskID, head).Scan(&existing); err != nil {
			return err
		}
		if !existing {
			if _, err := tx.ExecContext(ctx, `INSERT INTO review_dispositions(ship_task_id,head_sha,decision_id,user_quote,at) VALUES(?,?,?,?,?)`, taskID, head, nullableDecisionID(decisionID), quote, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
	}
	return nil
}

// RecordReviewHeadDisposition binds the current heads to a disposition that
// left no blocking finding, without touching any finding. The next step needs
// no User quote because no blocking finding was accepted.
func (db *DB) RecordReviewHeadDisposition(ctx context.Context, taskID int64, heads []string, note string) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertReviewDispositionsTx(ctx, tx, taskID, 0, heads, note); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func nullableDecisionID(id int64) any {
	if id < 1 {
		return nil
	}
	return id
}

// applyReviewFixResolutionsTx requires exactly one answer for every pattern in
// the latest delivered Review Fix instruction, and no answers for other patterns.
func applyReviewFixResolutionsTx(ctx context.Context, tx *writeTx, taskID int64, input *ReviewFixResolutionsInput) error {
	round, err := scanReviewFixRound(tx.QueryRowContext(ctx, deliveredReviewFixRoundSQL, taskID, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		if input != nil {
			return fmt.Errorf("%w: no delivered Review fix instruction is pending", ErrReviewResolutionsInvalid)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if input == nil {
		return ErrReviewResolutionsRequired
	}
	if input.ExpectedBaseHead != round.ExpectedBaseHead || !validValidationSHA(input.ExpectedBaseHead) {
		return fmt.Errorf("%w: Review fix head binding changed", ErrReviewResolutionsInvalid)
	}
	if len(round.MemberHeads) > 0 || len(input.Members) > 0 {
		err = checkReviewFixMemberHeadsTx(ctx, tx, taskID, round.MemberHeads, input.Members)
	} else {
		err = checkReviewFixHeadTx(ctx, tx, taskID, input)
	}
	if err != nil {
		return err
	}
	selected, err := reviewFixPatternSequences(round.DedupeKey, round.ExpectedBaseHead)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrReviewResolutionsInvalid, err)
	}
	answers := make(map[int]ReviewFixResolution, len(input.Patterns))
	for _, answer := range input.Patterns {
		seq, ok := reviewPatternSequence(answer.Pattern)
		if !ok || (answer.Status != "fixed" && answer.Status != "disputed") {
			return fmt.Errorf("%w: each answer needs a P-n pattern and fixed or disputed status", ErrReviewResolutionsInvalid)
		}
		if !selected[seq] {
			return fmt.Errorf("%w: pattern %s was not selected by the pending instruction", ErrReviewResolutionsInvalid, answer.Pattern)
		}
		if _, exists := answers[seq]; exists {
			return fmt.Errorf("%w: duplicate answer for pattern P-%d", ErrReviewResolutionsInvalid, seq)
		}
		answers[seq] = answer
	}
	return applyReviewFixAnswersTx(ctx, tx, taskID, selected, answers)
}

// checkReviewFixHeadTx binds a single-repository fix answer to its head and,
// under full-pipeline, to the private candidate that holds it.
func checkReviewFixHeadTx(ctx context.Context, tx *writeTx, taskID int64, input *ReviewFixResolutionsInput) error {
	if !validValidationSHA(input.ActualHead) {
		return fmt.Errorf("%w: Review fix head binding changed", ErrReviewResolutionsInvalid)
	}
	var planJSON string
	if err := tx.QueryRowContext(ctx, `SELECT plan_json FROM tasks WHERE id=?`, taskID).Scan(&planJSON); err != nil {
		return err
	}
	if planJSON != "" {
		plan, err := taskplan.Decode(planJSON)
		if err != nil {
			return err
		}
		if plan.Review == "full-pipeline" && input.CandidateID < 1 {
			return fmt.Errorf("%w: full-pipeline Review fixes require private candidate binding", ErrReviewResolutionsInvalid)
		}
	}
	if input.CandidateID > 0 {
		var candidateHead, candidateState string
		if err := tx.QueryRowContext(ctx, `SELECT candidate_sha,state FROM validation_candidates WHERE id=? AND task_id=?`, input.CandidateID, taskID).Scan(&candidateHead, &candidateState); err != nil {
			return err
		}
		if candidateHead != input.ActualHead || candidateState != ValidationCandidateActive {
			return fmt.Errorf("%w: private candidate does not match the Review fix head", ErrReviewResolutionsInvalid)
		}
	}
	return nil
}

// checkReviewFixMemberHeadsTx requires one answer head for every Member the
// workspace fix instruction named. A Member bound to a private candidate must
// answer from an active candidate at its current head, either the bound one or
// a newer submission that superseded it.
func checkReviewFixMemberHeadsTx(ctx context.Context, tx *writeTx, taskID int64, expected []ValidationFixMemberHead, actual []ReviewFixMemberHead) error {
	if len(expected) == 0 || len(actual) != len(expected) {
		return fmt.Errorf("%w: Review fix Member heads changed", ErrReviewResolutionsInvalid)
	}
	answers := make(map[string]ReviewFixMemberHead, len(actual))
	for _, member := range actual {
		answers[member.Repository] = member
	}
	for _, member := range expected {
		answer, found := answers[member.Repository]
		if !found || answer.CandidateID < member.CandidateID || (member.CandidateID == 0 && answer.CandidateID != 0) || !validValidationSHA(answer.ActualHead) {
			return fmt.Errorf("%w: Review fix binding changed for Member %s", ErrReviewResolutionsInvalid, member.Repository)
		}
		if member.CandidateID == 0 {
			continue
		}
		var candidateHead, candidateState string
		err := tx.QueryRowContext(ctx, `SELECT candidate_sha,state FROM validation_candidates WHERE id=? AND task_id=? AND repo=?`, answer.CandidateID, taskID, member.Repository).Scan(&candidateHead, &candidateState)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: Member %s private candidate is missing", ErrReviewResolutionsInvalid, member.Repository)
		}
		if err != nil {
			return err
		}
		if candidateHead != answer.ActualHead || candidateState != ValidationCandidateActive {
			return fmt.Errorf("%w: Member %s private candidate does not match the Review fix head", ErrReviewResolutionsInvalid, member.Repository)
		}
	}
	return nil
}

// applyReviewFixAnswersTx records one answer for every selected pattern on all
// of its open findings, including those of patterns merged into it.
func applyReviewFixAnswersTx(ctx context.Context, tx *writeTx, taskID int64, selected map[int]bool, answers map[int]ReviewFixResolution) error {
	sequences := make([]int, 0, len(selected))
	for seq := range selected {
		sequences = append(sequences, seq)
	}
	sort.Ints(sequences)
	var reviewRound int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(round),0) FROM review_rounds WHERE ship_task_id=? AND status='verdict'`, taskID).Scan(&reviewRound); err != nil {
		return err
	}
	for _, seq := range sequences {
		answer, found := answers[seq]
		if !found {
			return fmt.Errorf("%w: missing answer for pattern P-%d", ErrReviewResolutionsRequired, seq)
		}
		var patternID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM review_patterns WHERE ship_task_id=? AND seq=?`, taskID, seq).Scan(&patternID); err != nil {
			return err
		}
		state, event := "fix_claimed", "fix_claimed"
		if answer.Status == "disputed" {
			state, event = "disputed", "disputed"
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM review_findings WHERE ship_task_id=? AND (pattern_id=? OR pattern_id IN (SELECT id FROM review_patterns WHERE merged_into=?)) AND state IN ('open','unverified','disputed','regressed','fix_claimed')`, taskID, patternID, patternID)
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
			return fmt.Errorf("%w: pattern P-%d has no open findings", ErrReviewResolutionsInvalid, seq)
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE review_findings SET state=? WHERE id=?`, state, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO review_finding_events(finding_id,round,event,actor,note,at) VALUES(?,?,?,'change_worker',?,?)`, id, reviewRound, event, answer.Note, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
	}
	return nil
}

func reviewPatternSequence(value string) (int, bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "P-") {
		return 0, false
	}
	var sequence int
	if _, err := fmt.Sscanf(value, "P-%d", &sequence); err != nil || sequence < 1 || fmt.Sprintf("P-%d", sequence) != value {
		return 0, false
	}
	return sequence, true
}

func reviewFixPatternSequences(dedupeKey, head string) (map[int]bool, error) {
	parts := strings.SplitN(dedupeKey, ":", 3)
	if len(parts) != 3 || parts[0] != "review" || parts[1] != head || parts[2] == "" {
		return nil, fmt.Errorf("invalid Review fix instruction binding")
	}
	selected := map[int]bool{}
	for _, value := range strings.Split(parts[2], ",") {
		seq, ok := reviewPatternSequence(value)
		if !ok || selected[seq] {
			return nil, fmt.Errorf("invalid selected pattern %q", value)
		}
		selected[seq] = true
	}
	return selected, nil
}
