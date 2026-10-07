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
var ErrBusy = errors.New("database contention; retry the operation")

const sqliteBusyTimeoutMillis = 5000

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
	return openAt(path, 5*time.Second)
}

func openAt(path string, timeout time.Duration) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	fileURL := (&url.URL{Scheme: "file", Path: path}).String()
	dsn := fileURL + "?_pragma=busy_timeout(" + strconv.Itoa(sqliteBusyTimeoutMillis) + ")&_pragma=foreign_keys(1)&_txlock=immediate"
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
	return result, nil
}

func (db *DB) MigrateIfNeeded(ctx context.Context) (migrationErr error) {
	defer func() { migrationErr = retryableContention(migrationErr) }()
	lock, err := acquireMigrationLock(ctx, db.Path)
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
	sources, err := embeddedMigrations()
	if err != nil {
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
	StateLanding       State = "landing"
	StateLanded        State = "landed"
	StateReported      State = "reported"
	StateFailed        State = "failed"
	StateLost          State = "lost"
	StateTornDown      State = "torn-down"
)

func taskAcceptsInstruction(state State, taskType string) bool {
	if oneOfState(state, StateSpawning, StateWorking, StateNeedsDecision, StateBlocked, StateStalled) {
		return true
	}
	return taskType == "ship" && oneOfState(state, StateDone, StateLanding)
}

var transitions = map[State]map[State]bool{
	StateSpawning:      {StateWorking: true, StateFailed: true, StateLost: true, StateLanded: true},
	StateWorking:       {StateNeedsDecision: true, StateFailed: true, StateBlocked: true, StateStalled: true, StateDone: true, StateLost: true, StateLanded: true},
	StateNeedsDecision: {StateWorking: true, StateFailed: true, StateLost: true, StateLanded: true},
	StateBlocked:       {StateWorking: true, StateFailed: true, StateLost: true, StateLanded: true},
	StateStalled:       {StateWorking: true, StateTornDown: true, StateLost: true, StateLanded: true},
	StateDone:          {StateLanding: true, StateLanded: true, StateReported: true, StateWorking: true},
	StateLanding:       {StateLanded: true, StateDone: true, StateWorking: true},
	StateLanded:        {StateTornDown: true},
	StateReported:      {StateTornDown: true},
	StateFailed:        {StateWorking: true, StateTornDown: true, StateLanded: true},
	StateLost:          {StateWorking: true, StateTornDown: true, StateLanded: true},
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
	if next == StateTornDown && (expected == StateFailed || expected == StateLost || expected == StateStalled) {
		return fmt.Errorf("discarding unlanded work requires a recorded User approval")
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

func (db *DB) TransitionWithApproval(ctx context.Context, taskID int64, expected, next State, source, note, action, quote string) error {
	if next != StateTornDown || !oneOfState(expected, StateFailed, StateLost, StateStalled) || source != "user" {
		return fmt.Errorf("approval-backed transition is only valid for User-approved discard")
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
	if next != StateTornDown || !oneOfState(expected, StateFailed, StateLost, StateStalled) || source != "user" {
		return fmt.Errorf("approval-backed transition is only valid for User-approved discard")
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
	if expected == StateLanding && next == StateDone {
		if err := queries.ClearGatedSHALandingToDone(ctx, taskID); err != nil {
			return err
		}
	}
	return queries.TouchProjectForTask(ctx, dbgen.TouchProjectForTaskParams{LastActivityAt: now, ID: taskID})
}

func validateTransitionSource(from, to State, source string) error {
	expected := "cli"
	switch {
	case (to == StateDone && from != StateLanding) || (from == StateWorking && (to == StateNeedsDecision || to == StateFailed)):
		expected = "worker"
	case from == StateWorking && to == StateBlocked || from == StateBlocked && to == StateWorking:
		expected = "herdr"
	case from == StateNeedsDecision && to == StateWorking:
		expected = "lead"
	case from == StateDone && to == StateWorking:
		expected = "lead"
	case from == StateLanding && to == StateWorking:
		expected = "lead"
	case from == StateStalled && to == StateWorking:
		if source != "cli" {
			expected = "worker"
		}
	case (from == StateFailed || from == StateLost) && to == StateWorking:
		expected = "cli"
	case (from == StateFailed || from == StateLost || from == StateStalled) && to == StateTornDown:
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
		Title: task.Title, ShortName: task.ShortName, State: string(task.State), Profile: task.Profile, DispatchRule: task.DispatchRule,
		LandingMode: task.LandingMode, AutonomyReview: task.AutonomyReview, AutonomyLand: task.AutonomyLand,
		Branch: task.Branch, BaseRef: task.BaseRef, WorktreePath: task.WorktreePath,
		HerdrWorkspaceID: task.HerdrWorkspaceID, PaneID: task.PaneID, PaneLabel: task.PaneLabel,
		AgentName: task.AgentName, AgentSession: task.AgentSession, PrUrl: task.PRURL, LandedRef: task.LandedRef,
		LastOutputHash: task.LastOutputHash, LastWorktreeHash: task.LastWorktreeHash,
		LastProgressAt: task.LastProgressAt, AgentAbsentSince: task.AgentAbsentSince, IdleSince: task.IdleSince, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
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
	Profile         string `toml:"profile" json:"profile"`
	ConfiguredModel string `toml:"configured_model" json:"configured_model"`
	ModelKnown      bool   `toml:"model_known" json:"model_known"`
}

type Task struct {
	ID                   int64  `toml:"id"`
	ProjectID            int64  `toml:"project_id"`
	Seq                  int    `toml:"seq"`
	Type                 string `toml:"type"`
	ReviewsTaskID        int64  `toml:"reviews_task_id"`
	Title                string `toml:"title"`
	ShortName            string `toml:"short_name"`
	State                State  `toml:"state"`
	Profile              string `toml:"profile"`
	DispatchRule         string `toml:"dispatch_rule"`
	LandingMode          string `toml:"landing_mode"`
	AutonomyReview       string `toml:"autonomy_review"`
	AutonomyLand         string `toml:"autonomy_land"`
	Branch               string `toml:"branch"`
	BaseRef              string `toml:"base_ref"`
	WorktreePath         string `toml:"worktree_path"`
	MountID              int64  `toml:"mount_id"`
	Launches             int    `toml:"launches"`
	GatedSHA             string `toml:"gated_sha"`
	HerdrWorkspaceID     string `toml:"herdr_workspace_id"`
	PaneID               string `toml:"pane_id"`
	PaneLabel            string `toml:"pane_label"`
	AgentName            string `toml:"agent_name"`
	AgentSession         string `toml:"agent_session"`
	AgentAbsentSince     int64  `toml:"agent_absent_since"`
	IdleSince            int64  `toml:"idle_since"`
	AgentServerStartedAt string `toml:"agent_server_started_at"`
	PRURL                string `toml:"pr_url"`
	LandedRef            string `toml:"landed_ref"`
	LastOutputHash       string `toml:"last_output_hash"`
	LastWorktreeHash     string `toml:"last_worktree_hash"`
	LastProgressAt       int64  `toml:"last_progress_at"`
	CreatedAt            int64  `toml:"created_at"`
	UpdatedAt            int64  `toml:"updated_at"`
}

type Mount struct {
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
	return db.createTaskWithSequence(ctx, projectID, projectName, task, "", 0, occupied)
}

func (db *DB) CreateTaskWithSequenceAndIntent(ctx context.Context, projectID int64, projectName string, task Task, command string, processID int, occupied func(int) (bool, error)) (int64, int, error) {
	if command == "" || processID < 1 {
		return 0, 0, fmt.Errorf("intent command and process id are required")
	}
	return db.createTaskWithSequence(ctx, projectID, projectName, task, command, processID, occupied)
}

var ErrTaskBranchExists = errors.New("Task branch already used")

func (db *DB) TaskBranchExists(ctx context.Context, projectID int64, branch string) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE project_id=? AND branch=?)`, projectID, branch).Scan(&exists)
	return exists, err
}

func (db *DB) createTaskWithSequence(ctx context.Context, projectID int64, projectName string, task Task, intentCommand string, processID int, occupied func(int) (bool, error)) (int64, int, error) {
	if task.ShortName == "" {
		return 0, 0, fmt.Errorf("Task short name is required")
	}
	task.Branch = "posse/" + task.ShortName
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
	var branchUsed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE project_id=? AND branch=?)`, projectID, task.Branch).Scan(&branchUsed); err != nil {
		return 0, 0, err
	}
	if branchUsed {
		return 0, 0, ErrTaskBranchExists
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
	err = tx.QueryRowContext(ctx, `INSERT INTO tasks(project_id,seq,type,reviews_task_id,title,short_name,state,profile,dispatch_rule,landing_mode,autonomy_review,autonomy_land,branch,base_ref,worktree_path,pane_label,created_at,updated_at)
VALUES (?, ?, ?, ?, ?, ?, 'spawning', ?, ?, ?, ?, ?, ?, ?, ?, 'posse:' || ? || ':t' || ?, ?, ?) RETURNING id`,
		projectID, seq, task.Type, nullableID(task.ReviewsTaskID), task.Title, task.ShortName, task.Profile, task.DispatchRule,
		task.LandingMode, task.AutonomyReview, task.AutonomyLand, task.Branch, task.BaseRef, task.WorktreePath,
		projectName, seq, now, now).Scan(&id)
	if err != nil {
		return 0, 0, err
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
	return id, seq, nil
}

func (db *DB) AcquireMount(ctx context.Context, projectID, taskID int64, basePath string) (Mount, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return Mount{}, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	var mount Mount
	err = tx.QueryRowContext(ctx, `UPDATE mounts SET state='held',task_id=?,acquired_at=?,released_at=0 WHERE id=(SELECT id FROM mounts WHERE project_id=? AND state='idle' ORDER BY n LIMIT 1) AND state='idle' RETURNING id,project_id,n,path,state,task_id,acquired_at,released_at`, taskID, now, projectID).Scan(&mount.ID, &mount.ProjectID, &mount.Number, &mount.Path, &mount.State, &mount.TaskID, &mount.AcquiredAt, &mount.ReleasedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id,acquired_at,released_at) SELECT ?,COALESCE(MAX(n),0)+1,? || '/mount-' || (COALESCE(MAX(n),0)+1),'held',?,?,0 FROM mounts WHERE project_id=? RETURNING id,project_id,n,path,state,task_id,acquired_at,released_at`, projectID, basePath, taskID, now, projectID).Scan(&mount.ID, &mount.ProjectID, &mount.Number, &mount.Path, &mount.State, &mount.TaskID, &mount.AcquiredAt, &mount.ReleasedAt)
	}
	if err != nil {
		return Mount{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET mount_id=?,worktree_path=?,updated_at=? WHERE id=?`, mount.ID, mount.Path, now, taskID); err != nil {
		return Mount{}, err
	}
	if err := tx.Commit(); err != nil {
		return Mount{}, err
	}
	if err := db.PersistTask(ctx, taskID); err != nil {
		return Mount{}, err
	}
	return mount, nil
}

// BeginMountRelease records that the checkout has been reset and no longer
// contains Task work. Its Git lock may now be removed without ever exposing
// an unlocked checkout that the database still describes as held.
func (db *DB) BeginMountRelease(ctx context.Context, mountID, taskID int64) error {
	var projectID int64
	if err := db.QueryRowContext(ctx, `SELECT project_id FROM mounts WHERE id=? AND task_id=? AND state='held'`, mountID, taskID).Scan(&projectID); err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE mounts SET state='releasing' WHERE id=? AND task_id=? AND state='held'`, mountID, taskID)
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

// FinishMountRelease is retryable after a crash. A failed Task snapshot write
// may follow the committed idle transition; callers must never relock it.
func (db *DB) FinishMountRelease(ctx context.Context, mountID, taskID int64) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `UPDATE mounts SET state='idle',task_id=NULL,released_at=? WHERE id=? AND task_id=? AND state='releasing'`, now, mountID, taskID)
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
	result, err = tx.ExecContext(ctx, `UPDATE tasks SET mount_id=NULL,updated_at=? WHERE id=? AND mount_id=?`, now, taskID, mountID)
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

func (db *DB) UpdateTaskMountPath(ctx context.Context, taskID, mountID int64, path string) error {
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET mount_id=?,worktree_path=?,updated_at=? WHERE id=?`, mountID, path, time.Now().UnixMilli(), taskID); err != nil {
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

func (db *DB) BreakMount(ctx context.Context, mountID, taskID int64) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `UPDATE mounts SET state='broken',task_id=NULL,released_at=? WHERE id=? AND task_id=? AND state='held'`, now, mountID, taskID)
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
	result, err = tx.ExecContext(ctx, `UPDATE tasks SET mount_id=NULL,updated_at=? WHERE id=? AND mount_id=?`, now, taskID, mountID)
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

func (db *DB) ClaimMountPrune(ctx context.Context, projectID, mountID int64) (bool, error) {
	result, err := db.ExecContext(ctx, `UPDATE mounts SET state='pruning' WHERE id=? AND project_id=? AND state IN ('idle','broken')`, mountID, projectID)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (db *DB) RestoreMountAfterPrune(ctx context.Context, mountID int64, state string) error {
	if state != "idle" && state != "broken" {
		return fmt.Errorf("invalid Mount state after prune: %q", state)
	}
	result, err := db.ExecContext(ctx, `UPDATE mounts SET state=? WHERE id=? AND state='pruning'`, state, mountID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return ErrStateRace
	}
	return err
}

func (db *DB) DeletePrunedMount(ctx context.Context, projectID, mountID int64) error {
	result, err := db.ExecContext(ctx, `DELETE FROM mounts WHERE id=? AND project_id=? AND state='pruning'`, mountID, projectID)
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

func (db *DB) Mounts(ctx context.Context, projectID int64) ([]Mount, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,project_id,n,path,state,COALESCE(task_id,0),acquired_at,released_at FROM mounts WHERE project_id=? ORDER BY n`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	mounts := []Mount{}
	for rows.Next() {
		var mount Mount
		if err := rows.Scan(&mount.ID, &mount.ProjectID, &mount.Number, &mount.Path, &mount.State, &mount.TaskID, &mount.AcquiredAt, &mount.ReleasedAt); err != nil {
			return nil, err
		}
		mounts = append(mounts, mount)
	}
	return mounts, rows.Err()
}

func (db *DB) MountByTask(ctx context.Context, taskID int64) (Mount, error) {
	var mount Mount
	err := db.QueryRowContext(ctx, `SELECT id,project_id,n,path,state,COALESCE(task_id,0),acquired_at,released_at FROM mounts WHERE task_id=?`, taskID).Scan(&mount.ID, &mount.ProjectID, &mount.Number, &mount.Path, &mount.State, &mount.TaskID, &mount.AcquiredAt, &mount.ReleasedAt)
	return mount, err
}

func (db *DB) SetTaskGatedSHA(ctx context.Context, taskID int64, sha string) error {
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET gated_sha=?,updated_at=? WHERE id=? AND state='done'`, sha, time.Now().UnixMilli(), taskID); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

// ClearTaskGatedSHA forgets the commit the Gate passed, for the Task and for
// each workspace member not already landing through a pull request.
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_launch_identities(task_id,launch_number,profile_name,configured_model,model_known) VALUES(?,?,?,?,?)`, taskID, launches, profile, configuredModel, modelKnown); err != nil {
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
func (db *DB) TaskLaunchIdentities(ctx context.Context, taskID int64) ([]TaskLaunchIdentity, error) {
	rows, err := db.QueryContext(ctx, `SELECT task_id,launch_number,profile_name,configured_model,model_known FROM task_launch_identities WHERE task_id=? ORDER BY launch_number`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	identities := []TaskLaunchIdentity{}
	for rows.Next() {
		var identity TaskLaunchIdentity
		var modelKnown int
		if err := rows.Scan(&identity.TaskID, &identity.LaunchNumber, &identity.Profile, &identity.ConfiguredModel, &modelKnown); err != nil {
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
// Nothing restarts its Lead or Riders until `posse up`.
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

func (db *DB) ClaimLeadStart(ctx context.Context, projectID int64, now, staleBefore int64) (bool, error) {
	result, err := db.queries.InsertLeadStartClaim(ctx, dbgen.InsertLeadStartClaimParams{ProjectID: projectID, ClaimedAt: now})
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows == 1 {
		return rows == 1, err
	}
	result, err = db.queries.RefreshLeadStartClaim(ctx, dbgen.RefreshLeadStartClaimParams{ClaimedAt: now, ProjectID: projectID, ClaimedAt_2: staleBefore})
	if err != nil {
		return false, err
	}
	rows, err = result.RowsAffected()
	return rows == 1, err
}

func (db *DB) ReleaseLeadStart(ctx context.Context, projectID int64) error {
	return db.queries.ReleaseLeadStart(ctx, projectID)
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

func (db *DB) UpdateTaskLanding(ctx context.Context, taskID int64, prURL, landedRef string) error {
	if err := db.queries.UpdateTaskLanding(ctx, dbgen.UpdateTaskLandingParams{PrUrl: prURL, LandedRef: landedRef, UpdatedAt: time.Now().UnixMilli(), ID: taskID}); err != nil {
		return err
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
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id=?`, task.ID).Scan(&state); err != nil {
		return "", err
	}
	current := State(state)
	if current == StateStalled {
		if err := transitionTx(ctx, queries, task.ID, StateStalled, StateWorking, "worker", "Rider resumed with a Signal"); err != nil {
			return "", err
		}
		current = StateWorking
	}
	if current != StateWorking {
		return "", fmt.Errorf("%w: Task %d is %q", ErrStateRace, task.ID, current)
	}
	if _, err := queries.InsertSignal(ctx, dbgen.InsertSignalParams{TaskID: task.ID, Verb: verb, Note: note, DataJson: string(encoded), At: time.Now().UnixMilli()}); err != nil {
		return "", err
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
	}
	if verb == "done" && task.Type != "ship" {
		if err := transitionTx(ctx, queries, task.ID, StateDone, StateReported, "cli", "Rider Report copied to the Project home"); err != nil {
			return "", err
		}
		current = StateReported
	}
	if noticeKind != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at) VALUES(?,?,?,?,?,?)`, task.ProjectID, task.ID, noticeKind, note, string(encoded), time.Now().UnixMilli()); err != nil {
			return "", err
		}
	}
	if verb == "done" && data != nil {
		if values, ok := data.(map[string]string); ok && values["pr_url"] != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET pr_url=? WHERE id=?`, values["pr_url"], task.ID); err != nil {
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
	return current, nil
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
        OR type='ship' AND state IN ('done', 'landing')
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
	var message Message
	err := db.QueryRowContext(ctx, `SELECT id, task_id, body, created_at, COALESCE(delivered_at, 0), status, wait_for_idle FROM messages WHERE task_id=? AND status='queued' ORDER BY created_at, id LIMIT 1`, taskID).
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

func (db *DB) UndeliveredTaskMessages(ctx context.Context, taskID int64) ([]Message, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, task_id, body, created_at, COALESCE(delivered_at, 0), status, wait_for_idle
FROM messages WHERE task_id=? AND status IN ('queued', 'claimed', 'undeliverable') ORDER BY created_at, id`, taskID)
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

func (db *DB) ClaimMessage(ctx context.Context, messageID int64, token string, at int64) (bool, error) {
	result, err := db.ExecContext(ctx, `UPDATE messages SET status='claimed',claim_token=?,claimed_at=? WHERE id=? AND status='queued'`, token, at, messageID)
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
	if err := tx.QueryRowContext(ctx, `SELECT t.id,t.state,t.type FROM tasks t JOIN messages m ON m.task_id=t.id WHERE m.id=? AND m.status='claimed' AND m.claim_token=?`, messageID, token).Scan(&taskID, &state, &taskType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrStateRace
		}
		return err
	}
	if !taskAcceptsInstruction(State(state), taskType) {
		return ErrStateRace
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET status='submitting' WHERE id=? AND claim_token=?`, messageID, token); err != nil {
		return err
	}
	// Commit the authorized Task transition before Herdr can expose the
	// instruction. An immediate Rider Signal must see the reopened Task.
	current := State(state)
	if current == StateNeedsDecision || taskType == "ship" && (current == StateDone || current == StateLanding) {
		if err := transitionTx(ctx, db.queries.WithTx(tx.Tx), taskID, current, StateWorking, "lead", "Lead submitted an instruction"); err != nil {
			return err
		}
		if taskType == "ship" && current != StateNeedsDecision {
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
	_, err := db.ExecContext(ctx, `UPDATE messages SET status='queued',claim_token='',claimed_at=0 WHERE id=? AND status='claimed' AND claim_token=?`, messageID, token)
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
	// Only pre-submission claims are safe to retry. `submitting` rows represent
	// an ambiguous external outcome and intentionally remain held for review.
	if _, err := db.ExecContext(ctx, `UPDATE messages SET status='queued',claim_token='',claimed_at=0 WHERE status='claimed' AND claimed_at>0 AND claimed_at<=?`, before); err != nil {
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
	count := 0
	if len(identifiers) == 1 && identifiers[0] == "all" {
		result, err := db.queries.AckAllNotices(ctx, dbgen.AckAllNoticesParams{AckedAt: sql.NullInt64{Int64: at, Valid: true}, ProjectID: projectID})
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
			result, err := db.queries.AckNotice(ctx, dbgen.AckNoticeParams{AckedAt: sql.NullInt64{Int64: at, Valid: true}, ID: id, ProjectID: projectID})
			if err != nil {
				return count, err
			}
			changed, _ := result.RowsAffected()
			count += int(changed)
		}
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
	mountID := int64(0)
	if row.MountID.Valid {
		mountID = row.MountID.Int64
	}
	return Task{ID: row.ID, ProjectID: row.ProjectID, Seq: int(row.Seq), Type: row.Type, ReviewsTaskID: reviewsTaskID,
		Title: row.Title, ShortName: row.ShortName, State: State(row.State), Profile: row.Profile, DispatchRule: row.DispatchRule,
		LandingMode: row.LandingMode, AutonomyReview: row.AutonomyReview, AutonomyLand: row.AutonomyLand,
		Branch: row.Branch, BaseRef: row.BaseRef, WorktreePath: row.WorktreePath, MountID: mountID, Launches: int(row.Launches), GatedSHA: row.GatedSha,
		HerdrWorkspaceID: row.HerdrWorkspaceID, PaneID: row.PaneID, PaneLabel: row.PaneLabel,
		AgentName: row.AgentName, AgentSession: row.AgentSession, AgentAbsentSince: row.AgentAbsentSince, IdleSince: row.IdleSince, AgentServerStartedAt: row.AgentServerStartedAt,
		PRURL: row.PrUrl, LandedRef: row.LandedRef, LastOutputHash: row.LastOutputHash,
		LastWorktreeHash: row.LastWorktreeHash, LastProgressAt: row.LastProgressAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func (db *DB) UpdateTaskObservation(ctx context.Context, taskID int64, paneID, workspaceID, agentSession string, agentAbsentSince, idleSince int64, agentServerStartedAt string) error {
	ctx, cancel := context.WithTimeout(ctx, ObservationWriteBudget)
	defer cancel()
	params := dbgen.UpdateTaskObservationParams{PaneID: paneID, HerdrWorkspaceID: workspaceID, AgentSession: agentSession, AgentAbsentSince: agentAbsentSince, AgentServerStartedAt: agentServerStartedAt, IdleSince: idleSince, UpdatedAt: time.Now().UnixMilli(), ID: taskID}
	if err := db.withBusyTimeout(ctx, reconcileWriteBusyTimeoutMillis, func(conn *sql.Conn) error {
		return dbgen.New(conn).UpdateTaskObservation(ctx, params)
	}); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) UpdateProjectObservation(ctx context.Context, projectID int64, paneID, workspaceID string, absentSince int64) error {
	ctx, cancel := context.WithTimeout(ctx, ObservationWriteBudget)
	defer cancel()
	// Concurrent CLI commands often observe the same Lead pane. Skip the write
	// lock when another reconciliation already recorded this state.
	unchanged, err := db.projectObservationMatches(ctx, projectID, paneID, workspaceID, absentSince)
	if err != nil || unchanged {
		return err
	}
	params := dbgen.UpdateProjectObservationParams{LeadPaneID: paneID, HerdrWorkspaceID: workspaceID, LeadAbsentSince: absentSince, ID: projectID}
	if err := db.withBusyTimeout(ctx, reconcileWriteBusyTimeoutMillis, func(conn *sql.Conn) error {
		return dbgen.New(conn).UpdateProjectObservation(ctx, params)
	}); err != nil {
		return err
	}
	return db.PersistProject(ctx, projectID)
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
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO notices(project_id, task_id, kind, summary, data_json, created_at) VALUES (?, ?, 'worker_exited', ?, '{}', ?)`, task.ProjectID, task.ID, task.Title+" Rider exited after signaling done", now.UnixMilli()); err != nil {
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

// ErrProjectNotEmpty reports a Project that still has Tasks or Mounts.
var ErrProjectNotEmpty = errors.New("project has Tasks or Mounts")

// ProjectContents counts a Project's Tasks (of any state) and Mounts.
func (db *DB) ProjectContents(ctx context.Context, projectID int64) (tasks, mounts int, err error) {
	err = db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM tasks WHERE project_id=?), (SELECT COUNT(*) FROM mounts WHERE project_id=?)`, projectID, projectID).Scan(&tasks, &mounts)
	return tasks, mounts, err
}

// DeleteEmptyProject removes a Project that never had a Task or a Mount, with
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
	var tasks, mounts int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM tasks WHERE project_id=?), (SELECT COUNT(*) FROM mounts WHERE project_id=?)`, projectID, projectID).Scan(&tasks, &mounts); err != nil {
		return err
	}
	if tasks != 0 || mounts != 0 {
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
