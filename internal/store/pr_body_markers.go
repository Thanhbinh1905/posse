package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	dbgen "github.com/thanhbinh1905/posse/internal/store/sqlc"
)

// PRBodyMarker records the token Posse may use to replace one PR/MR body span.
type PRBodyMarker struct {
	TaskID    int64  `toml:"task_id"`
	Repo      string `toml:"repo"`
	PRURL     string `toml:"pr_url"`
	Token     string `toml:"token"`
	UpdatedAt int64  `toml:"updated_at"`
}

// EnsurePRBodyMarker returns the token for this Task member's PR. An empty URL
// reserves a token before creating a PR so retries can adopt a created PR after
// losing the forge's acknowledgement.
func (db *DB) EnsurePRBodyMarker(ctx context.Context, taskID int64, repo, prURL string) (string, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	queries := db.queries.WithTx(tx.Tx)
	current, err := queries.PRBodyMarkerByTaskRepo(ctx, dbgen.PRBodyMarkerByTaskRepoParams{TaskID: taskID, Repo: repo})
	var token string
	switch {
	case err == nil && current.PrUrl == prURL:
		token = current.MarkerToken
	case err == nil && prURL != "" && current.PrUrl == "":
		result, bindErr := queries.BindPRBodyMarker(ctx, dbgen.BindPRBodyMarkerParams{
			PrUrl: prURL, UpdatedAt: time.Now().UnixMilli(), TaskID: taskID,
			Repo: repo, MarkerToken: current.MarkerToken, PrUrl_2: prURL,
		})
		if bindErr != nil {
			return "", bindErr
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return "", sql.ErrNoRows
		}
		token = current.MarkerToken
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return "", err
	default:
		token, err = newPRBodyMarkerToken()
		if err != nil {
			return "", err
		}
		if err := queries.UpsertPRBodyMarker(ctx, dbgen.UpsertPRBodyMarkerParams{
			TaskID: taskID, Repo: repo, PrUrl: prURL, MarkerToken: token, UpdatedAt: time.Now().UnixMilli(),
		}); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if err := db.PersistTask(ctx, taskID); err != nil {
		return "", err
	}
	return token, nil
}

// BindPRBodyMarker associates a pre-created token with the URL returned by the
// forge. It is idempotent when the token is already bound to that URL.
func (db *DB) BindPRBodyMarker(ctx context.Context, taskID int64, repo, prURL, token string) error {
	result, err := db.queries.BindPRBodyMarker(ctx, dbgen.BindPRBodyMarkerParams{
		PrUrl: prURL, UpdatedAt: time.Now().UnixMilli(), TaskID: taskID,
		Repo: repo, MarkerToken: token, PrUrl_2: prURL,
	})
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed == 1 {
		return db.PersistTask(ctx, taskID)
	}
	current, err := db.GetPRBodyMarker(ctx, taskID, repo)
	if err == nil && current.PRURL == prURL && current.Token == token {
		return db.PersistTask(ctx, taskID)
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("PR body marker changed before it could be bound")
}

// GetPRBodyMarker returns the marker recorded for a Task member.
func (db *DB) GetPRBodyMarker(ctx context.Context, taskID int64, repo string) (PRBodyMarker, error) {
	row, err := db.queries.PRBodyMarkerByTaskRepo(ctx, dbgen.PRBodyMarkerByTaskRepoParams{TaskID: taskID, Repo: repo})
	if err != nil {
		return PRBodyMarker{}, err
	}
	return PRBodyMarker{TaskID: taskID, Repo: repo, PRURL: row.PrUrl, Token: row.MarkerToken}, nil
}

// PRBodyMarkersForTask returns every forge body marker for a Task and its members.
func (db *DB) PRBodyMarkersForTask(ctx context.Context, taskID int64) ([]PRBodyMarker, error) {
	rows, err := db.queries.PRBodyMarkersByTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	markers := make([]PRBodyMarker, 0, len(rows))
	for _, row := range rows {
		markers = append(markers, PRBodyMarker{TaskID: row.TaskID, Repo: row.Repo, PRURL: row.PrUrl, Token: row.MarkerToken, UpdatedAt: row.UpdatedAt})
	}
	return markers, nil
}

func newPRBodyMarkerToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate PR body marker token: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
