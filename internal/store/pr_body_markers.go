package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	dbgen "github.com/thanhbinh1905/posse/internal/store/sqlc"
)

// PRBodyMarker records the token Posse may use to replace one PR/MR body span.
type PRBodyMarker struct {
	TaskID int64
	Repo   string
	PRURL  string
	Token  string
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
	if err == nil {
		if current.PrUrl == prURL {
			if err := tx.Commit(); err != nil {
				return "", err
			}
			return current.MarkerToken, nil
		}
		if prURL != "" && current.PrUrl == "" {
			result, err := queries.BindPRBodyMarker(ctx, dbgen.BindPRBodyMarkerParams{
				PrUrl: prURL, UpdatedAt: time.Now().UnixMilli(), TaskID: taskID,
				Repo: repo, MarkerToken: current.MarkerToken, PrUrl_2: prURL,
			})
			if err != nil {
				return "", err
			}
			if changed, _ := result.RowsAffected(); changed != 1 {
				return "", sql.ErrNoRows
			}
			if err := tx.Commit(); err != nil {
				return "", err
			}
			return current.MarkerToken, nil
		}
	} else if err != sql.ErrNoRows {
		return "", err
	}

	token, err := newPRBodyMarkerToken()
	if err != nil {
		return "", err
	}
	if err := queries.UpsertPRBodyMarker(ctx, dbgen.UpsertPRBodyMarkerParams{
		TaskID: taskID, Repo: repo, PrUrl: prURL, MarkerToken: token, UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
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
		return nil
	}
	current, err := db.GetPRBodyMarker(ctx, taskID, repo)
	if err == nil && current.PRURL == prURL && current.Token == token {
		return nil
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

func newPRBodyMarkerToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate PR body marker token: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
