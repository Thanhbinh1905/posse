package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ReviewRound binds one repository in a Review Task to an exact Change head.
type ReviewRound struct {
	ChangeTaskID    int64  `toml:"ship_task_id"`
	Round           int    `toml:"round"`
	ReviewTaskID    int64  `toml:"review_task_id"`
	Repository      string `toml:"repository"`
	ScopeKind       string `toml:"scope_kind"`
	ScopeBaseSHA    string `toml:"scope_base_sha"`
	HeadSHA         string `toml:"head_sha"`
	TargetCommitSHA string `toml:"target_commit_sha"`
	BaselineTree    string `toml:"baseline_tree"`
	DeltaSHA256     string `toml:"delta_sha256"`
	ConflictedPaths string `toml:"conflicted_paths_json"`
	Status          string `toml:"status"`
	Verdict         string `toml:"verdict"`
	CreatedAt       int64  `toml:"created_at"`
}

// ReviewPattern groups findings that share one root cause or violated rule.
type ReviewPattern struct {
	ID           int64  `toml:"id"`
	ChangeTaskID int64  `toml:"ship_task_id"`
	Seq          int    `toml:"seq"`
	Key          string `toml:"key"`
	Title        string `toml:"title"`
	Rule         string `toml:"rule"`
	RootCause    string `toml:"root_cause"`
	FixGuidance  string `toml:"fix_guidance"`
	MergedInto   int64  `toml:"merged_into"`
	CreatedRound int    `toml:"created_round"`
}

// ReviewFinding is one stable, anchored instance in a Change Task's ledger.
type ReviewFinding struct {
	ID            int64  `toml:"id"`
	ChangeTaskID  int64  `toml:"ship_task_id"`
	Seq           int    `toml:"seq"`
	PatternID     int64  `toml:"pattern_id"`
	Fingerprint   string `toml:"fingerprint"`
	Round         int    `toml:"round"`
	Repository    string `toml:"repository"`
	Path          string `toml:"path"`
	Symbol        string `toml:"symbol"`
	Excerpt       string `toml:"excerpt"`
	LineAtReport  int    `toml:"line_at_report"`
	HeadSHA       string `toml:"head_sha"`
	Severity      string `toml:"severity"`
	Origin        string `toml:"origin"`
	InScope       bool   `toml:"in_scope"`
	State         string `toml:"state"`
	FollowUpURL   string `toml:"follow_up_url"`
	InheritedFrom int64  `toml:"inherited_from"`
}

// ReviewFindingEvent is an append-only ledger event for a finding.
type ReviewFindingEvent struct {
	ID        int64  `toml:"id"`
	FindingID int64  `toml:"finding_id"`
	Round     int    `toml:"round"`
	Event     string `toml:"event"`
	Actor     string `toml:"actor"`
	Note      string `toml:"note"`
	UserQuote string `toml:"user_quote"`
	At        int64  `toml:"at"`
}

// ReviewCarry records proof that a clean target merge preserves a passed tree.
type ReviewCarry struct {
	ChangeTaskID    int64  `toml:"ship_task_id"`
	Repository      string `toml:"repository"`
	FromHead        string `toml:"from_head"`
	ToHead          string `toml:"to_head"`
	TargetCommitSHA string `toml:"target_commit_sha"`
	Tree            string `toml:"tree"`
	CreatedAt       int64  `toml:"created_at"`
}

// ReviewDisposition records a risk decision against one exact Change head.
type ReviewDisposition struct {
	ID           int64  `toml:"id"`
	ChangeTaskID int64  `toml:"ship_task_id"`
	HeadSHA      string `toml:"head_sha"`
	DecisionID   int64  `toml:"decision_id"`
	UserQuote    string `toml:"user_quote"`
	At           int64  `toml:"at"`
}

// ValidationFixRound records one queued automatic validation instruction.
type ValidationFixRound struct {
	ID               int64  `toml:"id"`
	ChangeTaskID     int64  `toml:"ship_task_id"`
	Kind             string `toml:"kind"`
	Seq              int    `toml:"seq"`
	ExpectedBaseHead string `toml:"expected_base_head"`
	DedupeKey        string `toml:"dedupe_key"`
	MessageID        int64  `toml:"message_id"`
	CreatedAt        int64  `toml:"created_at"`
}

type ReviewFindingDetail struct {
	FindingSeq    int
	PatternSeq    int
	PatternKey    string
	Title         string
	Rule          string
	RootCause     string
	FixGuidance   string
	Repository    string
	Path          string
	Symbol        string
	Excerpt       string
	Severity      string
	Origin        string
	InScope       bool
	State         string
	InheritedFrom int64
}

type ReviewFixResolution struct {
	Pattern string
	Status  string
	Note    string
}

type ReviewFixResolutionsInput struct {
	ExpectedBaseHead string
	ActualHead       string
	CandidateID      int64
	Patterns         []ReviewFixResolution
}

type ReviewEvidence struct {
	Complete    bool
	Pass        bool
	Carry       bool
	Disposition bool
}

type ReviewCapUsage struct {
	ReviewRounds int
	RepairRounds int
}

func (usage ReviewCapUsage) Total() int { return usage.ReviewRounds + usage.RepairRounds }

// QueueValidationFixResult describes whether an automatic instruction was added.
type QueueValidationFixResult struct {
	MessageID       int64
	Seq             int
	RoundsUsed      int
	Queued          bool
	Coalesced       bool
	Exhausted       bool
	SharedExhausted bool
}

// ValidationContinuitySnapshot binds a done Signal to the current fix rounds.
type ValidationContinuitySnapshot struct {
	Heads         []string
	LatestRoundID int64
}

// ErrValidationHeadsChanged means a new fix instruction arrived after continuity was checked.
var ErrValidationHeadsChanged = errors.New("validation fix instructions changed while recording done")

// QueueValidationFixInstruction records a fix round and queues or extends the
// unsent validation message for the same head in one transaction.
func (db *DB) QueueValidationFixInstruction(ctx context.Context, taskID int64, kind, expectedBaseHead, dedupeKey, body string) (QueueValidationFixResult, error) {
	return db.queueValidationFixInstruction(ctx, taskID, kind, expectedBaseHead, dedupeKey, body, nil, nil)
}

// QueueValidationFixInstructionWithinBudget records a fix round only while its
// per-kind budget remains. The budget check and insertion share one transaction.
func (db *DB) QueueValidationFixInstructionWithinBudget(ctx context.Context, taskID int64, kind, expectedBaseHead, dedupeKey, body string, maxRounds int) (QueueValidationFixResult, error) {
	if maxRounds < 0 {
		return QueueValidationFixResult{}, fmt.Errorf("validation fix round budget cannot be negative")
	}
	return db.queueValidationFixInstruction(ctx, taskID, kind, expectedBaseHead, dedupeKey, body, &maxRounds, nil)
}

// QueueValidationFixInstructionWithSharedBudget enforces the frozen per-kind
// limit and shared Review/Gate/CI cap in the same admission transaction.
func (db *DB) QueueValidationFixInstructionWithSharedBudget(ctx context.Context, taskID int64, kind, expectedBaseHead, dedupeKey, body string, maxRounds, sharedMax int) (QueueValidationFixResult, error) {
	if maxRounds < 0 || sharedMax < 1 {
		return QueueValidationFixResult{}, fmt.Errorf("invalid validation fix budget")
	}
	return db.queueValidationFixInstruction(ctx, taskID, kind, expectedBaseHead, dedupeKey, body, &maxRounds, &sharedMax)
}

func (db *DB) queueValidationFixInstruction(ctx context.Context, taskID int64, kind, expectedBaseHead, dedupeKey, body string, maxRounds, sharedMax *int) (QueueValidationFixResult, error) {
	if taskID < 1 || (kind != "gate" && kind != "ci" && kind != "review" && kind != "freshness") {
		return QueueValidationFixResult{}, fmt.Errorf("invalid validation fix task or kind")
	}
	if len(expectedBaseHead) != 40 && len(expectedBaseHead) != 64 {
		return QueueValidationFixResult{}, fmt.Errorf("expected base head must be a commit SHA")
	}
	if _, err := hex.DecodeString(expectedBaseHead); err != nil {
		return QueueValidationFixResult{}, fmt.Errorf("expected base head must be a hexadecimal commit SHA")
	}
	if strings.TrimSpace(dedupeKey) == "" || strings.TrimSpace(body) == "" {
		return QueueValidationFixResult{}, fmt.Errorf("validation fix dedupe key and body are required")
	}

	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return QueueValidationFixResult{}, err
	}
	defer tx.Rollback()
	var state, taskType string
	if err := tx.QueryRowContext(ctx, `SELECT state,type FROM tasks WHERE id=?`, taskID).Scan(&state, &taskType); err != nil {
		return QueueValidationFixResult{}, err
	}
	if taskType != "change" || !taskAcceptsInstruction(State(state), taskType) {
		return QueueValidationFixResult{}, ErrStateRace
	}

	var existingMessageID int64
	var existingExpected string
	var existingSeq int
	err = tx.QueryRowContext(ctx, `SELECT expected_base_head,message_id,seq FROM validation_fix_rounds WHERE ship_task_id=? AND kind=? AND dedupe_key=?`, taskID, kind, dedupeKey).Scan(&existingExpected, &existingMessageID, &existingSeq)
	if err == nil {
		if existingExpected != expectedBaseHead {
			return QueueValidationFixResult{}, fmt.Errorf("validation fix dedupe key %q was reused for another head", dedupeKey)
		}
		var roundsUsed int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind=?`, taskID, kind).Scan(&roundsUsed); err != nil {
			return QueueValidationFixResult{}, err
		}
		return QueueValidationFixResult{MessageID: existingMessageID, Seq: existingSeq, RoundsUsed: roundsUsed}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return QueueValidationFixResult{}, err
	}

	var roundsUsed, lastSeq int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(seq),0) FROM validation_fix_rounds WHERE ship_task_id=? AND kind=?`, taskID, kind).Scan(&roundsUsed, &lastSeq); err != nil {
		return QueueValidationFixResult{}, err
	}
	if maxRounds != nil && roundsUsed >= *maxRounds {
		return QueueValidationFixResult{RoundsUsed: roundsUsed, Exhausted: true}, nil
	}
	if sharedMax != nil {
		var reviews, repairs int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT round) FROM review_rounds WHERE ship_task_id=? AND status IN ('admitted','verdict')`, taskID).Scan(&reviews); err != nil {
			return QueueValidationFixResult{}, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind IN ('review','gate','ci')`, taskID).Scan(&repairs); err != nil {
			return QueueValidationFixResult{}, err
		}
		if reviews+repairs >= *sharedMax {
			return QueueValidationFixResult{RoundsUsed: reviews + repairs, SharedExhausted: true}, nil
		}
	}
	seq := lastSeq + 1
	var messageID int64
	var coalesced bool
	var existingBody string
	err = tx.QueryRowContext(ctx, `SELECT m.id,m.body FROM validation_fix_rounds r JOIN messages m ON m.id=r.message_id WHERE r.ship_task_id=? AND r.expected_base_head=? AND m.status='queued' ORDER BY m.id LIMIT 1`, taskID, expectedBaseHead).Scan(&messageID, &existingBody)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return QueueValidationFixResult{}, err
	}
	createdAt := time.Now().UnixMilli()
	var lastDone sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(at) FROM signals WHERE task_id=? AND verb='done'`, taskID).Scan(&lastDone); err != nil {
		return QueueValidationFixResult{}, err
	}
	if lastDone.Valid && createdAt <= lastDone.Int64 {
		createdAt = lastDone.Int64 + 1
	}
	if messageID != 0 {
		updated, err := tx.ExecContext(ctx, `UPDATE messages SET body=? WHERE id=? AND status='queued'`, existingBody+"\n\n---\n\n"+strings.TrimSpace(body), messageID)
		if err != nil {
			return QueueValidationFixResult{}, err
		}
		changed, err := updated.RowsAffected()
		if err != nil {
			return QueueValidationFixResult{}, err
		}
		if changed != 1 {
			return QueueValidationFixResult{}, ErrStateRace
		}
		coalesced = true
	} else {
		result, err := tx.ExecContext(ctx, `INSERT INTO messages(task_id,body,created_at,status,wait_for_idle) VALUES(?,?,?,'queued',1)`, taskID, strings.TrimSpace(body), createdAt)
		if err != nil {
			return QueueValidationFixResult{}, err
		}
		messageID, err = result.LastInsertId()
		if err != nil {
			return QueueValidationFixResult{}, err
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO validation_fix_rounds(ship_task_id,kind,seq,expected_base_head,dedupe_key,message_id,created_at) VALUES(?,?,?,?,?,?,?)`, taskID, kind, seq, expectedBaseHead, dedupeKey, messageID, createdAt)
	if err != nil {
		return QueueValidationFixResult{}, err
	}
	if _, err := result.LastInsertId(); err != nil {
		return QueueValidationFixResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return QueueValidationFixResult{}, err
	}
	if err := db.PersistTask(ctx, taskID); err != nil {
		return QueueValidationFixResult{}, err
	}
	return QueueValidationFixResult{MessageID: messageID, Seq: seq, RoundsUsed: roundsUsed + 1, Queued: true, Coalesced: coalesced}, nil
}

// ValidationContinuitySnapshot returns the heads named by automatic
// instructions issued since the Change Task's most recent done Signal, together
// with a revision that detects additional rounds on an already-known head.
func (db *DB) ValidationContinuitySnapshot(ctx context.Context, taskID int64) (ValidationContinuitySnapshot, error) {
	return validationContinuitySnapshot(ctx, db, taskID)
}

// ValidationExpectedBaseHeads returns the unique expected heads for callers
// that do not need to bind a done Signal to the current round revision.
func (db *DB) ValidationExpectedBaseHeads(ctx context.Context, taskID int64) ([]string, error) {
	snapshot, err := db.ValidationContinuitySnapshot(ctx, taskID)
	return snapshot.Heads, err
}

// OutstandingValidationFixHead returns the head named by the newest automatic
// fix instruction of kind issued since the Task's most recent done Signal, or
// "" when there is none.
func (db *DB) OutstandingValidationFixHead(ctx context.Context, taskID int64, kind string) (string, error) {
	var head string
	err := db.QueryRowContext(ctx, `SELECT r.expected_base_head FROM validation_fix_rounds r JOIN messages m ON m.id=r.message_id WHERE r.ship_task_id=? AND r.kind=? AND m.status IN ('queued','claimed','interrupting','submitting','delivered') AND m.created_at>COALESCE((SELECT MAX(at) FROM signals WHERE task_id=? AND verb='done'),0) ORDER BY r.id DESC LIMIT 1`, taskID, kind, taskID).Scan(&head)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return head, err
}

func validationContinuitySnapshotTx(ctx context.Context, tx *writeTx, taskID int64) (ValidationContinuitySnapshot, error) {
	return validationContinuitySnapshot(ctx, tx, taskID)
}

type validationRows interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func validationContinuitySnapshot(ctx context.Context, queryer validationRows, taskID int64) (ValidationContinuitySnapshot, error) {
	var snapshot ValidationContinuitySnapshot
	rows, err := queryer.QueryContext(ctx, `SELECT r.id,r.expected_base_head FROM validation_fix_rounds r JOIN messages m ON m.id=r.message_id WHERE r.ship_task_id=? AND m.status IN ('queued','claimed','interrupting','submitting','delivered') AND m.created_at>COALESCE((SELECT MAX(at) FROM signals WHERE task_id=? AND verb='done'),0) ORDER BY r.id`, taskID, taskID)
	if err != nil {
		return snapshot, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var id int64
		var head string
		if err := rows.Scan(&id, &head); err != nil {
			return snapshot, err
		}
		if id > snapshot.LatestRoundID {
			snapshot.LatestRoundID = id
		}
		if !seen[head] {
			seen[head] = true
			snapshot.Heads = append(snapshot.Heads, head)
		}
	}
	if err := rows.Err(); err != nil {
		return snapshot, err
	}
	sort.Strings(snapshot.Heads)
	return snapshot, nil
}

func sameValidationSnapshot(left, right ValidationContinuitySnapshot) bool {
	return left.LatestRoundID == right.LatestRoundID && sameValidationHeadList(left.Heads, right.Heads)
}

func sameValidationHeadList(left, right []string) bool {
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

type validationStateSnapshot struct {
	ReviewRounds        []ReviewRound
	ReviewPatterns      []ReviewPattern
	ReviewFindings      []ReviewFinding
	ReviewFindingEvents []ReviewFindingEvent
	ReviewCarries       []ReviewCarry
	ReviewDispositions  []ReviewDisposition
	ValidationFixRounds []ValidationFixRound
}

func (db *DB) validationStateForTask(ctx context.Context, taskID int64) (validationStateSnapshot, error) {
	var state validationStateSnapshot
	rows, err := db.QueryContext(ctx, `SELECT ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,verdict,created_at FROM review_rounds WHERE ship_task_id=? ORDER BY round,repository,review_task_id`, taskID)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var value ReviewRound
		var verdict sql.NullString
		if err := rows.Scan(&value.ChangeTaskID, &value.Round, &value.ReviewTaskID, &value.Repository, &value.ScopeKind, &value.ScopeBaseSHA, &value.HeadSHA, &value.TargetCommitSHA, &value.BaselineTree, &value.DeltaSHA256, &value.ConflictedPaths, &value.Status, &verdict, &value.CreatedAt); err != nil {
			_ = rows.Close()
			return state, err
		}
		value.Verdict = verdict.String
		state.ReviewRounds = append(state.ReviewRounds, value)
	}
	if err := closeSnapshotRows(rows); err != nil {
		return state, err
	}

	rows, err = db.QueryContext(ctx, `SELECT id,ship_task_id,seq,key,title,rule,root_cause,fix_guidance,merged_into,created_round FROM review_patterns WHERE ship_task_id=? ORDER BY seq`, taskID)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var value ReviewPattern
		var mergedInto sql.NullInt64
		if err := rows.Scan(&value.ID, &value.ChangeTaskID, &value.Seq, &value.Key, &value.Title, &value.Rule, &value.RootCause, &value.FixGuidance, &mergedInto, &value.CreatedRound); err != nil {
			_ = rows.Close()
			return state, err
		}
		value.MergedInto = mergedInto.Int64
		state.ReviewPatterns = append(state.ReviewPatterns, value)
	}
	if err := closeSnapshotRows(rows); err != nil {
		return state, err
	}

	rows, err = db.QueryContext(ctx, `SELECT id,ship_task_id,seq,pattern_id,fingerprint,round,repository,path,symbol,excerpt,line_at_report,head_sha,severity,origin,in_scope,state,follow_up_url,inherited_from FROM review_findings WHERE ship_task_id=? ORDER BY seq`, taskID)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var value ReviewFinding
		var inScope int
		var inheritedFrom sql.NullInt64
		if err := rows.Scan(&value.ID, &value.ChangeTaskID, &value.Seq, &value.PatternID, &value.Fingerprint, &value.Round, &value.Repository, &value.Path, &value.Symbol, &value.Excerpt, &value.LineAtReport, &value.HeadSHA, &value.Severity, &value.Origin, &inScope, &value.State, &value.FollowUpURL, &inheritedFrom); err != nil {
			_ = rows.Close()
			return state, err
		}
		value.InScope = inScope == 1
		value.InheritedFrom = inheritedFrom.Int64
		state.ReviewFindings = append(state.ReviewFindings, value)
	}
	if err := closeSnapshotRows(rows); err != nil {
		return state, err
	}

	rows, err = db.QueryContext(ctx, `SELECT e.id,e.finding_id,e.round,e.event,e.actor,e.note,e.user_quote,e.at FROM review_finding_events e JOIN review_findings f ON f.id=e.finding_id WHERE f.ship_task_id=? ORDER BY e.id`, taskID)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var value ReviewFindingEvent
		if err := rows.Scan(&value.ID, &value.FindingID, &value.Round, &value.Event, &value.Actor, &value.Note, &value.UserQuote, &value.At); err != nil {
			_ = rows.Close()
			return state, err
		}
		state.ReviewFindingEvents = append(state.ReviewFindingEvents, value)
	}
	if err := closeSnapshotRows(rows); err != nil {
		return state, err
	}

	rows, err = db.QueryContext(ctx, `SELECT ship_task_id,repository,from_head,to_head,target_commit_sha,tree,created_at FROM review_carries WHERE ship_task_id=? ORDER BY repository,from_head,to_head`, taskID)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var value ReviewCarry
		if err := rows.Scan(&value.ChangeTaskID, &value.Repository, &value.FromHead, &value.ToHead, &value.TargetCommitSHA, &value.Tree, &value.CreatedAt); err != nil {
			_ = rows.Close()
			return state, err
		}
		state.ReviewCarries = append(state.ReviewCarries, value)
	}
	if err := closeSnapshotRows(rows); err != nil {
		return state, err
	}

	rows, err = db.QueryContext(ctx, `SELECT id,ship_task_id,head_sha,decision_id,user_quote,at FROM review_dispositions WHERE ship_task_id=? ORDER BY id`, taskID)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var value ReviewDisposition
		var decisionID sql.NullInt64
		if err := rows.Scan(&value.ID, &value.ChangeTaskID, &value.HeadSHA, &decisionID, &value.UserQuote, &value.At); err != nil {
			_ = rows.Close()
			return state, err
		}
		value.DecisionID = decisionID.Int64
		state.ReviewDispositions = append(state.ReviewDispositions, value)
	}
	if err := closeSnapshotRows(rows); err != nil {
		return state, err
	}

	rows, err = db.QueryContext(ctx, `SELECT id,ship_task_id,kind,seq,expected_base_head,dedupe_key,message_id,created_at FROM validation_fix_rounds WHERE ship_task_id=? ORDER BY kind,seq`, taskID)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var value ValidationFixRound
		if err := rows.Scan(&value.ID, &value.ChangeTaskID, &value.Kind, &value.Seq, &value.ExpectedBaseHead, &value.DedupeKey, &value.MessageID, &value.CreatedAt); err != nil {
			_ = rows.Close()
			return state, err
		}
		state.ValidationFixRounds = append(state.ValidationFixRounds, value)
	}
	if err := closeSnapshotRows(rows); err != nil {
		return state, err
	}
	return state, nil
}

func closeSnapshotRows(rows *sql.Rows) error {
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close validation snapshot rows: %w", err)
	}
	return nil
}
