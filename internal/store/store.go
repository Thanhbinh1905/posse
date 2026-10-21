package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	dbgen "github.com/thanhbinh1905/posse/internal/store/sqlc"
	"github.com/thanhbinh1905/posse/internal/taskplan"
	"modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type DB struct {
	*sql.DB
	Path    string
	queries *dbgen.Queries
}

var ErrNotFound = sql.ErrNoRows
var ErrStateRace = errors.New("Task state changed concurrently")
var ErrMessageDeliveryInProgress = errors.New("a message delivery is already in progress")
var ErrQueuedInstructionsPending = errors.New("queued instructions remain undelivered")
var ErrStaleWorkerTurn = errors.New("done Signal belongs to a stale Worker turn")
var ErrTaskMergeInProgress = errors.New("Task merge has started or is in progress")
var ErrInvalidApprovedDiscardTransition = errors.New("approval-backed transition is only valid for User-approved discard")
var ErrBusy = errors.New("database contention; retry the operation")

const sqliteBusyTimeoutMillis = 5000
const storeOpenMigrationBudget = 30 * time.Second
const migrationLockWaitBudget = 5 * time.Second

// Observation lock waits stay short enough to defer contended work within
// the overall reconciliation budget.
const ObservationWriteBudget = 3 * time.Second
const ObservationWriteBusyTimeout = 250 * time.Millisecond
const reconcileWriteBusyTimeoutMillis = int(ObservationWriteBusyTimeout / time.Millisecond)

// IsBusy reports whether err represents SQLite lock contention, so callers can
// distinguish an invalid write from one that should be retried.
func IsBusy(err error) bool {
	return errors.Is(err, ErrBusy) || isBusyErr(err)
}

// IsOnlyBusy reports whether every constituent error represents transient
// store contention. Unlike IsBusy, it does not classify a joined error as
// transient when any constituent failure is non-contention.
func IsOnlyBusy(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return IsBusy(err)
		}
		for _, cause := range causes {
			if !IsOnlyBusy(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return IsOnlyBusy(wrapped.Unwrap())
	}
	return IsBusy(err)
}

// WithoutBusy removes transient contention failures from an error tree while
// retaining wrappers and non-contention failures for diagnostics.
func WithoutBusy(err error) error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var retained []error
		for _, cause := range joined.Unwrap() {
			if remaining := WithoutBusy(cause); remaining != nil {
				retained = append(retained, remaining)
			}
		}
		return errors.Join(retained...)
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		cause := wrapped.Unwrap()
		remaining := WithoutBusy(cause)
		if remaining == nil {
			return nil
		}
		prefix := strings.TrimSuffix(err.Error(), cause.Error())
		return fmt.Errorf("%s%w", prefix, remaining)
	}
	if IsBusy(err) {
		return nil
	}
	return err
}

// IsStorageError identifies SQLite and database/sql no-row errors so command
// boundaries can return a typed storage failure instead of an internal error.
func IsStorageError(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) || errors.Is(err, sql.ErrNoRows)
}

func isBusyErr(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) && (sqliteErr.Code()&0xff == 5 || sqliteErr.Code()&0xff == 6) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sqlite_busy") || strings.Contains(msg, "sqlite_locked") || strings.Contains(msg, "database is locked") || strings.Contains(msg, "database table is locked")
}

func retryableContention(err error) error {
	if err == nil || errors.Is(err, ErrBusy) {
		return err
	}
	if !isBusyErr(err) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrBusy, err)
}

func (db *DB) withBusyTimeout(ctx context.Context, timeoutMillis int, operation func(*sql.Conn) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return retryableContention(err)
	}
	timeoutMillis = min(timeoutMillis, writeBusyTimeout(ctx))
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout="+strconv.Itoa(timeoutMillis)); err != nil {
		return errors.Join(retryableContention(err), restoreBusyTimeoutAndClose(conn))
	}
	return errors.Join(retryableContention(operation(conn)), restoreBusyTimeoutAndClose(conn))
}

func Open(home string) (*DB, error) {
	if home == "" {
		return nil, fmt.Errorf("POSSE_HOME is empty")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("create POSSE_HOME: %w", err)
	}
	path := filepath.Join(home, "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		return nil, err
	}
	_ = db.BackupDaily(time.Now())
	return db, nil
}

func OpenAt(path string) (*DB, error) {
	return openAt(path, storeOpenMigrationBudget)
}

func openAt(path string, timeout time.Duration) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	captureTemplate := seedNewStore(path)
	fileURL := (&url.URL{Scheme: "file", Path: path}).String()
	// WAL with synchronous=NORMAL stays consistent after a crash and syncs only
	// at checkpoints, so commits do not wait on fsync behind other writers.
	dsn := fileURL + "?_pragma=busy_timeout(" + strconv.Itoa(sqliteBusyTimeoutMillis) + ")&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(4)
	database.SetMaxIdleConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	result := &DB{DB: database, Path: path}
	result.queries = dbgen.New(result)
	if err := result.MigrateIfNeeded(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	if captureTemplate {
		result.captureNewStoreTemplate(ctx)
	}
	return result, nil
}

func (db *DB) MigrateIfNeeded(ctx context.Context) (migrationErr error) {
	defer func() { migrationErr = retryableContention(migrationErr) }()
	sources, err := embeddedMigrations()
	if err != nil {
		return err
	}
	if db.schemaCurrent(ctx, sources) {
		return nil
	}
	lockCtx, cancelLock := context.WithTimeout(ctx, migrationLockWaitBudget)
	lock, err := acquireMigrationLock(lockCtx, db.Path)
	cancelLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return err
	}
	if !strings.EqualFold(journalMode, "wal") {
		if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
			return err
		}
	}
	if err := db.normalizeLegacyNoticeDeliveryMigration(ctx); err != nil {
		return err
	}
	if err := db.normalizeLegacyProjectUUIDMigration(ctx); err != nil {
		return err
	}
	if err := db.normalizeLegacyReportBundleMigration(ctx); err != nil {
		return err
	}
	if err := db.normalizeLegacyVocabularyMigration(ctx); err != nil {
		return err
	}
	if err := db.normalizeLegacyTaskContainmentMigration(ctx); err != nil {
		return err
	}
	if err := db.normalizeLegacyTaskPaneShellIdentityMigration(ctx); err != nil {
		return err
	}
	if err := db.checkSchema(ctx, sources); err != nil {
		return err
	}
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		provider, err := goose.NewProvider(goose.DialectSQLite3, db.DB, migrations, goose.WithAllowOutofOrder(true))
		if err != nil {
			return err
		}
		missing, err := missingMigrationVersions(ctx, provider)
		if err != nil {
			lastErr = err
			if !strings.Contains(strings.ToLower(err.Error()), "locked") && !strings.Contains(strings.ToLower(err.Error()), "busy") {
				return err
			}
		} else if results, err := provider.Up(ctx); err == nil {
			if len(missing) > 0 {
				missingSet := make(map[int64]bool, len(missing))
				for _, version := range missing {
					missingSet[version] = true
				}
				applied := make([]string, 0, len(missing))
				for _, result := range results {
					if missingSet[result.Source.Version] {
						applied = append(applied, strconv.FormatInt(result.Source.Version, 10))
					}
				}
				if len(applied) > 0 {
					log.Printf("posse: applied missing migrations: %s", strings.Join(applied, ", "))
				}
			}
			return db.recordChecksums(ctx, sources)
		} else {
			lastErr = err
			if !strings.Contains(strings.ToLower(err.Error()), "locked") && !strings.Contains(strings.ToLower(err.Error()), "busy") {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 25 * time.Millisecond):
		}
	}
	return fmt.Errorf("migrate database after concurrent access: %w", lastErr)
}

func missingMigrationVersions(ctx context.Context, provider *goose.Provider) ([]int64, error) {
	current, err := provider.GetDBVersion(ctx)
	if err != nil {
		return nil, err
	}
	statuses, err := provider.Status(ctx)
	if err != nil {
		return nil, err
	}
	missing := make([]int64, 0)
	for _, status := range statuses {
		if status.State == goose.StatePending && status.Source.Version < current {
			missing = append(missing, status.Source.Version)
		}
	}
	return missing, nil
}

type State string

const (
	StateSpawning      State = "spawning"
	StateWorking       State = "working"
	StateNeedsDecision State = "needs-decision"
	StateBlocked       State = "blocked"
	StateStalled       State = "stalled"
	StateDone          State = "done"
	StateMerging       State = "merging"
	StateMerged        State = "merged"
	StateReported      State = "reported"
	StateFailed        State = "failed"
	StateLost          State = "lost"
	StateTornDown      State = "torn-down"
)

// IsApprovedDiscardSourceState reports whether User approval can authorize
// tearing down a Task from this state. Keep this predicate centralized so the
// allowed set can expand without changing each discard path independently.
func IsApprovedDiscardSourceState(state State) bool {
	return oneOfState(state, StateSpawning, StateWorking, StateNeedsDecision, StateBlocked, StateStalled, StateDone, StateMerging, StateFailed, StateLost, StateReported, StateMerged)
}

func taskAcceptsInstruction(state State, taskType string) bool {
	if oneOfState(state, StateSpawning, StateWorking, StateNeedsDecision, StateBlocked, StateStalled) {
		return true
	}
	return taskType == "change" && oneOfState(state, StateDone, StateMerging)
}

var transitions = map[State]map[State]bool{
	StateSpawning:      {StateWorking: true, StateFailed: true, StateLost: true, StateMerged: true, StateTornDown: true},
	StateWorking:       {StateNeedsDecision: true, StateFailed: true, StateBlocked: true, StateStalled: true, StateDone: true, StateLost: true, StateMerged: true, StateTornDown: true},
	StateNeedsDecision: {StateWorking: true, StateFailed: true, StateLost: true, StateMerged: true, StateTornDown: true},
	StateBlocked:       {StateWorking: true, StateFailed: true, StateLost: true, StateMerged: true, StateTornDown: true},
	StateStalled:       {StateWorking: true, StateTornDown: true, StateLost: true, StateMerged: true},
	StateDone:          {StateMerging: true, StateMerged: true, StateReported: true, StateWorking: true, StateTornDown: true},
	StateMerging:       {StateMerged: true, StateDone: true, StateWorking: true, StateTornDown: true},
	StateMerged:        {StateTornDown: true},
	StateReported:      {StateTornDown: true},
	StateFailed:        {StateWorking: true, StateTornDown: true, StateMerged: true},
	StateLost:          {StateWorking: true, StateTornDown: true, StateMerged: true},
}

func (db *DB) Transition(ctx context.Context, taskID int64, expected, next State, source, note string) error {
	if !transitions[expected][next] {
		return fmt.Errorf("transition %q -> %q is not allowed", expected, next)
	}
	if !oneOfSource(source) {
		return fmt.Errorf("invalid transition source %q", source)
	}
	if err := validateTransitionSource(expected, next, source); err != nil {
		return err
	}
	if next == StateTornDown && source == "user" && IsApprovedDiscardSourceState(expected) {
		return fmt.Errorf("approved discard requires a recorded User approval")
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := transitionTx(ctx, db.queries.WithTx(tx.Tx), taskID, expected, next, source, note); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

// TransitionLostUnlessTeardownIntent gives an admitted Teardown ownership of
// the Task's terminal transition. The intent check and state change share one
// transaction so reconciliation cannot race an intent inserted concurrently.
func (db *DB) TransitionLostUnlessTeardownIntent(ctx context.Context, taskID int64, expected State, source, note string) (bool, error) {
	if !transitions[expected][StateLost] {
		return false, fmt.Errorf("transition %q -> %q is not allowed", expected, StateLost)
	}
	if !oneOfSource(source) {
		return false, fmt.Errorf("invalid transition source %q", source)
	}
	if err := validateTransitionSource(expected, StateLost, source); err != nil {
		return false, err
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var hasTeardown int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM intents WHERE task_id=? AND command='teardown')`, taskID).Scan(&hasTeardown); err != nil {
		return false, err
	}
	if hasTeardown != 0 {
		return false, nil
	}
	if err := transitionTx(ctx, db.queries.WithTx(tx.Tx), taskID, expected, StateLost, source, note); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if err := db.PersistTask(ctx, taskID); err != nil {
		return false, err
	}
	return true, nil
}

func (db *DB) TransitionInPlacePreserved(ctx context.Context, task Task, expected State, note, quote string) error {
	quote = strings.TrimSpace(quote)
	if quote == "" {
		return fmt.Errorf("preserving failed or lost in-place work requires a recorded User choice")
	}
	if !transitions[expected][StateTornDown] {
		return fmt.Errorf("transition %q -> %q is not allowed", expected, StateTornDown)
	}
	if err := validateTransitionSource(expected, StateTornDown, "user"); err != nil {
		return err
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, planJSON string
	if err := tx.QueryRowContext(ctx, `SELECT state, plan_json FROM tasks WHERE id=?`, task.ID).Scan(&state, &planJSON); err != nil {
		return err
	}
	if State(state) != expected {
		return fmt.Errorf("%w: Task %d is %q, expected %q", ErrStateRace, task.ID, state, expected)
	}
	plan, err := task.AdmittedPlan()
	if err != nil {
		return err
	}
	storedPlan, err := taskplan.Decode(planJSON)
	if err != nil {
		return err
	}
	if plan == nil || plan.Execution.Mode != "in-place" || storedPlan.Execution.Mode != "in-place" {
		return fmt.Errorf("preserving recovery requires an admitted in-place plan")
	}
	note = strings.TrimSpace(note)
	if note != "" {
		note += "; "
	}
	note += "User approval: " + quote
	if err := transitionTx(ctx, db.queries.WithTx(tx.Tx), task.ID, expected, StateTornDown, "user", note); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, task.ID)
}

func (db *DB) TransitionWithApproval(ctx context.Context, taskID int64, expected, next State, source, note, action, quote string) error {
	if next != StateTornDown || !IsApprovedDiscardSourceState(expected) || source != "user" || action != "discard" {
		return ErrInvalidApprovedDiscardTransition
	}
	if strings.TrimSpace(quote) == "" {
		return fmt.Errorf("user approval quote is required")
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := db.queries.WithTx(tx.Tx)
	if err := queries.InsertApproval(ctx, dbgen.InsertApprovalParams{TaskID: taskID, Action: action, UserQuote: quote, At: time.Now().UnixMilli()}); err != nil {
		return err
	}
	if err := transitionTx(ctx, queries, taskID, expected, next, source, note); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) RecordApproval(ctx context.Context, taskID int64, action, quote string) error {
	if strings.TrimSpace(quote) == "" {
		return fmt.Errorf("user approval quote is required")
	}
	return db.queries.InsertApproval(ctx, dbgen.InsertApprovalParams{TaskID: taskID, Action: action, UserQuote: quote, At: time.Now().UnixMilli()})
}

func (db *DB) RecordApprovalWithBranch(ctx context.Context, taskID int64, action, quote, branchSHA string) error {
	if strings.TrimSpace(quote) == "" {
		return fmt.Errorf("user approval quote is required")
	}
	_, err := db.ExecContext(ctx, `INSERT INTO approvals(task_id,action,user_quote,branch_sha,at) VALUES(?,?,?,?,?)`, taskID, action, quote, branchSHA, time.Now().UnixMilli())
	return err
}

func (db *DB) LatestApprovalBranchSHA(ctx context.Context, taskID int64, action string) (string, error) {
	var branchSHA string
	err := db.QueryRowContext(ctx, `SELECT branch_sha FROM approvals WHERE task_id=? AND action=? ORDER BY at DESC,id DESC LIMIT 1`, taskID, action).Scan(&branchSHA)
	return branchSHA, err
}

func (db *DB) HasApproval(ctx context.Context, taskID int64, action string) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE task_id=? AND action=?`, taskID, action).Scan(&count)
	return count > 0, err
}

func (db *DB) LatestApprovalQuote(ctx context.Context, taskID int64, action string) (string, error) {
	var quote string
	err := db.QueryRowContext(ctx, `SELECT user_quote FROM approvals WHERE task_id=? AND action=? ORDER BY at DESC,id DESC LIMIT 1`, taskID, action).Scan(&quote)
	return quote, err
}

func (db *DB) TransitionAfterApproval(ctx context.Context, taskID int64, expected, next State, source, note, action string) error {
	if next != StateTornDown || !IsApprovedDiscardSourceState(expected) || source != "user" || action != "discard" {
		return ErrInvalidApprovedDiscardTransition
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	count, err := db.queries.WithTx(tx.Tx).CountApproval(ctx, dbgen.CountApprovalParams{TaskID: taskID, Action: action})
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("recorded User approval for %q is required", action)
	}
	if err := transitionTx(ctx, db.queries.WithTx(tx.Tx), taskID, expected, next, source, note); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func transitionTx(ctx context.Context, queries *dbgen.Queries, taskID int64, expected, next State, source, note string) error {
	now := time.Now().UnixMilli()
	result, err := queries.UpdateTaskState(ctx, dbgen.UpdateTaskStateParams{State: string(next), UpdatedAt: now, ID: taskID, State_2: string(expected)})
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%w: Task %d is not in %q", ErrStateRace, taskID, expected)
	}
	if err := queries.InsertTransition(ctx, dbgen.InsertTransitionParams{TaskID: taskID, FromState: string(expected), ToState: string(next), Source: source, Note: note, At: now}); err != nil {
		return err
	}
	if next == StateWorking {
		if err := queries.ResetTaskProgress(ctx, dbgen.ResetTaskProgressParams{UpdatedAt: now, ID: taskID}); err != nil {
			return err
		}
	}
	if expected == StateMerging && next == StateDone {
		if err := queries.ClearGatedSHAMergeToDone(ctx, taskID); err != nil {
			return err
		}
	}
	return queries.TouchProjectForTask(ctx, dbgen.TouchProjectForTaskParams{LastActivityAt: now, ID: taskID})
}

func validateTransitionSource(from, to State, source string) error {
	expected := "cli"
	switch {
	case (to == StateDone && from != StateMerging) || (from == StateWorking && (to == StateNeedsDecision || to == StateFailed)):
		expected = "worker"
	case from == StateWorking && to == StateBlocked || from == StateBlocked && to == StateWorking:
		expected = "herdr"
	case from == StateNeedsDecision && to == StateWorking:
		expected = "lead"
		if source == "worker" {
			expected = source
		}
	case (from == StateDone || from == StateMerging) && to == StateWorking:
		expected = "lead"
		if source == "watcher" || source == "worker" {
			expected = source
		}
	case from == StateStalled && to == StateWorking:
		if source == "worker" {
			expected = "worker"
		} else if source != "cli" {
			expected = "worker"
		}
	case (from == StateFailed || from == StateLost) && to == StateWorking:
		expected = "cli"
	case to == StateTornDown && source == "user" && IsApprovedDiscardSourceState(from):
		expected = "user"
	case to == StateTornDown && oneOfState(from, StateSpawning, StateWorking, StateNeedsDecision, StateBlocked, StateStalled, StateDone, StateMerging, StateFailed, StateLost):
		expected = "user"
	}
	if source != expected {
		return fmt.Errorf("transition %q -> %q requires source %q, got %q", from, to, expected, source)
	}
	return nil
}

func oneOfState(value State, candidates ...State) bool {
	for _, candidate := range candidates {
		if value == candidate {
			return true
		}
	}
	return false
}

func (db *DB) CreateTask(ctx context.Context, projectID int64, task Task) (int64, error) {
	planJSON, err := bindTaskPlan(task, task.Seq, "")
	if err != nil {
		return 0, err
	}
	now := time.Now().UnixMilli()
	if task.State == "" {
		task.State = StateSpawning
	}
	if task.State != StateSpawning {
		return 0, fmt.Errorf("new Task must begin in spawning")
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	queries := db.queries.WithTx(tx.Tx)
	result, err := queries.InsertTask(ctx, dbgen.InsertTaskParams{
		ProjectID: projectID, Seq: int64(task.Seq), Type: task.Type, ReviewsTaskID: nullableID(task.ReviewsTaskID),
		Title: task.Title, PrTitle: task.PRTitle, ShortName: task.ShortName, State: string(task.State), Profile: task.Profile, DispatchRule: task.DispatchRule,
		MergeMode: task.MergeMode, AutonomyReview: task.AutonomyReview, AutonomyMerge: task.AutonomyMerge,
		Branch: task.Branch, BaseRef: task.BaseRef, TargetBranch: task.TargetBranch, WorktreePath: task.WorktreePath,
		HerdrWorkspaceID: task.HerdrWorkspaceID, PaneID: task.PaneID, PaneLabel: task.PaneLabel,
		AgentName: task.AgentName, AgentSession: task.AgentSession, AgentSessionKind: task.AgentSessionKind, PrUrl: task.PRURL, MergedRef: task.MergedRef,
		LastOutputHash: task.LastOutputHash, LastWorktreeHash: task.LastWorktreeHash,
		LastProgressAt: task.LastProgressAt, AgentAbsentSince: task.AgentAbsentSince, IdleSince: task.IdleSince, CreatedAt: now, UpdatedAt: now, PlanJson: planJSON,
		ReportKind: task.ReportKind, PaneShellIdentity: task.PaneShellIdentity,
	})
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if planJSON != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,1,?)`, id, planJSON); err != nil {
			return 0, err
		}
	}
	if len(task.ReviewTargets) != 0 {
		if task.Type != "review" {
			return 0, ErrReviewVerdictType
		}
		if err := insertReviewTargets(ctx, tx, id, task.ReviewTargets); err != nil {
			return 0, err
		}
	}
	if err := queries.InsertTransition(ctx, dbgen.InsertTransitionParams{TaskID: id, FromState: "", ToState: string(StateSpawning), Source: "cli", Note: "Task created", At: now}); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if err := db.PersistTask(ctx, id); err != nil {
		return 0, err
	}
	return id, nil
}

func nullableID(id int64) sql.NullInt64 {
	if id == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: id, Valid: true}
}

func oneOfSource(source string) bool {
	switch source {
	case "worker", "herdr", "cli", "lead", "user":
		return true
	default:
		return false
	}
}

type TaskLaunchIdentity struct {
	TaskID          int64  `toml:"task_id" json:"task_id"`
	LaunchNumber    int    `toml:"launch_number" json:"launch_number"`
	PlanRevision    int    `toml:"plan_revision,omitempty" json:"plan_revision"`
	Profile         string `toml:"profile" json:"profile"`
	ConfiguredModel string `toml:"configured_model" json:"configured_model"`
	ModelKnown      bool   `toml:"model_known" json:"model_known"`
}

type Task struct {
	ID             int64  `toml:"id"`
	ProjectID      int64  `toml:"project_id"`
	Seq            int    `toml:"seq"`
	Type           string `toml:"type"`
	ReviewsTaskID  int64  `toml:"reviews_task_id"`
	Title          string `toml:"title"`
	PRTitle        string `toml:"pr_title,omitempty"`
	ShortName      string `toml:"short_name"`
	State          State  `toml:"state"`
	Profile        string `toml:"profile"`
	DispatchRule   string `toml:"dispatch_rule"`
	PlanJSON       string `toml:"plan_json,omitempty"`
	MergeMode      string `toml:"landing_mode"`
	AutonomyReview string `toml:"autonomy_review"`
	AutonomyMerge  string `toml:"autonomy_land"`
	Branch         string `toml:"branch"`
	BaseRef        string `toml:"base_ref"`
	TargetBranch   string `toml:"target_branch"`
	WorktreePath   string `toml:"worktree_path"`
	SandboxID      int64  `toml:"mount_id"`
	Launches       int    `toml:"launches"`
	// ReviewTargets is read only when a Review Task is created; the store keeps
	// the bound commits in review_targets.
	ReviewTargets        []ReviewTarget `toml:"-"`
	GatedSHA             string         `toml:"gated_sha"`
	HerdrWorkspaceID     string         `toml:"herdr_workspace_id"`
	PaneID               string         `toml:"pane_id"`
	PaneLabel            string         `toml:"pane_label"`
	PaneShellIdentity    string         `toml:"pane_shell_identity,omitempty"`
	AgentName            string         `toml:"agent_name"`
	AgentSession         string         `toml:"agent_session"`
	AgentSessionKind     string         `toml:"agent_session_kind"`
	AgentAbsentSince     int64          `toml:"agent_absent_since"`
	IdleSince            int64          `toml:"idle_since"`
	AgentServerStartedAt string         `toml:"agent_server_started_at"`
	PRURL                string         `toml:"pr_url"`
	MergedRef            string         `toml:"landed_ref"`
	LastOutputHash       string         `toml:"last_output_hash"`
	LastWorktreeHash     string         `toml:"last_worktree_hash"`
	LastProgressAt       int64          `toml:"last_progress_at"`
	CreatedAt            int64          `toml:"created_at"`
	UpdatedAt            int64          `toml:"updated_at"`
	ReportKind           string         `toml:"report_kind,omitempty"`
	ReportDigest         string         `toml:"report_digest,omitempty"`
	ReportManifestJSON   string         `toml:"report_manifest_json,omitempty"`
	ContainmentMode      string         `toml:"containment_mode,omitempty"`
	ContainmentPath      string         `toml:"containment_path,omitempty"`
	ContainmentMarker    string         `toml:"containment_marker,omitempty"`
}

type Sandbox struct {
	ID         int64
	ProjectID  int64
	Number     int
	Path       string
	State      string
	TaskID     int64
	AcquiredAt int64
	ReleasedAt int64
}

func (db *DB) CreateTaskWithSequence(ctx context.Context, projectID int64, projectName string, task Task, occupied func(int) (bool, error)) (int64, int, error) {
	return db.createTaskWithSequence(ctx, projectID, projectName, task, "", 0, occupied, nil)
}

func (db *DB) CreateTaskWithSequenceAndIntent(ctx context.Context, projectID int64, projectName string, task Task, command string, processID int, occupied func(int) (bool, error)) (int64, int, error) {
	if command == "" || processID < 1 {
		return 0, 0, fmt.Errorf("intent command and process id are required")
	}
	return db.createTaskWithSequence(ctx, projectID, projectName, task, command, processID, occupied, nil)
}

// CreateReviewRoundTaskWithSequenceAndIntent atomically creates a Review Task
// and admits all of its repository bindings as one numbered round.
func (db *DB) CreateReviewRoundTaskWithSequenceAndIntent(ctx context.Context, projectID int64, projectName string, task Task, command string, processID int, admission ReviewRoundAdmission, occupied func(int) (bool, error)) (int64, int, error) {
	if command == "" || processID < 1 {
		return 0, 0, fmt.Errorf("intent command and process id are required")
	}
	if task.Type != "review" || task.ReviewsTaskID != admission.ChangeTaskID {
		return 0, 0, fmt.Errorf("review round task does not target its admitted Change Task")
	}
	return db.createTaskWithSequence(ctx, projectID, projectName, task, command, processID, occupied, &admission)
}

var ErrTaskBranchExists = errors.New("Task branch already used")
var ErrTaskBranchSequenceMismatch = errors.New("Task branch does not match allocated Task sequence")

// TaskBranchName returns the stable Git branch for a Task sequence.
func TaskBranchName(sequence int) string {
	return "posse/t" + strconv.Itoa(sequence)
}

func taskBranchSequence(branch string) (int, bool) {
	const prefix = "posse/t"
	if !strings.HasPrefix(branch, prefix) {
		return 0, false
	}
	digits := strings.TrimPrefix(branch, prefix)
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, false
	}
	sequence, err := strconv.Atoi(digits)
	return sequence, err == nil && sequence > 0 && strconv.Itoa(sequence) == digits
}

func (db *DB) TaskBranchExists(ctx context.Context, projectID int64, branch string) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE project_id=? AND branch=?)`, projectID, branch).Scan(&exists)
	return exists, err
}

func (db *DB) createTaskWithSequence(ctx context.Context, projectID int64, projectName string, task Task, intentCommand string, processID int, occupied func(int) (bool, error), roundAdmission *ReviewRoundAdmission) (int64, int, error) {
	if task.ShortName == "" {
		return 0, 0, fmt.Errorf("Task short name is required")
	}
	inPlace := false
	if plan, err := task.AdmittedPlan(); err != nil {
		return 0, 0, err
	} else if plan != nil && plan.Execution.Mode == "in-place" {
		inPlace = true
	}
	now := time.Now().UnixMilli()
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var id int64
	var seq int
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET last_activity_at=last_activity_at WHERE id=?`, projectID); err != nil {
		return 0, 0, err
	}
	if roundAdmission != nil {
		if err := validateReviewRoundAdmissionTx(ctx, tx, *roundAdmission); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM tasks WHERE project_id=?`, projectID).Scan(&seq); err != nil {
		return 0, 0, err
	}
	for {
		busy, err := occupied(seq)
		if err != nil {
			return 0, 0, err
		}
		if !busy {
			break
		}
		seq++
	}
	if task.Branch == "" && !inPlace {
		task.Branch = TaskBranchName(seq)
	}
	if task.Branch != "" {
		if branchSequence, ok := taskBranchSequence(task.Branch); ok && branchSequence != seq {
			return 0, 0, ErrTaskBranchSequenceMismatch
		}
		var branchUsed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE project_id=? AND branch=?)`, projectID, task.Branch).Scan(&branchUsed); err != nil {
			return 0, 0, err
		}
		if branchUsed {
			return 0, 0, ErrTaskBranchExists
		}
	}
	workflowRunID := ""
	if admitted, err := task.AdmittedPlan(); err != nil {
		return 0, 0, err
	} else if admitted != nil && admitted.Workflow != nil {
		if admitted.Workflow.RunID == "new" {
			workflowRunID, err = nextWorkflowRunIDTx(ctx, tx.Tx, projectID)
			if err != nil {
				return 0, 0, err
			}
		} else if err := validateWorkflowTaskAdmissionTx(ctx, tx.Tx, projectID, task.Type, admitted.Workflow); err != nil {
			return 0, 0, err
		}
	}
	planJSON, err := bindTaskPlan(task, seq, workflowRunID)
	if err != nil {
		return 0, 0, err
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO tasks(project_id,seq,type,reviews_task_id,title,short_name,state,profile,dispatch_rule,landing_mode,autonomy_review,autonomy_land,branch,base_ref,target_branch,worktree_path,pane_label,created_at,updated_at,plan_json,report_kind)
VALUES (?, ?, ?, ?, ?, ?, 'spawning', ?, ?, ?, ?, ?, ?, ?, ?, ?, 'posse:' || ? || ':t' || ?, ?, ?, ?, ?) RETURNING id`,
		projectID, seq, task.Type, nullableID(task.ReviewsTaskID), task.Title, task.ShortName, task.Profile, task.DispatchRule,
		task.MergeMode, task.AutonomyReview, task.AutonomyMerge, task.Branch, task.BaseRef, task.TargetBranch, task.WorktreePath,
		projectName, seq, now, now, planJSON, task.ReportKind).Scan(&id)
	if err != nil {
		return 0, 0, err
	}
	if planJSON != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,1,?)`, id, planJSON); err != nil {
			return 0, 0, err
		}
	}
	if len(task.ReviewTargets) != 0 {
		if task.Type != "review" {
			return 0, 0, ErrReviewVerdictType
		}
		if err := insertReviewTargets(ctx, tx, id, task.ReviewTargets); err != nil {
			return 0, 0, err
		}
	}
	if roundAdmission != nil {
		if err := insertReviewRoundBindingsTx(ctx, tx, id, *roundAdmission); err != nil {
			return 0, 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions(task_id,from_state,to_state,source,note,at) VALUES (?, '', 'spawning', 'cli', 'Task created', ?)`, id, now); err != nil {
		return 0, 0, err
	}
	if intentCommand != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO intents(project_id,task_id,command,step,process_id,updated_at,payload_json) VALUES(?,?,?,'done:task.create',?,?,'{}')`, projectID, id, intentCommand, processID, now); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	if err := db.PersistTask(ctx, id); err != nil {
		return 0, 0, err
	}
	if roundAdmission != nil {
		if err := db.PersistTask(ctx, roundAdmission.ChangeTaskID); err != nil {
			return 0, 0, err
		}
	}
	return id, seq, nil
}

func (db *DB) AcquireSandbox(ctx context.Context, projectID, taskID int64, basePath string) (Sandbox, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return Sandbox{}, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	var sandbox Sandbox
	err = tx.QueryRowContext(ctx, `UPDATE mounts SET state='held',task_id=?,acquired_at=?,released_at=0 WHERE id=(SELECT id FROM mounts WHERE project_id=? AND state='idle' ORDER BY n LIMIT 1) AND state='idle' RETURNING id,project_id,n,path,state,task_id,acquired_at,released_at`, taskID, now, projectID).Scan(&sandbox.ID, &sandbox.ProjectID, &sandbox.Number, &sandbox.Path, &sandbox.State, &sandbox.TaskID, &sandbox.AcquiredAt, &sandbox.ReleasedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id,acquired_at,released_at) SELECT ?,COALESCE(MAX(n),0)+1,? || '/sandbox-' || (COALESCE(MAX(n),0)+1),'held',?,?,0 FROM mounts WHERE project_id=? RETURNING id,project_id,n,path,state,task_id,acquired_at,released_at`, projectID, basePath, taskID, now, projectID).Scan(&sandbox.ID, &sandbox.ProjectID, &sandbox.Number, &sandbox.Path, &sandbox.State, &sandbox.TaskID, &sandbox.AcquiredAt, &sandbox.ReleasedAt)
	}
	if err != nil {
		return Sandbox{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET mount_id=?,worktree_path=?,updated_at=? WHERE id=?`, sandbox.ID, sandbox.Path, now, taskID); err != nil {
		return Sandbox{}, err
	}
	if err := tx.Commit(); err != nil {
		return Sandbox{}, err
	}
	if err := db.PersistTask(ctx, taskID); err != nil {
		return Sandbox{}, err
	}
	return sandbox, nil
}

// BeginSandboxRelease records that the checkout has been reset and no longer
// contains Task work. Its Git lock may now be removed without ever exposing
// an unlocked checkout that the database still describes as held.
func (db *DB) BeginSandboxRelease(ctx context.Context, sandboxID, taskID int64) error {
	var projectID int64
	if err := db.QueryRowContext(ctx, `SELECT project_id FROM mounts WHERE id=? AND task_id=? AND state='held'`, sandboxID, taskID).Scan(&projectID); err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE mounts SET state='releasing' WHERE id=? AND task_id=? AND state='held'`, sandboxID, taskID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrStateRace
	}
	return db.PersistProject(ctx, projectID)
}

// FinishSandboxRelease is retryable after a crash. A failed Task snapshot write
// may follow the committed idle transition; callers must never relock it.
func (db *DB) FinishSandboxRelease(ctx context.Context, sandboxID, taskID int64) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `UPDATE mounts SET state='idle',task_id=NULL,released_at=? WHERE id=? AND task_id=? AND state='releasing'`, now, sandboxID, taskID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrStateRace
	}
	result, err = tx.ExecContext(ctx, `UPDATE tasks SET mount_id=NULL,updated_at=? WHERE id=? AND mount_id=?`, now, taskID, sandboxID)
	if err != nil {
		return err
	}
	count, err = result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrStateRace
	}
	if err := db.releaseSandboxLeaseTx(ctx, tx, sandboxID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) UpdateTaskSandboxPath(ctx context.Context, taskID, sandboxID int64, path string) error {
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET mount_id=NULLIF(?,0),worktree_path=?,updated_at=? WHERE id=?`, sandboxID, path, time.Now().UnixMilli(), taskID); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) UpdateTaskWorkspace(ctx context.Context, taskID int64, workspaceID, paneID string) error {
	if err := db.queries.UpdateTaskWorkspace(ctx, dbgen.UpdateTaskWorkspaceParams{HerdrWorkspaceID: workspaceID, PaneID: paneID, UpdatedAt: time.Now().UnixMilli(), ID: taskID}); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) RepairBrokenSandbox(ctx context.Context, sandboxID int64) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var projectID int64
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM mounts WHERE id=? AND state='broken' AND task_id IS NULL`, sandboxID).Scan(&projectID); err != nil {
		if err == sql.ErrNoRows {
			return ErrStateRace
		}
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE mounts SET state='idle',task_id=NULL,released_at=? WHERE id=? AND state='broken' AND task_id IS NULL`, time.Now().UnixMilli(), sandboxID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrStateRace
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM resource_leases WHERE mount_id=?`, sandboxID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistProject(ctx, projectID)
}

func (db *DB) BreakSandbox(ctx context.Context, sandboxID, taskID int64) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `UPDATE mounts SET state='broken',task_id=NULL,released_at=? WHERE id=? AND task_id=? AND state='held'`, now, sandboxID, taskID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrStateRace
	}
	result, err = tx.ExecContext(ctx, `UPDATE tasks SET mount_id=NULL,updated_at=? WHERE id=? AND mount_id=?`, now, taskID, sandboxID)
	if err != nil {
		return err
	}
	count, err = result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrStateRace
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) ClaimSandboxPrune(ctx context.Context, projectID, sandboxID int64) (bool, error) {
	result, err := db.ExecContext(ctx, `UPDATE mounts SET state='pruning' WHERE id=? AND project_id=? AND state IN ('idle','broken')`, sandboxID, projectID)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (db *DB) RestoreSandboxAfterPrune(ctx context.Context, sandboxID int64, state string) error {
	if state != "idle" && state != "broken" {
		return fmt.Errorf("invalid Sandbox state after prune: %q", state)
	}
	result, err := db.ExecContext(ctx, `UPDATE mounts SET state=? WHERE id=? AND state='pruning'`, state, sandboxID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return ErrStateRace
	}
	return err
}

func (db *DB) DeletePrunedSandbox(ctx context.Context, projectID, sandboxID int64) error {
	result, err := db.ExecContext(ctx, `DELETE FROM mounts WHERE id=? AND project_id=? AND state='pruning'`, sandboxID, projectID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrStateRace
	}
	return db.PersistProject(ctx, projectID)
}

func (db *DB) Sandboxes(ctx context.Context, projectID int64) ([]Sandbox, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,project_id,n,path,state,COALESCE(task_id,0),acquired_at,released_at FROM mounts WHERE project_id=? ORDER BY n`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sandboxes := []Sandbox{}
	for rows.Next() {
		var sandbox Sandbox
		if err := rows.Scan(&sandbox.ID, &sandbox.ProjectID, &sandbox.Number, &sandbox.Path, &sandbox.State, &sandbox.TaskID, &sandbox.AcquiredAt, &sandbox.ReleasedAt); err != nil {
			return nil, err
		}
		sandboxes = append(sandboxes, sandbox)
	}
	return sandboxes, rows.Err()
}

func (db *DB) SandboxByTask(ctx context.Context, taskID int64) (Sandbox, error) {
	var sandbox Sandbox
	err := db.QueryRowContext(ctx, `SELECT id,project_id,n,path,state,COALESCE(task_id,0),acquired_at,released_at FROM mounts WHERE task_id=?`, taskID).Scan(&sandbox.ID, &sandbox.ProjectID, &sandbox.Number, &sandbox.Path, &sandbox.State, &sandbox.TaskID, &sandbox.AcquiredAt, &sandbox.ReleasedAt)
	return sandbox, err
}

func (db *DB) SetTaskGatedSHA(ctx context.Context, taskID int64, sha string) error {
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET gated_sha=?,updated_at=? WHERE id=? AND state='done'`, sha, time.Now().UnixMilli(), taskID); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

// ClearTaskGatedSHA forgets the commit the Gate passed, for the Task and for
// each workspace member not already merging through a pull request.
func (db *DB) ClearTaskGatedSHA(ctx context.Context, taskID int64) error {
	now := time.Now().UnixMilli()
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET gated_sha='',updated_at=? WHERE id=?`, now, taskID); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE task_repos SET state='open',gated_sha='',updated_at=? WHERE task_id=? AND state IN ('gated','unchanged')`, now, taskID); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

type Project struct {
	ID               int64  `toml:"id"`
	UUID             string `toml:"uuid" json:"-"`
	Name             string `toml:"name"`
	Root             string `toml:"root"`
	DefaultBranch    string `toml:"default_branch"`
	Kind             string `toml:"kind"`
	HerdrWorkspaceID string `toml:"herdr_workspace_id"`
	LeadPaneID       string `toml:"lead_pane_id"`
	LeadLabel        string `toml:"lead_label"`
	LeadLaunches     int    `toml:"lead_launches"`
	LeadAbsentSince  int64  `toml:"lead_absent_since"`
	Status           string `toml:"status"`
	DownAt           int64  `toml:"down_at"`
	CreatedAt        int64  `toml:"created_at"`
	LastActivityAt   int64  `toml:"last_activity_at"`
}

func (db *DB) NextTaskLaunch(ctx context.Context, taskID int64) (int, error) {
	var profile string
	if err := db.QueryRowContext(ctx, `SELECT profile FROM tasks WHERE id=?`, taskID).Scan(&profile); err != nil {
		return 0, err
	}
	return db.NextTaskLaunchWithIdentity(ctx, taskID, profile, "", false)
}

// NextTaskLaunchWithIdentity atomically allocates a launch number and records
// the configured identity that Posse supplied to that launch. modelKnown is
// false when the configured model is absent or an override prevents Posse from
// identifying the effective configured value.
func (db *DB) NextTaskLaunchWithIdentity(ctx context.Context, taskID int64, profile, configuredModel string, modelKnown bool) (int, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	var launches int
	if err := tx.QueryRowContext(ctx, `UPDATE tasks SET launches=launches+1,updated_at=? WHERE id=? RETURNING launches`, now, taskID).Scan(&launches); err != nil {
		return 0, err
	}
	if modelKnown && strings.TrimSpace(configuredModel) == "" {
		modelKnown = false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_launch_identities(task_id,launch_number,profile_name,configured_model,model_known,plan_revision) SELECT id,?,?,?,?,CASE WHEN plan_json='' THEN 0 ELSE json_extract(plan_json,'$.revision') END FROM tasks WHERE id=?`, launches, profile, configuredModel, modelKnown, taskID); err != nil {
		return 0, err
	}
	if err := setTaskTurnTokenTx(ctx, tx, taskID, fmt.Sprintf("launch:%d", launches), now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return launches, db.PersistTask(ctx, taskID)
}

// TaskLaunchIdentities returns the configured Profile and model values recorded
// for each launch. Missing rows remain visible through the caller's comparison
// of these rows with Task.Launches; they are not reconstructed from current config.
func setTaskTurnTokenTx(ctx context.Context, tx *writeTx, taskID int64, token string, at int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO task_turn_tokens(task_id,token,updated_at) VALUES(?,?,?)
		ON CONFLICT(task_id) DO UPDATE SET token=excluded.token,updated_at=excluded.updated_at`, taskID, token, at)
	return err
}

func (db *DB) TaskTurnToken(ctx context.Context, taskID int64) (string, error) {
	var token string
	err := db.QueryRowContext(ctx, `SELECT token FROM task_turn_tokens WHERE task_id=?`, taskID).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return token, err
}

func (db *DB) TaskLaunchIdentities(ctx context.Context, taskID int64) ([]TaskLaunchIdentity, error) {
	rows, err := db.QueryContext(ctx, `SELECT task_id,launch_number,profile_name,configured_model,model_known,plan_revision FROM task_launch_identities WHERE task_id=? ORDER BY launch_number`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	identities := []TaskLaunchIdentity{}
	for rows.Next() {
		var identity TaskLaunchIdentity
		var modelKnown int
		if err := rows.Scan(&identity.TaskID, &identity.LaunchNumber, &identity.Profile, &identity.ConfiguredModel, &modelKnown, &identity.PlanRevision); err != nil {
			return nil, err
		}
		identity.ModelKnown = modelKnown == 1
		identities = append(identities, identity)
	}
	return identities, rows.Err()
}

func (db *DB) NextLeadLaunch(ctx context.Context, projectID int64) (int, error) {
	var launches int
	err := db.QueryRowContext(ctx, `UPDATE projects SET lead_launches=lead_launches+1 WHERE id=? RETURNING lead_launches`, projectID).Scan(&launches)
	if err != nil {
		return 0, err
	}
	return launches, db.PersistProject(ctx, projectID)
}

func (db *DB) ClearLead(ctx context.Context, projectID int64) error {
	if _, err := db.ExecContext(ctx, `UPDATE projects SET lead_pane_id='',lead_label='',lead_absent_since=0 WHERE id=?`, projectID); err != nil {
		return err
	}
	return db.PersistProject(ctx, projectID)
}

func (db *DB) CreateProject(ctx context.Context, name, root, defaultBranch string) (Project, error) {
	uuid, err := newProjectUUID()
	if err != nil {
		return Project{}, err
	}
	now := time.Now().UnixMilli()
	result, err := db.queries.InsertProject(ctx, dbgen.InsertProjectParams{Name: name, Root: root, DefaultBranch: defaultBranch, CreatedAt: now, LastActivityAt: now, ProjectUuid: uuid})
	if err != nil {
		return Project{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Project{}, err
	}
	project := Project{ID: id, UUID: uuid, Name: name, Root: root, DefaultBranch: defaultBranch, Kind: ProjectKindRepo, Status: "active", CreatedAt: now, LastActivityAt: now}
	if err := db.PersistProject(ctx, id); err != nil {
		return Project{}, err
	}
	return project, nil
}

// IsDown reports that the User stopped this Project with `posse down`.
// Nothing restarts its Lead or Workers until `posse up`.
func (p Project) IsDown() bool { return p.DownAt != 0 }

func (db *DB) MarkProjectDown(ctx context.Context, projectID int64) error {
	if err := db.queries.MarkProjectDown(ctx, dbgen.MarkProjectDownParams{DownAt: time.Now().UnixMilli(), ID: projectID}); err != nil {
		return err
	}
	return db.PersistProject(ctx, projectID)
}

// ClearDownProjectLead forgets the stopped Lead pane of a down Project.
func (db *DB) ClearDownProjectLead(ctx context.Context, projectID int64) error {
	if err := db.queries.ClearDownProjectLead(ctx, projectID); err != nil {
		return err
	}
	return db.PersistProject(ctx, projectID)
}

func (db *DB) SetProjectLead(ctx context.Context, projectID int64, workspaceID, paneID, label string) error {
	if err := db.queries.SetProjectLead(ctx, dbgen.SetProjectLeadParams{HerdrWorkspaceID: workspaceID, LeadPaneID: paneID, LeadLabel: label, LastActivityAt: time.Now().UnixMilli(), ID: projectID}); err != nil {
		return err
	}
	return db.PersistProject(ctx, projectID)
}

func (db *DB) SetProjectWorkspace(ctx context.Context, projectID int64, workspaceID string) error {
	if _, err := db.ExecContext(ctx, `UPDATE projects SET herdr_workspace_id=?,lead_pane_id='',lead_label='',lead_absent_since=0 WHERE id=?`, workspaceID, projectID); err != nil {
		return err
	}
	return db.PersistProject(ctx, projectID)
}

type LeadStartClaim struct {
	ClaimedAt int64
	Holder    string
	PID       int64
}

func (db *DB) ClaimLeadStart(ctx context.Context, projectID int64, now, staleBefore int64, holder string, pid int) (bool, error) {
	result, err := db.queries.InsertLeadStartClaim(ctx, dbgen.InsertLeadStartClaimParams{ProjectID: projectID, ClaimedAt: now, Holder: holder, Pid: int64(pid)})
	if err != nil {
		return false, retryableContention(err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows == 1 {
		return rows == 1, err
	}
	claim, err := db.queries.LeadStartClaim(ctx, projectID)
	if err != nil {
		return false, retryableContention(err)
	}
	if claim.ClaimedAt > staleBefore && processAlive(int(claim.Pid)) {
		return false, nil
	}
	result, err = db.queries.ReclaimLeadStartClaim(ctx, dbgen.ReclaimLeadStartClaimParams{
		ClaimedAt: now, Holder: holder, Pid: int64(pid), ProjectID: projectID,
		ClaimedAt_2: claim.ClaimedAt, Holder_2: claim.Holder, Pid_2: claim.Pid,
	})
	if err != nil {
		return false, retryableContention(err)
	}
	rows, err = result.RowsAffected()
	return rows == 1, err
}

func (db *DB) LeadStartClaim(ctx context.Context, projectID int64) (LeadStartClaim, error) {
	claim, err := db.queries.LeadStartClaim(ctx, projectID)
	return LeadStartClaim{ClaimedAt: claim.ClaimedAt, Holder: claim.Holder, PID: claim.Pid}, err
}

func (db *DB) ReleaseLeadStart(ctx context.Context, projectID int64, holder string, pid int) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		err = retryableContention(db.queries.ReleaseLeadStart(ctx, dbgen.ReleaseLeadStartParams{ProjectID: projectID, Holder: holder, Pid: int64(pid)}))
		if !errors.Is(err, ErrBusy) {
			return err
		}
		if attempt == 3 {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
	return fmt.Errorf("release Lead start claim after retries: %w", err)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); err == nil || !os.IsNotExist(err) {
		return true
	}
	// On systems without procfs, retain the time-based lease rather than
	// risking reclaiming a live process's claim.
	if _, err := os.Stat("/proc"); os.IsNotExist(err) {
		return true
	}
	return false
}

func (db *DB) MoveProject(ctx context.Context, projectID int64, newRoot, defaultBranch string) error {
	if err := db.queries.MoveProject(ctx, dbgen.MoveProjectParams{Root: newRoot, DefaultBranch: defaultBranch, LastActivityAt: time.Now().UnixMilli(), ID: projectID}); err != nil {
		return err
	}
	if err := db.PersistProject(ctx, projectID); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM tasks WHERE project_id=? ORDER BY id`, projectID)
	if err != nil {
		return err
	}
	var taskIDs []int64
	for rows.Next() {
		var taskID int64
		if err := rows.Scan(&taskID); err != nil {
			_ = rows.Close()
			return err
		}
		taskIDs = append(taskIDs, taskID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, taskID := range taskIDs {
		if err := db.persistTask(ctx, taskID, false); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) NextTaskSeq(ctx context.Context, projectID int64) (int, error) {
	sequence, err := db.queries.NextTaskSeq(ctx, projectID)
	return int(sequence), err
}

func (db *DB) ActiveWorkerCount(ctx context.Context, projectID int64) (int, error) {
	count, err := db.queries.ActiveWorkerCount(ctx, projectID)
	return int(count), err
}

func (db *DB) UpdateTaskLaunch(ctx context.Context, taskID int64, worktreePath, workspaceID, paneID, paneLabel, agentName string) error {
	if err := db.queries.UpdateTaskLaunch(ctx, dbgen.UpdateTaskLaunchParams{WorktreePath: worktreePath, HerdrWorkspaceID: workspaceID, PaneID: paneID, PaneLabel: paneLabel, AgentName: agentName, UpdatedAt: time.Now().UnixMilli(), ID: taskID}); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) ChangeTaskProfile(ctx context.Context, taskID int64, oldProfile, newProfile string) error {
	if oldProfile == "" || newProfile == "" || oldProfile == newProfile {
		return fmt.Errorf("old and new Profiles must be non-empty and different")
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id=? AND profile=?`, taskID, oldProfile).Scan(&state); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET profile=?, dispatch_rule=?, updated_at=? WHERE id=? AND profile=?`, newProfile, "relaunch --profile "+newProfile, now, taskID, oldProfile)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil {
		return err
	} else if count != 1 {
		return fmt.Errorf("%w: Task %d Profile changed concurrently", ErrStateRace, taskID)
	}
	note := fmt.Sprintf("Profile changed from %s to %s", oldProfile, newProfile)
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions(task_id,from_state,to_state,source,note,at) VALUES(?,?,?,?,?,?)`, taskID, state, state, "lead", note, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET last_activity_at=? WHERE id=(SELECT project_id FROM tasks WHERE id=?)`, now, taskID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) UpdateTaskMerge(ctx context.Context, taskID int64, prURL, mergedRef string) error {
	if err := db.queries.UpdateTaskMerge(ctx, dbgen.UpdateTaskMergeParams{PrUrl: prURL, MergedRef: mergedRef, UpdatedAt: time.Now().UnixMilli(), ID: taskID}); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) SetTaskPRTitle(ctx context.Context, taskID int64, title string) error {
	result, err := db.queries.SetTaskPRTitle(ctx, dbgen.SetTaskPRTitleParams{PrTitle: title, UpdatedAt: time.Now().UnixMilli(), ID: taskID})
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrNotFound
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) AddSignal(ctx context.Context, taskID int64, verb, note string, data any) (int64, error) {
	dataJSON := "{}"
	if data != nil {
		encoded, err := json.Marshal(data)
		if err != nil {
			return 0, err
		}
		dataJSON = string(encoded)
	}
	result, err := db.queries.InsertSignal(ctx, dbgen.InsertSignalParams{TaskID: taskID, Verb: verb, Note: note, DataJson: dataJSON, At: time.Now().UnixMilli()})
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (db *DB) RecordWorkerSignal(ctx context.Context, task Task, verb, note string, data any, noticeKind string) (State, error) {
	return db.RecordWorkerSignalWithVerdict(ctx, task, verb, note, data, noticeKind, nil)
}

// RecordWorkerSignalWithVerdict records a Signal and, for a Review Task's done
// Signal, its verdict in one transaction.
func (db *DB) RecordWorkerSignalWithVerdict(ctx context.Context, task Task, verb, note string, data any, noticeKind string, verdict *ReviewVerdictInput) (State, error) {
	return db.recordWorkerSignal(ctx, task, verb, note, data, noticeKind, verdict, nil, nil)
}

// RecordWorkerSignalWithResolutions records the Worker's pattern-level Review
// fix answers with the done Signal in the same transaction.
func (db *DB) RecordWorkerSignalWithResolutions(ctx context.Context, task Task, verb, note string, data any, noticeKind string, verdict *ReviewVerdictInput, resolutions *ReviewFixResolutionsInput) (State, error) {
	return db.recordWorkerSignal(ctx, task, verb, note, data, noticeKind, verdict, nil, resolutions)
}

// RecordWorkerSignalWithValidation records a Change done Signal only if the
// automatic validation instructions checked by the caller have not changed.
func (db *DB) RecordWorkerSignalWithValidation(ctx context.Context, task Task, verb, note string, data any, noticeKind string, validation ValidationContinuitySnapshot) (State, error) {
	return db.recordWorkerSignal(ctx, task, verb, note, data, noticeKind, nil, &validation, nil)
}

// RecordWorkerSignalWithValidationAndResolutions atomically checks instruction
// continuity and records Review fix answers with the Worker's done Signal.
func (db *DB) RecordWorkerSignalWithValidationAndResolutions(ctx context.Context, task Task, verb, note string, data any, noticeKind string, validation ValidationContinuitySnapshot, resolutions *ReviewFixResolutionsInput) (State, error) {
	return db.recordWorkerSignal(ctx, task, verb, note, data, noticeKind, nil, &validation, resolutions)
}

func workerTurnTokenFromData(data any) string {
	switch value := data.(type) {
	case map[string]any:
		token, _ := value["turn_token"].(string)
		return token
	case map[string]string:
		return value["turn_token"]
	default:
		return ""
	}
}

func (db *DB) recordWorkerSignal(ctx context.Context, task Task, verb, note string, data any, noticeKind string, verdict *ReviewVerdictInput, validationHeads *ValidationContinuitySnapshot, resolutions *ReviewFixResolutionsInput) (State, error) {
	inPlace := false
	if verb == "done" {
		plan, err := task.AdmittedPlan()
		if err != nil {
			return "", err
		}
		inPlace = plan != nil && plan.Execution.Mode == "in-place"
	}
	if verdict != nil && verb != "done" {
		return "", fmt.Errorf("a review verdict is valid only on a done Signal")
	}
	if resolutions != nil && (verb != "done" || task.Type != "change") {
		return "", fmt.Errorf("review fix resolutions are valid only on a Change done Signal")
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	queries := db.queries.WithTx(tx.Tx)
	var state, taskType string
	if err := tx.QueryRowContext(ctx, `SELECT state,type FROM tasks WHERE id=?`, task.ID).Scan(&state, &taskType); err != nil {
		return "", err
	}
	current := State(state)
	if verb == "done" {
		var currentTurn string
		err := tx.QueryRowContext(ctx, `SELECT token FROM task_turn_tokens WHERE task_id=?`, task.ID).Scan(&currentTurn)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		if currentTurn != "" {
			submittedTurn := workerTurnTokenFromData(data)
			if currentTurn != "launch:1" || submittedTurn != "" {
				if submittedTurn != currentTurn {
					return "", ErrStaleWorkerTurn
				}
			}
		}
	}
	if verb == "done" && taskType == "change" && validationHeads != nil {
		currentHeads, err := validationContinuitySnapshotTx(ctx, tx, task.ID)
		if err != nil {
			return "", err
		}
		if !sameValidationSnapshot(currentHeads, *validationHeads) {
			return "", ErrValidationHeadsChanged
		}
	}
	previousState := current
	workerResumed := false
	watcherTransitionAt := int64(0)
	if verb == "working" && current == StateWorking && taskType == "change" {
		var transitionID, transitionAt int64
		var fromState, source string
		err := tx.QueryRowContext(ctx, `SELECT id,from_state,source,at FROM transitions WHERE task_id=? AND to_state='working' ORDER BY id DESC LIMIT 1`, task.ID).Scan(&transitionID, &fromState, &source, &transitionAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		if err == nil && source == "watcher" && (fromState == string(StateDone) || fromState == string(StateMerging)) {
			var alreadySignaled bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM signals WHERE task_id=? AND verb='working' AND at>?)`, task.ID, transitionAt).Scan(&alreadySignaled); err != nil {
				return "", err
			}
			if !alreadySignaled {
				if err := cancelPendingMergeIntentTx(ctx, tx, task.ID); err != nil {
					return "", err
				}
				result, err := tx.ExecContext(ctx, `UPDATE transitions SET source='worker',note=? WHERE id=? AND source='watcher'`, note, transitionID)
				if err != nil {
					return "", err
				}
				changed, err := result.RowsAffected()
				if err != nil {
					return "", err
				}
				if changed != 1 {
					return "", ErrStateRace
				}
				previousState = State(fromState)
				watcherTransitionAt = transitionAt
				workerResumed = true
			}
		}
	}
	completedChange := taskType == "change" && oneOfState(current, StateDone, StateMerging)
	if !workerResumed {
		if verb == "working" && (completedChange || oneOfState(current, StateStalled, StateNeedsDecision)) {
			if completedChange {
				if err := cancelPendingMergeIntentTx(ctx, tx, task.ID); err != nil {
					return "", err
				}
			}
			if err := transitionTx(ctx, queries, task.ID, current, StateWorking, "worker", note); err != nil {
				return "", err
			}
			previousState = current
			current = StateWorking
			workerResumed = true
		} else {
			if current == StateStalled {
				if err := transitionTx(ctx, queries, task.ID, StateStalled, StateWorking, "worker", "Worker resumed with a Signal"); err != nil {
					return "", err
				}
				current = StateWorking
			}
			if current != StateWorking {
				return "", fmt.Errorf("%w: Task %d is %q", ErrStateRace, task.ID, current)
			}
		}
	}
	if workerResumed {
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET gated_sha='' WHERE id=?`, task.ID); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE task_repos SET gated_sha='',state=CASE WHEN state IN ('gated','unchanged','merging') THEN 'open' ELSE state END,updated_at=? WHERE task_id=? AND state<>'merged'`, time.Now().UnixMilli(), task.ID); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM completed_rider_busy WHERE task_id=?`, task.ID); err != nil {
			return "", err
		}
		if taskType == "change" && oneOfState(previousState, StateDone, StateMerging) {
			if _, err := tx.ExecContext(ctx, `UPDATE decisions SET obsolete_at=?,obsolete_reason='Worker reopened Task' WHERE task_id=? AND kind='merge_ready' AND answered_at=0 AND obsolete_at=0`, time.Now().UnixMilli(), task.ID); err != nil {
				return "", err
			}
		}
	}
	if verb == "done" {
		pending, err := hasPendingTaskInstructions(ctx, tx.Tx, task.ID)
		if err != nil {
			return "", err
		}
		if pending {
			return "", ErrQueuedInstructionsPending
		}
	}
	if verb == "done" && taskType == "change" {
		if err := applyReviewFixResolutionsTx(ctx, tx, task.ID, resolutions); err != nil {
			return "", err
		}
	}
	signalAt := time.Now().UnixMilli()
	if verb == "done" && taskType == "change" && validationHeads != nil {
		var latestInstruction sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(m.created_at) FROM messages m JOIN validation_fix_rounds r ON r.message_id=m.id WHERE r.ship_task_id=?`, task.ID).Scan(&latestInstruction); err != nil {
			return "", err
		}
		if latestInstruction.Valid && signalAt <= latestInstruction.Int64 {
			signalAt = latestInstruction.Int64 + 1
		}
	}
	signal, err := queries.InsertSignal(ctx, dbgen.InsertSignalParams{TaskID: task.ID, Verb: verb, Note: note, DataJson: string(encoded), At: signalAt})
	if err != nil {
		return "", err
	}
	if verb == "working" {
		signalID, err := signal.LastInsertId()
		if err != nil {
			return "", err
		}
		if err := setTaskTurnTokenTx(ctx, tx, task.ID, fmt.Sprintf("signal:%d", signalID), signalAt); err != nil {
			return "", err
		}
	}
	if verdict != nil {
		signalID, err := signal.LastInsertId()
		if err != nil {
			return "", err
		}
		if err := recordReviewVerdictTx(ctx, tx, task.ID, signalID, *verdict); err != nil {
			return "", err
		}
	}
	next := StateWorking
	switch verb {
	case "needs-decision":
		next = StateNeedsDecision
	case "failed":
		next = StateFailed
	case "done":
		next = StateDone
	case "working":
	default:
		return "", fmt.Errorf("unknown Signal %q", verb)
	}
	if next != StateWorking {
		if err := transitionTx(ctx, queries, task.ID, StateWorking, next, "worker", note); err != nil {
			return "", err
		}
		current = next
		if err := blockModelErrorEpisodeTx(ctx, tx, task.ID, "Task transitioned to "+string(next)+" before automatic continue submission", time.Now().UnixMilli()); err != nil {
			return "", err
		}
	}
	if verb == "done" && task.Type == "change" && !inPlace {
		var taskType, paneID, server string
		var transitionID int64
		if err := tx.QueryRowContext(ctx, `SELECT type,pane_id,agent_server_started_at,COALESCE((SELECT id FROM transitions WHERE task_id=tasks.id AND to_state='done' ORDER BY id DESC LIMIT 1),0) FROM tasks WHERE id=?`, task.ID).Scan(&taskType, &paneID, &server, &transitionID); err != nil {
			return "", err
		}
		if taskType == "change" {
			if err := upsertCompletedWorkerTurn(ctx, tx, task.ID, transitionID, paneID, server, time.Now().UnixMilli(), false); err != nil {
				return "", err
			}
		}
	}
	if verb == "done" && (task.Type != "change" || inPlace) {
		reason := "Worker Report copied to the Project home"
		if inPlace {
			reason = "in-place Task output recorded without a Merge step"
		}
		if err := transitionTx(ctx, queries, task.ID, StateDone, StateReported, "cli", reason); err != nil {
			return "", err
		}
		current = StateReported
	}
	noticeSummary := note
	noticeDataJSON := string(encoded)
	if workerResumed {
		noticeKind = "worker_resumed"
		noticeSummary = task.Title + ": Worker resumed: " + strconv.Quote(note)
		noticeData, marshalErr := json.Marshal(map[string]any{"previous_state": previousState, "reason": note})
		if marshalErr != nil {
			return "", marshalErr
		}
		noticeDataJSON = string(noticeData)
	} else if noticeKind == "worker_resumed" {
		noticeKind = ""
	}
	if noticeKind != "" {
		updatedWatcherNotice := false
		if workerResumed && watcherTransitionAt > 0 {
			var noticeID int64
			var priorDataJSON string
			err := tx.QueryRowContext(ctx, `SELECT id,data_json FROM notices WHERE task_id=? AND kind='worker_resumed' AND delivered_at=0 AND created_at>=? ORDER BY id DESC LIMIT 1`, task.ID, watcherTransitionAt).Scan(&noticeID, &priorDataJSON)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return "", err
			}
			if err == nil {
				var noticeData map[string]any
				if err := json.Unmarshal([]byte(priorDataJSON), &noticeData); err != nil {
					return "", err
				}
				noticeData["previous_state"] = previousState
				noticeData["reason"] = note
				encodedNotice, err := json.Marshal(noticeData)
				if err != nil {
					return "", err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE notices SET summary=?,data_json=? WHERE id=? AND delivered_at=0`, noticeSummary, string(encodedNotice), noticeID); err != nil {
					return "", err
				}
				updatedWatcherNotice = true
			}
		}
		if !updatedWatcherNotice {
			if _, err := tx.ExecContext(ctx, `INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at) VALUES(?,?,?,?,?,?)`, task.ProjectID, task.ID, noticeKind, noticeSummary, noticeDataJSON, time.Now().UnixMilli()); err != nil {
				return "", err
			}
		}
	}
	if verb == "done" && data != nil {
		prURL := ""
		switch values := data.(type) {
		case map[string]string:
			prURL = values["pr_url"]
		case map[string]any:
			prURL, _ = values["pr_url"].(string)
		}
		if prURL != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET pr_url=? WHERE id=?`, prURL, task.ID); err != nil {
				return "", err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if err := db.PersistTask(ctx, task.ID); err != nil {
		log.Printf("posse: Signal %q committed for Task %d, but recovery snapshot refresh failed: %v", verb, task.ID, err)
	}
	if verdict != nil && verdict.Findings != nil && task.ReviewsTaskID > 0 {
		if err := db.PersistTask(ctx, task.ReviewsTaskID); err != nil {
			log.Printf("posse: Review findings committed for Change Task %d, but recovery snapshot refresh failed: %v", task.ReviewsTaskID, err)
		}
	}
	if resolutions != nil {
		if err := db.PersistTask(ctx, task.ID); err != nil {
			log.Printf("posse: Review fix resolutions committed for Task %d, but recovery snapshot refresh failed: %v", task.ID, err)
		}
	}
	return current, nil
}

func cancelPendingMergeIntentTx(ctx context.Context, tx *writeTx, taskID int64) error {
	var intentID int64
	var command, step string
	err := tx.QueryRowContext(ctx, `SELECT id,command,step FROM intents WHERE task_id=?`, taskID).Scan(&intentID, &command, &step)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if command != "merge --execute" && command != "merge --open-pr" {
		return fmt.Errorf("%w: %s", ErrIntentOwned, command)
	}
	step = strings.TrimPrefix(step, "resuming:")
	step = strings.TrimPrefix(strings.TrimPrefix(step, "in_progress:"), "done:")
	if step == "merge" || strings.HasPrefix(step, "merge:") || step == "pr.merge" || strings.HasPrefix(step, "pr.merge:") {
		return ErrTaskMergeInProgress
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM intents WHERE id=? AND task_id=?`, intentID, taskID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrIntentOwned
	}
	return nil
}

func (db *DB) TaskSignals(ctx context.Context, taskID int64, limit int) ([]Signal, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := db.queries.TaskSignals(ctx, dbgen.TaskSignalsParams{TaskID: taskID, Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	signals := make([]Signal, 0, len(rows))
	for _, row := range rows {
		signals = append(signals, Signal{ID: row.ID, TaskID: row.TaskID, Verb: row.Verb, Note: row.Note, DataJSON: row.DataJson, At: row.At})
	}
	return signals, nil
}

func (db *DB) TaskMessagesAfter(ctx context.Context, taskID int64, createdAt int64, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 33
	}
	rows, err := db.QueryContext(ctx, `SELECT id, task_id, body, created_at, COALESCE(delivered_at, 0), status, wait_for_idle
FROM messages WHERE task_id=? AND status='delivered' AND created_at>=? ORDER BY created_at DESC, id DESC LIMIT ?`, taskID, createdAt, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []Message
	for rows.Next() {
		var message Message
		if err := rows.Scan(&message.ID, &message.TaskID, &message.Body, &message.CreatedAt, &message.DeliveredAt, &message.Status, &message.WaitForIdle); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
	return messages, nil
}

func (db *DB) TaskTransitions(ctx context.Context, taskID int64, limit int) ([]TransitionRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.queries.TaskTransitions(ctx, dbgen.TaskTransitionsParams{TaskID: taskID, Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	transitions := make([]TransitionRecord, 0, len(rows))
	for _, row := range rows {
		transitions = append(transitions, TransitionRecord{ID: row.ID, TaskID: row.TaskID, From: row.FromState, To: row.ToState, Source: row.Source, Note: row.Note, At: row.At})
	}
	return transitions, nil
}

type Signal struct {
	ID       int64
	TaskID   int64
	Verb     string
	Note     string
	DataJSON string
	At       int64
}

type TransitionRecord struct {
	ID     int64
	TaskID int64
	From   string
	To     string
	Source string
	Note   string
	At     int64
}

func (db *DB) QueueMessage(ctx context.Context, taskID int64, body string, waitForIdle bool) (int64, error) {
	result, err := db.ExecContext(ctx, `INSERT INTO messages(task_id, body, created_at, status, wait_for_idle)
SELECT ?, ?, ?, 'queued', ?
WHERE EXISTS (
    SELECT 1 FROM tasks WHERE id=? AND (
        state IN ('spawning', 'working', 'needs-decision', 'blocked', 'stalled')
        OR type='change' AND state IN ('done', 'merging')
    )
)`, taskID, body, time.Now().UnixMilli(), waitForIdle, taskID)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if count != 1 {
		return 0, ErrStateRace
	}
	return result.LastInsertId()
}

// QueueMessageOnceAfter queues a message once for the current completion. A
// later done Signal permits a retry, while repeated watcher ticks do not spam
// the Worker with the same outstanding freshness instruction.
func (db *DB) QueueMessageOnceAfter(ctx context.Context, taskID int64, body string, waitForIdle bool, after int64) (int64, bool, error) {
	result, err := db.ExecContext(ctx, `INSERT INTO messages(task_id, body, created_at, status, wait_for_idle)
SELECT ?, ?, ?, 'queued', ?
WHERE EXISTS (
    SELECT 1 FROM tasks WHERE id=? AND (
        state IN ('spawning', 'working', 'needs-decision', 'blocked', 'stalled')
        OR type='change' AND state IN ('done', 'merging')
    )
) AND NOT EXISTS (
    SELECT 1 FROM messages WHERE task_id=? AND body=? AND created_at>? AND status IN ('queued', 'claimed', 'interrupting', 'submitting', 'delivered')
)`, taskID, body, time.Now().UnixMilli(), waitForIdle, taskID, taskID, body, after)
	if err != nil {
		return 0, false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if count == 1 {
		id, err := result.LastInsertId()
		return id, true, err
	}
	var eligible, exists bool
	if err := db.QueryRowContext(ctx, `SELECT
EXISTS(SELECT 1 FROM tasks WHERE id=? AND (
    state IN ('spawning', 'working', 'needs-decision', 'blocked', 'stalled')
    OR type='change' AND state IN ('done', 'merging')
)),
EXISTS(SELECT 1 FROM messages WHERE task_id=? AND body=? AND created_at>? AND status IN ('queued', 'claimed', 'interrupting', 'submitting', 'delivered'))`, taskID, taskID, body, after).Scan(&eligible, &exists); err != nil {
		return 0, false, err
	}
	if !eligible && !exists {
		return 0, false, ErrStateRace
	}
	return 0, false, nil
}

type Message struct {
	ID          int64
	TaskID      int64
	Body        string
	CreatedAt   int64
	DeliveredAt int64
	Status      string
	WaitForIdle bool
}

func (db *DB) HasQueuedMessages(ctx context.Context, projectID int64) (bool, error) {
	var pending bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages JOIN tasks ON tasks.id=messages.task_id WHERE tasks.project_id=? AND messages.status='queued')`, projectID).Scan(&pending)
	return pending, err
}

func (db *DB) OldestQueuedMessage(ctx context.Context, taskID int64) (Message, error) {
	return db.queuedMessage(ctx, taskID, "")
}

func (db *DB) OldestSteerableQueuedMessage(ctx context.Context, taskID int64) (Message, error) {
	return db.queuedMessage(ctx, taskID, " AND wait_for_idle=0")
}

// QueueInterruptMessage claims the Task's queued messages and the new redirect
// in one transaction, preventing background delivery from racing the abort.
func (db *DB) QueueInterruptMessage(ctx context.Context, taskID int64, body, token string, at, activeBefore int64) (int64, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var state, taskType string
	if err := tx.QueryRowContext(ctx, `SELECT state,type FROM tasks WHERE id=?`, taskID).Scan(&state, &taskType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	if !taskAcceptsInstruction(State(state), taskType) {
		return 0, ErrStateRace
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE task_id=? AND status IN ('claimed','interrupting','submitting') AND (claimed_at=0 OR claimed_at>?)`, taskID, activeBefore).Scan(&active); err != nil {
		return 0, err
	}
	if active > 0 {
		return 0, ErrMessageDeliveryInProgress
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET status='claimed',claim_token=?,claimed_at=? WHERE task_id=? AND status='queued'`, token, at, taskID); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO messages(task_id,body,created_at,status,wait_for_idle,claim_token,claimed_at) VALUES(?,?,?,'interrupting',0,?,?)`, taskID, body, at, token, at)
	if err != nil {
		return 0, err
	}
	messageID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return messageID, nil
}

func (db *DB) WithdrawQueuedMessage(ctx context.Context, messageID int64) (Message, bool, error) {
	result, err := db.ExecContext(ctx, `UPDATE messages SET status='withdrawn' WHERE id=? AND status='queued'`, messageID)
	if err != nil {
		return Message{}, false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Message{}, false, err
	}
	message, err := db.MessageByID(ctx, messageID)
	return message, count == 1, err
}

func (db *DB) WithdrawInterruptMessage(ctx context.Context, messageID int64, token string) error {
	_, err := db.ExecContext(ctx, `UPDATE messages SET status='withdrawn',claim_token='',claimed_at=0 WHERE id=? AND status='interrupting' AND claim_token=?`, messageID, token)
	return err
}

func (db *DB) RollbackInterruptMessage(ctx context.Context, messageID int64, token string) error {
	_, err := db.ExecContext(ctx, `UPDATE messages
SET status='queued', created_at=COALESCE((
	SELECT MIN(created_at)-1 FROM messages older
	WHERE older.task_id=messages.task_id AND older.id<>messages.id AND older.status IN ('queued','claimed')
), created_at), claim_token='', claimed_at=0
WHERE id=? AND status IN ('interrupting','submitting') AND claim_token=?`, messageID, token)
	return err
}

// HasDeliveryInProgress expires only the mutual-exclusion lease. Old submitting
// rows remain ambiguous evidence and are never reset for automatic replay.
func (db *DB) HasDeliveryInProgress(ctx context.Context, taskID, activeBefore int64) (bool, error) {
	var active bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE task_id=? AND status IN ('claimed','interrupting','submitting') AND (claimed_at=0 OR claimed_at>?))`, taskID, activeBefore).Scan(&active)
	return active, err
}

// RefreshMessageDeliveryClaims extends the lease while the command owns its
// submission. Interrupt redirects refresh their whole reserved queue; ordinary
// sends refresh only their own claim. Uncertain rows remain held after exit.
func (db *DB) RefreshMessageDeliveryClaims(ctx context.Context, taskID, messageID int64, token string, at int64) error {
	_, err := db.ExecContext(ctx, `UPDATE messages SET claimed_at=?
WHERE task_id=? AND claim_token=? AND status IN ('claimed','interrupting','submitting')
  AND EXISTS (SELECT 1 FROM messages owner WHERE owner.id=? AND owner.task_id=? AND owner.claim_token=? AND owner.status IN ('claimed','interrupting','submitting'))`, at, taskID, token, messageID, taskID, token)
	return err
}

func (db *DB) ReleaseMessageClaims(ctx context.Context, taskID int64, token string, exceptID int64) error {
	_, err := db.ExecContext(ctx, `UPDATE messages SET status='queued',claim_token='',claimed_at=0 WHERE task_id=? AND status='claimed' AND claim_token=? AND (?=0 OR id<>?)`, taskID, token, exceptID, exceptID)
	return err
}

func (db *DB) RollbackMessageSubmission(ctx context.Context, messageID int64, token string) error {
	_, err := db.ExecContext(ctx, `UPDATE messages SET status='queued',claim_token='',claimed_at=0 WHERE id=? AND status='submitting' AND claim_token=?`, messageID, token)
	return err
}

func (db *DB) queuedMessage(ctx context.Context, taskID int64, predicate string) (Message, error) {
	var message Message
	query := `SELECT id, task_id, body, created_at, COALESCE(delivered_at, 0), status, wait_for_idle FROM messages WHERE task_id=? AND status='queued'` + predicate + ` ORDER BY created_at, id LIMIT 1`
	err := db.QueryRowContext(ctx, query, taskID).
		Scan(&message.ID, &message.TaskID, &message.Body, &message.CreatedAt, &message.DeliveredAt, &message.Status, &message.WaitForIdle)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	return message, err
}

func (db *DB) MessageByID(ctx context.Context, messageID int64) (Message, error) {
	var message Message
	err := db.QueryRowContext(ctx, `SELECT id, task_id, body, created_at, COALESCE(delivered_at, 0), status, wait_for_idle FROM messages WHERE id=?`, messageID).
		Scan(&message.ID, &message.TaskID, &message.Body, &message.CreatedAt, &message.DeliveredAt, &message.Status, &message.WaitForIdle)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	return message, err
}

func hasPendingTaskInstructions(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, taskID int64) (bool, error) {
	var pending bool
	err := queryer.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE task_id=? AND status IN ('queued','claimed','interrupting'))`, taskID).Scan(&pending)
	return pending, err
}

func (db *DB) HasPendingTaskInstructions(ctx context.Context, taskID int64) (bool, error) {
	return hasPendingTaskInstructions(ctx, db.DB, taskID)
}

func (db *DB) UndeliveredTaskMessages(ctx context.Context, taskID int64) ([]Message, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, task_id, body, created_at, COALESCE(delivered_at, 0), status, wait_for_idle
FROM messages WHERE task_id=? AND status IN ('queued', 'claimed', 'interrupting', 'undeliverable', 'withdrawn') ORDER BY created_at, id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := []Message{}
	for rows.Next() {
		var message Message
		if err := rows.Scan(&message.ID, &message.TaskID, &message.Body, &message.CreatedAt, &message.DeliveredAt, &message.Status, &message.WaitForIdle); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

type ExpiredMessageSubmission struct {
	Message
	TaskSeq int
}

func (db *DB) ExpiredMessageSubmissions(ctx context.Context, projectID, taskID, before int64) ([]ExpiredMessageSubmission, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.task_id, m.body, m.created_at, COALESCE(m.delivered_at, 0), m.status, m.wait_for_idle, t.seq
FROM messages m JOIN tasks t ON t.id=m.task_id
WHERE t.project_id=? AND (?=0 OR t.id=?) AND m.status='submitting' AND m.claimed_at>0 AND m.claimed_at<=?
ORDER BY m.claimed_at, m.id`, projectID, taskID, taskID, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var submissions []ExpiredMessageSubmission
	for rows.Next() {
		var submission ExpiredMessageSubmission
		if err := rows.Scan(&submission.ID, &submission.TaskID, &submission.Body, &submission.CreatedAt, &submission.DeliveredAt, &submission.Status, &submission.WaitForIdle, &submission.TaskSeq); err != nil {
			return nil, err
		}
		submissions = append(submissions, submission)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return submissions, nil
}

func (db *DB) UncertainTaskMessages(ctx context.Context, projectID, taskID, before int64) ([]Message, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.task_id, m.body, m.created_at, COALESCE(m.delivered_at, 0), m.status, m.wait_for_idle
FROM messages m JOIN tasks t ON t.id=m.task_id
WHERE t.project_id=? AND t.id=? AND m.status='submitting'
  AND ((m.claimed_at>0 AND m.claimed_at<=?) OR EXISTS (
    SELECT 1 FROM notices n WHERE n.project_id=t.project_id AND n.task_id=t.id
      AND n.kind='message_delivery_uncertain' AND json_extract(n.data_json, '$.message_id')=m.id
  ))
ORDER BY m.claimed_at, m.id`, projectID, taskID, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []Message
	for rows.Next() {
		var message Message
		if err := rows.Scan(&message.ID, &message.TaskID, &message.Body, &message.CreatedAt, &message.DeliveredAt, &message.Status, &message.WaitForIdle); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}

func (db *DB) CreateMessageDeliveryUncertainNotice(ctx context.Context, projectID, taskID, messageID int64, summary, body string) (bool, error) {
	data, err := json.Marshal(map[string]any{"message_id": messageID, "instruction": body})
	if err != nil {
		return false, err
	}
	result, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO notices(project_id, task_id, kind, summary, data_json, created_at)
SELECT t.project_id, t.id, 'message_delivery_uncertain', ?, ?, ?
FROM tasks t JOIN messages m ON m.task_id=t.id
WHERE t.project_id=? AND t.id=? AND m.id=? AND m.status='submitting'`, summary, string(data), time.Now().UnixMilli(), projectID, taskID, messageID)
	if err != nil {
		return false, err
	}
	created, err := result.RowsAffected()
	if err != nil || created != 1 {
		return created == 1, err
	}
	return true, db.PersistProject(ctx, projectID)
}

func (db *DB) MarkMessageDelivered(ctx context.Context, messageID int64, token string, at int64) error {
	return db.MarkClaimedMessageDelivered(ctx, messageID, token, at)
}

func (db *DB) ClaimMessage(ctx context.Context, messageID int64, token string, at, activeBefore int64) (bool, error) {
	result, err := db.ExecContext(ctx, `UPDATE messages SET status='claimed',claim_token=?,claimed_at=?
WHERE id=? AND status='queued' AND NOT EXISTS (
	SELECT 1 FROM messages active WHERE active.task_id=messages.task_id AND active.status IN ('claimed','interrupting','submitting') AND (active.claimed_at=0 OR active.claimed_at>?)
)`, token, at, messageID, activeBefore)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// MarkMessageSubmitting persists the external side-effect boundary. Once this
// succeeds, recovery must not submit the message again because Herdr may have
// accepted it even if Posse crashes before recording delivery.
func (db *DB) MarkMessageSubmitting(ctx context.Context, messageID int64, token string) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var taskID int64
	var state, taskType string
	if err := tx.QueryRowContext(ctx, `SELECT t.id,t.state,t.type FROM tasks t JOIN messages m ON m.task_id=t.id WHERE m.id=? AND m.status IN ('claimed','interrupting') AND m.claim_token=?`, messageID, token).Scan(&taskID, &state, &taskType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrStateRace
		}
		return err
	}
	if !taskAcceptsInstruction(State(state), taskType) {
		return ErrStateRace
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET status='submitting' WHERE id=? AND status IN ('claimed','interrupting') AND claim_token=?`, messageID, token); err != nil {
		return err
	}
	if err := setTaskTurnTokenTx(ctx, tx, taskID, fmt.Sprintf("message:%d", messageID), time.Now().UnixMilli()); err != nil {
		return err
	}
	// Commit the authorized Task transition before Herdr can expose the
	// instruction. An immediate Worker Signal must see the reopened Task.
	current := State(state)
	if current == StateNeedsDecision || taskType == "change" && (current == StateDone || current == StateMerging) {
		if err := transitionTx(ctx, db.queries.WithTx(tx.Tx), taskID, current, StateWorking, "lead", "Lead submitted an instruction"); err != nil {
			return err
		}
		if taskType == "change" && current != StateNeedsDecision {
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET gated_sha='' WHERE id=?`, taskID); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) RollbackMessageClaim(ctx context.Context, messageID int64, token string) error {
	_, err := db.ExecContext(ctx, `UPDATE messages SET status='queued',claim_token='',claimed_at=0 WHERE id=? AND status IN ('claimed','interrupting') AND claim_token=?`, messageID, token)
	return err
}

func (db *DB) MarkClaimedMessageDelivered(ctx context.Context, messageID int64, token string, at int64) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE messages SET status='delivered',delivered_at=?,claim_token='',claimed_at=0 WHERE id=? AND status='submitting' AND claim_token=?`, at, messageID, token)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrStateRace
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notices SET acked_at=COALESCE(acked_at,?) WHERE kind='message_delivery_uncertain' AND json_extract(data_json,'$.message_id')=?`, at, messageID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var projectID int64
	if err := db.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id=(SELECT task_id FROM messages WHERE id=?)`, messageID).Scan(&projectID); err != nil {
		return err
	}
	return db.PersistProject(ctx, projectID)
}

func (db *DB) Notices(ctx context.Context, projectID int64, openOnly bool) ([]Notice, error) {
	notices := make([]Notice, 0)
	if openOnly {
		rows, err := db.queries.OpenNotices(ctx, projectID)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			notices = append(notices, noticeFromDB(row.ID, row.ProjectID, row.TaskID, row.Kind, row.Summary, row.DataJson, row.CreatedAt, row.DeliveredAt, row.AckedAt))
		}
	} else {
		rows, err := db.queries.Notices(ctx, projectID)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			notices = append(notices, noticeFromDB(row.ID, row.ProjectID, row.TaskID, row.Kind, row.Summary, row.DataJson, row.CreatedAt, row.DeliveredAt, row.AckedAt))
		}
	}
	return notices, nil
}

func (db *DB) UndeliveredNotices(ctx context.Context, projectID int64) ([]Notice, error) {
	rows, err := db.queries.UndeliveredNotices(ctx, projectID)
	if err != nil {
		return nil, err
	}
	notices := make([]Notice, 0, len(rows))
	for _, row := range rows {
		notices = append(notices, noticeFromDB(row.ID, row.ProjectID, row.TaskID, row.Kind, row.Summary, row.DataJson, row.CreatedAt, row.DeliveredAt, row.AckedAt))
	}
	return notices, nil
}

func (db *DB) HasNotice(ctx context.Context, projectID, taskID int64, kind string, launch ...int) (bool, error) {
	var found int
	var err error
	if kind == "worker_idle" && len(launch) > 0 {
		err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notices WHERE project_id=? AND task_id=? AND kind=? AND json_extract(data_json,'$.launch')=?)`, projectID, taskID, kind, launch[0]).Scan(&found)
	} else {
		err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notices WHERE project_id=? AND task_id=? AND kind=?)`, projectID, taskID, kind).Scan(&found)
	}
	return found != 0, err
}

func noticeFromDB(id, projectID, taskID int64, kind, summary, dataJSON string, createdAt, deliveredAt, ackedAt int64) Notice {
	return Notice{ID: id, ProjectID: projectID, TaskID: taskID, Kind: kind, Summary: summary, DataJSON: dataJSON, CreatedAt: createdAt, DeliveredAt: deliveredAt, AckedAt: ackedAt}
}

func (db *DB) MarkNoticesDelivered(ctx context.Context, projectID int64, ids []int64, at int64) error {
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		if err := db.queries.MarkNoticeDelivered(ctx, dbgen.MarkNoticeDeliveredParams{DeliveredAt: sql.NullInt64{Int64: at, Valid: true}, ID: id, ProjectID: projectID}); err != nil {
			return err
		}
	}
	return db.PersistProject(ctx, projectID)
}

func (db *DB) ClaimNoticeBatch(ctx context.Context, projectID int64, ids []int64, token string, at int64) (bool, error) {
	if len(ids) == 0 {
		return false, nil
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	query := `UPDATE notices SET claim_token=?,claimed_at=? WHERE project_id=? AND delivered_at IS NULL AND acked_at IS NULL AND claim_token='' AND id IN (` + sqlPlaceholders(len(ids)) + `)`
	args := []any{token, at, projectID}
	for _, id := range ids {
		args = append(args, id)
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if count != int64(len(ids)) {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (db *DB) RollbackNoticeClaim(ctx context.Context, projectID int64, ids []int64, token string) error {
	if len(ids) == 0 {
		return nil
	}
	query := `UPDATE notices SET claim_token='',claimed_at=0 WHERE project_id=? AND claim_token=? AND id IN (` + sqlPlaceholders(len(ids)) + `)`
	args := []any{projectID, token}
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := db.ExecContext(ctx, query, args...)
	return err
}

func (db *DB) MarkClaimedNoticesDelivered(ctx context.Context, projectID int64, ids []int64, token string, at int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := `UPDATE notices SET delivered_at=?,claim_token='',claimed_at=0 WHERE project_id=? AND claim_token=? AND delivered_at IS NULL AND acked_at IS NULL AND id IN (` + sqlPlaceholders(len(ids)) + `)`
	args := []any{at, projectID, token}
	for _, id := range ids {
		args = append(args, id)
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != int64(len(ids)) {
		return ErrStateRace
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistProject(ctx, projectID)
}

func (db *DB) ReleaseExpiredDeliveryClaims(ctx context.Context, before int64) error {
	// Pre-submission delivery claims are safe to retry. An interrupted redirect
	// that never reached submission expires as withdrawn, not as a normal queue item.
	if _, err := db.ExecContext(ctx, `UPDATE messages SET status='queued',claim_token='',claimed_at=0 WHERE status='claimed' AND claimed_at>0 AND claimed_at<=?`, before); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE messages SET status='withdrawn',claim_token='',claimed_at=0 WHERE status='interrupting' AND claimed_at>0 AND claimed_at<=?`, before); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `UPDATE notices SET claim_token='',claimed_at=0 WHERE claim_token<>'' AND claim_token NOT LIKE 'uncertain:%' AND delivered_at IS NULL AND acked_at IS NULL AND claimed_at>0 AND claimed_at<=?
		AND NOT EXISTS (SELECT 1 FROM notice_delivery_receipts r WHERE r.project_id=notices.project_id AND r.owner_token=notices.claim_token AND r.state IN ('printed','uncertain'))`, before)
	return err
}

func sqlPlaceholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func (db *DB) LastNoticeDelivery(ctx context.Context, projectID int64) (int64, error) {
	return db.queries.LastNoticeDelivery(ctx, projectID)
}

func (db *DB) LastNoticeNotification(ctx context.Context, projectID int64) (int64, error) {
	return db.queries.LastNoticeNotification(ctx, projectID)
}

func (db *DB) RecordNoticeNotification(ctx context.Context, projectID, at int64) error {
	return db.queries.RecordNoticeNotification(ctx, dbgen.RecordNoticeNotificationParams{ProjectID: projectID, NotifiedAt: at})
}

// AckUndeliveredPROpened quietly handles only PR-opened Notices which need
// no Lead decision or follow-up. A claimed/delivered Notice belongs to its
// existing delivery path and must not be silently acknowledged here.
func (db *DB) AckUndeliveredPROpened(ctx context.Context, projectID, at int64) (int64, error) {
	result, err := db.ExecContext(ctx, `UPDATE notices SET delivered_at=?, acked_at=?
		WHERE project_id=? AND kind='pr_opened' AND delivered_at IS NULL AND acked_at IS NULL AND claim_token=''`, at, at, projectID)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return count, err
	}
	return count, db.PersistProject(ctx, projectID)
}

// RequeueNotices retries a Pi message that was rejected before it entered the
// conversation. Already acknowledged Notices and live claims are never reset.
func (db *DB) RequeueNotices(ctx context.Context, projectID int64, ids []int64) error {
	for _, id := range ids {
		if _, err := db.ExecContext(ctx, `UPDATE notices SET delivered_at=NULL
			WHERE id=? AND project_id=? AND delivered_at IS NOT NULL AND acked_at IS NULL AND claim_token=''`, id, projectID); err != nil {
			return err
		}
	}
	return db.PersistProject(ctx, projectID)
}

func (db *DB) AckNotices(ctx context.Context, projectID int64, identifiers []string) (int, error) {
	at := time.Now().UnixMilli()
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	queries := db.queries.WithTx(tx.Tx)
	count := 0
	if len(identifiers) == 1 && identifiers[0] == "all" {
		result, err := queries.AckAllNotices(ctx, dbgen.AckAllNoticesParams{AckedAt: sql.NullInt64{Int64: at, Valid: true}, ProjectID: projectID})
		if err != nil {
			return 0, err
		}
		changed, _ := result.RowsAffected()
		count = int(changed)
	} else {
		for _, value := range identifiers {
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil || id < 1 {
				return count, fmt.Errorf("invalid Notice id %q", value)
			}
			result, err := queries.AckNotice(ctx, dbgen.AckNoticeParams{AckedAt: sql.NullInt64{Int64: at, Valid: true}, ID: id, ProjectID: projectID})
			if err != nil {
				return count, err
			}
			changed, _ := result.RowsAffected()
			count += int(changed)
		}
	}
	if err := acceptAcknowledgedNoticeDeliveriesTx(ctx, tx, projectID, at); err != nil {
		return count, err
	}
	if err := tx.Commit(); err != nil {
		return count, err
	}
	if err := db.PersistNoticeDeliverySnapshotIfPresent(ctx, projectID); err != nil {
		return count, err
	}
	if err := db.PersistProject(ctx, projectID); err != nil {
		return count, err
	}
	return count, nil
}

func (db *DB) ProjectByRoot(ctx context.Context, root string) (Project, error) {
	row, err := db.queries.ProjectByRoot(ctx, root)
	return projectFromDB(row), err
}

func (db *DB) ProjectByName(ctx context.Context, name string) (Project, error) {
	row, err := db.queries.ProjectByName(ctx, name)
	return projectFromDB(row), err
}

func (db *DB) ProjectByID(ctx context.Context, id int64) (Project, error) {
	row, err := db.queries.ProjectByID(ctx, id)
	return projectFromDB(row), err
}

func (db *DB) ProjectByLeadPane(ctx context.Context, paneID string) (Project, error) {
	row, err := db.queries.ProjectByLeadPane(ctx, paneID)
	return projectFromDB(row), err
}

func (db *DB) Projects(ctx context.Context) ([]Project, error) {
	rows, err := db.queries.Projects(ctx)
	if err != nil {
		return nil, err
	}
	projects := make([]Project, 0, len(rows))
	for _, row := range rows {
		projects = append(projects, projectFromDB(row))
	}
	return projects, nil
}

func projectFromDB(row dbgen.Project) Project {
	return Project{ID: row.ID, UUID: row.ProjectUuid, Name: row.Name, Root: row.Root, DefaultBranch: row.DefaultBranch, Kind: row.Kind, HerdrWorkspaceID: row.HerdrWorkspaceID, LeadPaneID: row.LeadPaneID, LeadLabel: row.LeadLabel, LeadLaunches: int(row.LeadLaunches), LeadAbsentSince: row.LeadAbsentSince, Status: row.Status, DownAt: row.DownAt, CreatedAt: row.CreatedAt, LastActivityAt: row.LastActivityAt}
}

type AmbiguousTaskName struct {
	Name string
	IDs  []string
}

func (e *AmbiguousTaskName) Error() string { return fmt.Sprintf("Task name %q is ambiguous", e.Name) }

func (db *DB) Task(ctx context.Context, projectID int64, identifier string) (Task, error) {
	row, err := db.queries.Task(ctx, dbgen.TaskParams{ProjectID: projectID, Identifier: identifier})
	if errors.Is(err, sql.ErrNoRows) {
		rows, lookupErr := db.QueryContext(ctx, `SELECT seq FROM tasks WHERE project_id=? AND (short_name=? OR (short_name='' AND title=?)) ORDER BY seq`, projectID, identifier, identifier)
		if lookupErr != nil {
			return Task{}, lookupErr
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var seq int
			if err := rows.Scan(&seq); err != nil {
				return Task{}, err
			}
			ids = append(ids, fmt.Sprintf("t%d", seq))
		}
		if err := rows.Err(); err != nil {
			return Task{}, err
		}
		if len(ids) > 1 {
			return Task{}, &AmbiguousTaskName{Name: identifier, IDs: ids}
		}
		if len(ids) == 1 {
			return db.Task(ctx, projectID, ids[0])
		}
	}
	return taskFromDB(row), err
}

func (db *DB) TaskByID(ctx context.Context, projectID, id int64) (Task, error) {
	row, err := db.queries.TaskByID(ctx, dbgen.TaskByIDParams{ProjectID: projectID, ID: id})
	return taskFromDB(row), err
}

func (db *DB) TaskByPane(ctx context.Context, paneID string) (Task, error) {
	row, err := db.queries.TaskByPane(ctx, paneID)
	return taskFromDB(row), err
}

func (db *DB) TaskByWorktree(ctx context.Context, path string) (Task, error) {
	row, err := db.queries.TaskByWorktree(ctx, path)
	return taskFromDB(row), err
}

func (db *DB) Tasks(ctx context.Context, projectID int64, includeFinished bool) ([]Task, error) {
	var rows []dbgen.Task
	var err error
	if includeFinished {
		rows, err = db.queries.AllTasks(ctx, projectID)
	} else {
		rows, err = db.queries.Tasks(ctx, projectID)
	}
	if err != nil {
		return nil, err
	}
	tasks := make([]Task, 0, len(rows))
	for _, row := range rows {
		tasks = append(tasks, taskFromDB(row))
	}
	return tasks, nil
}

func (db *DB) LiveTasks(ctx context.Context, projectID int64) ([]Task, error) {
	rows, err := db.queries.LiveTasks(ctx, projectID)
	if err != nil {
		return nil, err
	}
	tasks := make([]Task, 0, len(rows))
	for _, row := range rows {
		tasks = append(tasks, taskFromDB(row))
	}
	return tasks, nil
}

func (db *DB) StallTasks(ctx context.Context, projectID int64) ([]Task, error) {
	rows, err := db.queries.StallTasks(ctx, projectID)
	if err != nil {
		return nil, err
	}
	tasks := make([]Task, 0, len(rows))
	for _, row := range rows {
		tasks = append(tasks, taskFromDB(row))
	}
	return tasks, nil
}

func taskFromDB(row dbgen.Task) Task {
	reviewsTaskID := int64(0)
	if row.ReviewsTaskID.Valid {
		reviewsTaskID = row.ReviewsTaskID.Int64
	}
	sandboxID := int64(0)
	if row.SandboxID.Valid {
		sandboxID = row.SandboxID.Int64
	}
	return Task{ID: row.ID, ProjectID: row.ProjectID, Seq: int(row.Seq), Type: row.Type, ReviewsTaskID: reviewsTaskID,
		Title: row.Title, PRTitle: row.PrTitle, ShortName: row.ShortName, State: State(row.State), Profile: row.Profile, DispatchRule: row.DispatchRule, PlanJSON: row.PlanJson,
		MergeMode: row.MergeMode, AutonomyReview: row.AutonomyReview, AutonomyMerge: row.AutonomyMerge,
		Branch: row.Branch, BaseRef: row.BaseRef, TargetBranch: row.TargetBranch, WorktreePath: row.WorktreePath, SandboxID: sandboxID, Launches: int(row.Launches), GatedSHA: row.GatedSha,
		HerdrWorkspaceID: row.HerdrWorkspaceID, PaneID: row.PaneID, PaneLabel: row.PaneLabel,
		AgentName: row.AgentName, AgentSession: row.AgentSession, AgentSessionKind: row.AgentSessionKind, AgentAbsentSince: row.AgentAbsentSince, IdleSince: row.IdleSince, AgentServerStartedAt: row.AgentServerStartedAt,
		PRURL: row.PrUrl, MergedRef: row.MergedRef, LastOutputHash: row.LastOutputHash,
		LastWorktreeHash: row.LastWorktreeHash, LastProgressAt: row.LastProgressAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		ReportKind: row.ReportKind, ReportDigest: row.ReportDigest, ReportManifestJSON: row.ReportManifestJson,
		ContainmentMode: row.ContainmentMode, ContainmentPath: row.ContainmentPath, ContainmentMarker: row.ContainmentMarker,
		PaneShellIdentity: row.PaneShellIdentity}
}

func (db *DB) UpdateTaskPaneShellIdentity(ctx context.Context, taskID int64, identity string) error {
	if identity == "" {
		return fmt.Errorf("Task pane shell identity cannot be empty")
	}
	if err := db.queries.SetTaskPaneShellIdentity(ctx, dbgen.SetTaskPaneShellIdentityParams{PaneShellIdentity: identity, UpdatedAt: time.Now().UnixMilli(), ID: taskID}); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) UpdateTaskContainment(ctx context.Context, taskID int64, mode, path, marker string) error {
	if mode != "" && mode != "cgroup" && mode != "marker" {
		return fmt.Errorf("invalid Task containment mode %q", mode)
	}
	if marker == "" {
		return fmt.Errorf("Task containment requires a process marker")
	}
	if err := db.queries.SetTaskContainment(ctx, dbgen.SetTaskContainmentParams{ContainmentMode: mode, ContainmentPath: path, ContainmentMarker: marker, UpdatedAt: time.Now().UnixMilli(), ID: taskID}); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) UpdateTaskObservation(ctx context.Context, taskID int64, paneID, workspaceID, agentSession, agentSessionKind string, agentAbsentSince, idleSince int64, agentServerStartedAt string) error {
	ctx, cancel := context.WithTimeout(ctx, ObservationWriteBudget)
	defer cancel()
	unchanged, err := db.taskObservationMatches(ctx, taskID, paneID, workspaceID, agentSession, agentSessionKind, agentAbsentSince, idleSince, agentServerStartedAt)
	if err != nil {
		return fmt.Errorf("check Task observation: %w", err)
	}
	if unchanged {
		// A prior database update may have committed before its snapshot write failed.
		return db.PersistTask(ctx, taskID)
	}
	if err := db.writeTaskObservation(ctx, taskID, paneID, workspaceID, agentSession, agentSessionKind, agentAbsentSince, idleSince, agentServerStartedAt, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("write Task observation: %w", err)
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) writeTaskObservation(ctx context.Context, taskID int64, paneID, workspaceID, agentSession, agentSessionKind string, agentAbsentSince, idleSince int64, agentServerStartedAt string, updatedAt int64) error {
	params := dbgen.UpdateTaskObservationParams{
		PaneID: paneID, HerdrWorkspaceID: workspaceID, AgentSession: agentSession, AgentSessionKind: agentSessionKind,
		AgentAbsentSince: agentAbsentSince, AgentServerStartedAt: agentServerStartedAt, IdleSince: idleSince,
		UpdatedAt: updatedAt, ID: taskID,
		PaneID_2: paneID, HerdrWorkspaceID_2: workspaceID, AgentSession_2: agentSession, AgentSessionKind_2: agentSessionKind,
		AgentAbsentSince_2: agentAbsentSince, AgentServerStartedAt_2: agentServerStartedAt, IdleSince_2: idleSince,
	}
	return db.withBusyTimeout(ctx, reconcileWriteBusyTimeoutMillis, func(conn *sql.Conn) error {
		return dbgen.New(conn).UpdateTaskObservation(ctx, params)
	})
}

func (db *DB) taskObservationMatches(ctx context.Context, taskID int64, paneID, workspaceID, agentSession, agentSessionKind string, agentAbsentSince, idleSince int64, agentServerStartedAt string) (bool, error) {
	var currentPaneID, currentWorkspaceID, currentAgentSession, currentAgentSessionKind, currentAgentServerStartedAt string
	var currentAgentAbsentSince, currentIdleSince int64
	err := db.QueryRowContext(ctx, `SELECT pane_id, herdr_workspace_id, agent_session, agent_session_kind, agent_absent_since, idle_since, agent_server_started_at FROM tasks WHERE id=?`, taskID).Scan(
		&currentPaneID, &currentWorkspaceID, &currentAgentSession, &currentAgentSessionKind, &currentAgentAbsentSince, &currentIdleSince, &currentAgentServerStartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, retryableContention(err)
	}
	return currentPaneID == paneID && currentWorkspaceID == workspaceID && currentAgentSession == agentSession && currentAgentSessionKind == agentSessionKind && currentAgentAbsentSince == agentAbsentSince && currentIdleSince == idleSince && currentAgentServerStartedAt == agentServerStartedAt, nil
}

func (db *DB) UpdateProjectObservation(ctx context.Context, project Project, paneID, workspaceID string, absentSince int64) error {
	ctx, cancel := context.WithTimeout(ctx, ObservationWriteBudget)
	defer cancel()
	// Concurrent CLI commands often observe the same Lead pane. Skip the write
	// lock when another reconciliation already recorded this state.
	unchanged, err := db.projectObservationMatches(ctx, project.ID, paneID, workspaceID, absentSince)
	if err != nil || unchanged {
		return err
	}
	if err := db.writeProjectObservation(ctx, project, paneID, workspaceID, absentSince); err != nil {
		return err
	}
	return db.PersistProject(ctx, project.ID)
}

func (db *DB) writeProjectObservation(ctx context.Context, project Project, paneID, workspaceID string, absentSince int64) error {
	params := dbgen.UpdateProjectObservationParams{
		PaneID: paneID, WorkspaceID: workspaceID, LeadAbsentSince: absentSince, ProjectID: project.ID,
		ExpectedLeadPaneID: project.LeadPaneID, ExpectedWorkspaceID: project.HerdrWorkspaceID, ExpectedLeadLabel: project.LeadLabel,
	}
	return db.withBusyTimeout(ctx, reconcileWriteBusyTimeoutMillis, func(conn *sql.Conn) error {
		return dbgen.New(conn).UpdateProjectObservation(ctx, params)
	})
}

func (db *DB) projectObservationMatches(ctx context.Context, projectID int64, paneID, workspaceID string, absentSince int64) (bool, error) {
	var currentPaneID, currentWorkspaceID string
	var currentAbsentSince int64
	err := db.QueryRowContext(ctx, `SELECT lead_pane_id, herdr_workspace_id, lead_absent_since FROM projects WHERE id=?`, projectID).Scan(&currentPaneID, &currentWorkspaceID, &currentAbsentSince)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, retryableContention(err)
	}
	return currentPaneID == paneID && currentWorkspaceID == workspaceID && currentAbsentSince == absentSince, nil
}

func (db *DB) UpdateProjectStatus(ctx context.Context, projectID int64, status string) error {
	if status != "active" && status != "missing" {
		return fmt.Errorf("invalid Project status %q", status)
	}
	if _, err := db.ExecContext(ctx, `UPDATE projects SET status=? WHERE id=?`, status, projectID); err != nil {
		return err
	}
	return db.PersistProject(ctx, projectID)
}

func (db *DB) UpdateProgress(ctx context.Context, taskID int64, outputHash, worktreeHash string, at int64) error {
	if err := db.queries.UpdateProgress(ctx, dbgen.UpdateProgressParams{LastOutputHash: outputHash, LastWorktreeHash: worktreeHash, LastProgressAt: at, UpdatedAt: at, ID: taskID}); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) ResetProgress(ctx context.Context, taskID int64) error {
	if err := db.queries.ResetProgress(ctx, dbgen.ResetProgressParams{UpdatedAt: time.Now().UnixMilli(), ID: taskID}); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) ResetTaskProgress(ctx context.Context, taskID int64) error {
	if err := db.queries.ResetTaskProgress(ctx, dbgen.ResetTaskProgressParams{UpdatedAt: time.Now().UnixMilli(), ID: taskID}); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

type Notice struct {
	ID          int64
	ProjectID   int64
	TaskID      int64
	Kind        string
	Summary     string
	DataJSON    string
	CreatedAt   int64
	DeliveredAt int64
	AckedAt     int64
}

func (db *DB) CreateNotice(ctx context.Context, notice Notice) (int64, error) {
	if notice.DataJSON == "" {
		notice.DataJSON = "{}"
	}
	createdAt := notice.CreatedAt
	if createdAt == 0 {
		createdAt = time.Now().UnixMilli()
	}
	var nullableTaskID sql.NullInt64
	if notice.TaskID != 0 {
		nullableTaskID = sql.NullInt64{Int64: notice.TaskID, Valid: true}
	}
	result, err := db.queries.InsertNotice(ctx, dbgen.InsertNoticeParams{ProjectID: notice.ProjectID, TaskID: nullableTaskID, Kind: notice.Kind, Summary: notice.Summary, DataJson: notice.DataJSON, CreatedAt: createdAt})
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := db.PersistProject(ctx, notice.ProjectID); err != nil {
		return 0, err
	}
	return id, nil
}

func (db *DB) CreateWorkerExitedNotice(ctx context.Context, task Task, now time.Time) error {
	dataJSON, err := json.Marshal(struct {
		TaskLaunches int `json:"task_launches"`
	}{TaskLaunches: task.Launches})
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO notices(project_id, task_id, kind, summary, data_json, created_at) VALUES (?, ?, 'worker_exited', ?, ?, ?)`, task.ProjectID, task.ID, task.Title+" Worker exited after signaling done", string(dataJSON), now.UnixMilli()); err != nil {
		return err
	}
	return db.PersistProject(ctx, task.ProjectID)
}

func (db *DB) AddEvent(ctx context.Context, kind, paneID, dataJSON string, at int64) error {
	if at == 0 {
		at = time.Now().UnixMilli()
	}
	if dataJSON == "" {
		dataJSON = "{}"
	}
	if err := db.queries.InsertEvent(ctx, dbgen.InsertEventParams{ReceivedAt: at, Kind: kind, PaneID: paneID, DataJson: dataJSON}); err != nil {
		return err
	}
	return db.queries.PruneEvents(ctx, time.Now().Add(-7*24*time.Hour).UnixMilli())
}

func IsNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// ErrProjectNotEmpty reports a Project that still has Tasks or Sandboxes.
var ErrProjectNotEmpty = errors.New("project has Tasks or Sandboxes")

// ProjectContents counts a Project's Tasks (of any state) and Sandboxes.
func (db *DB) ProjectContents(ctx context.Context, projectID int64) (tasks, sandboxes int, err error) {
	err = db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM tasks WHERE project_id=?), (SELECT COUNT(*) FROM mounts WHERE project_id=?)`, projectID, projectID).Scan(&tasks, &sandboxes)
	return tasks, sandboxes, err
}

// DeleteEmptyProject removes a Project that never had a Task or a Sandbox, with
// its Project-level rows, in one transaction. A Project with either is refused
// with ErrProjectNotEmpty, so no Task history or worktree is ever dropped.
func (db *DB) DeleteEmptyProject(ctx context.Context, projectID int64) error {
	var projectName string
	if err := db.QueryRowContext(ctx, `SELECT name FROM projects WHERE id=?`, projectID).Scan(&projectName); err != nil {
		return err
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var tasks, sandboxes int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM tasks WHERE project_id=?), (SELECT COUNT(*) FROM mounts WHERE project_id=?)`, projectID, projectID).Scan(&tasks, &sandboxes); err != nil {
		return err
	}
	if tasks != 0 || sandboxes != 0 {
		return ErrProjectNotEmpty
	}
	if err := deleteProjectReferences(ctx, tx, projectID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, projectID)
	if err != nil {
		return projectReferenceConstraint(err)
	}
	if deleted, err := result.RowsAffected(); err != nil {
		return err
	} else if deleted == 0 {
		return sql.ErrNoRows
	}
	if err := projectReferenceConstraint(tx.Commit()); err != nil {
		return err
	}
	if err := os.Remove(db.ProjectSnapshotPath(projectName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
