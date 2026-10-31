package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// writeTx owns the connection as well as its transaction, so a bounded writer
// never leaves its shorter SQLite busy timeout on a pooled connection.
type writeTx struct {
	*sql.Tx
	conn        *sql.Conn
	releaseOnce sync.Once
	releaseErr  error
}

func (tx *writeTx) release() error {
	tx.releaseOnce.Do(func() { tx.releaseErr = restoreBusyTimeoutAndClose(tx.conn) })
	return tx.releaseErr
}

func (tx *writeTx) Commit() error {
	err := tx.Tx.Commit()
	return errors.Join(retryableContention(err), tx.release())
}

func (tx *writeTx) Rollback() error {
	return errors.Join(tx.Tx.Rollback(), tx.release())
}

func restoreBusyTimeoutAndClose(conn *sql.Conn) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, err := conn.ExecContext(ctx, "PRAGMA busy_timeout="+strconv.Itoa(sqliteBusyTimeoutMillis))
	cancel()
	if err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	closeErr := conn.Close()
	if err != nil {
		return fmt.Errorf("restore SQLite busy timeout: %w", retryableContention(err))
	}
	return closeErr
}

type deferrableWritesKey struct{}

// WithDeferrableWrites marks writes the caller can skip under contention, such
// as the observations a read command records before it prints the last
// recorded state. Each waits at most ObservationWriteBusyTimeout for the write
// lock and returns its busy error without retrying. A claim that precedes a
// send ends deferral for the rest of ctx: the write recording that side effect
// must wait the full busy timeout.
func WithDeferrableWrites(ctx context.Context) context.Context {
	return context.WithValue(ctx, deferrableWritesKey{}, new(atomic.Bool))
}

func deferrableWrites(ctx context.Context) bool {
	ended, ok := ctx.Value(deferrableWritesKey{}).(*atomic.Bool)
	return ok && !ended.Load()
}

func endDeferrableWrites(ctx context.Context) {
	if ended, ok := ctx.Value(deferrableWritesKey{}).(*atomic.Bool); ok {
		ended.Store(true)
	}
}

// Busy handlers can outlive context cancellation. A short-lived writer uses
// short lock waits and retries within its context, not SQLite's five-second wait.
func writeBusyTimeout(ctx context.Context) int {
	deadline, bounded := ctx.Deadline()
	if !deferrableWrites(ctx) && (!bounded || time.Until(deadline) > time.Duration(sqliteBusyTimeoutMillis)*time.Millisecond) {
		return sqliteBusyTimeoutMillis
	}
	if !bounded {
		return reconcileWriteBusyTimeoutMillis
	}
	return max(1, min(reconcileWriteBusyTimeoutMillis, int(time.Until(deadline)/time.Millisecond)))
}

func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (result sql.Result, err error) {
	if writeBusyTimeout(ctx) == sqliteBusyTimeoutMillis {
		result, err = db.DB.ExecContext(ctx, query, args...)
		return result, retryableContention(err)
	}
	// Like beginTxWithRetry, keep retrying short lock waits until the caller's
	// deadline. A busy autocommit statement has not applied, so it is safe to rerun.
	for attempt := 0; ; attempt++ {
		err = db.withBusyTimeout(ctx, writeBusyTimeout(ctx), func(conn *sql.Conn) error {
			result, err = conn.ExecContext(ctx, query, args...)
			return err
		})
		if !isBusyErr(err) || ctx.Err() != nil || deferrableWrites(ctx) {
			return result, err
		}
		select {
		case <-ctx.Done():
			return result, err
		case <-time.After(time.Duration(min(attempt+1, 4)) * 50 * time.Millisecond):
		}
	}
}

// BEGIN IMMEDIATE acquires the write lock before reading, avoiding deferred
// transaction upgrades that bypass SQLite's busy handler.
func (db *DB) beginTxWithRetry(ctx context.Context) (*writeTx, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, retryableContention(err)
	}
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(retryableContention(err), restoreBusyTimeoutAndClose(conn))
		}
		if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout="+strconv.Itoa(writeBusyTimeout(ctx))); err != nil {
			return nil, errors.Join(retryableContention(err), restoreBusyTimeoutAndClose(conn))
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err == nil {
			return &writeTx{Tx: tx, conn: conn}, nil
		}
		lastErr = err
		if !isBusyErr(err) || deferrableWrites(ctx) {
			break
		}
		select {
		case <-ctx.Done():
			lastErr = ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 50 * time.Millisecond):
		}
	}
	return nil, errors.Join(retryableContention(lastErr), restoreBusyTimeoutAndClose(conn))
}
