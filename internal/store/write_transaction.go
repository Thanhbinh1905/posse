package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"sync"
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
	return errors.Join(retryableContention(tx.Tx.Commit()), tx.release())
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

// Busy handlers can outlive context cancellation. A short-lived writer uses
// short lock waits and retries within its context, not SQLite's five-second wait.
func writeBusyTimeout(ctx context.Context) int {
	deadline, bounded := ctx.Deadline()
	if !bounded || time.Until(deadline) > time.Duration(sqliteBusyTimeoutMillis)*time.Millisecond {
		return sqliteBusyTimeoutMillis
	}
	return max(1, min(reconcileWriteBusyTimeoutMillis, int(time.Until(deadline)/time.Millisecond)))
}

func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (result sql.Result, err error) {
	if writeBusyTimeout(ctx) == sqliteBusyTimeoutMillis {
		result, err = db.DB.ExecContext(ctx, query, args...)
		return result, retryableContention(err)
	}
	err = db.withBusyTimeout(ctx, writeBusyTimeout(ctx), func(conn *sql.Conn) error {
		result, err = conn.ExecContext(ctx, query, args...)
		return err
	})
	return result, err
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
		if !isBusyErr(err) {
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
