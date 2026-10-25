package runtime

import (
	"context"
	"errors"

	"github.com/thanhbinh1905/posse/internal/store"
)

// A completed Worker's turn-end baseline is the repository fingerprint paired
// with an idle or done status. The status comes from a snapshot taken once per
// pass, so a fingerprint read later can already contain the next turn's change.
// That change would become the baseline, and the next working observation would
// see nothing to reopen the Task for (#492). Reading the fingerprint before the
// snapshot keeps every baseline no newer than the status it pairs with.

type turnEndBaselinesKey struct{}

type repositoryState struct {
	fingerprint string
	err         error
}

var errNoTurnEndBaseline = errors.New("no repository fingerprint predates the Herdr snapshot")

// captureTurnEndBaselines fingerprints every completed Change Task before the
// pass takes its Herdr snapshot. A failed Task listing yields an empty map, so
// each turn end in the pass records no baseline and a new turn reopens.
func captureTurnEndBaselines(ctx context.Context, db *store.DB, projectID int64) map[int64]repositoryState {
	baselines := map[int64]repositoryState{}
	tasks, err := db.LiveTasks(ctx, projectID)
	if err != nil {
		return baselines
	}
	for _, task := range tasks {
		if task.Type != "change" || (task.State != store.StateDone && task.State != store.StateMerging) {
			continue
		}
		fingerprint, err := repositoryFingerprint(ctx, task.WorktreePath)
		baselines[task.ID] = repositoryState{fingerprint: fingerprint, err: err}
	}
	return baselines
}

func withTurnEndBaselines(ctx context.Context, baselines map[int64]repositoryState) context.Context {
	return context.WithValue(ctx, turnEndBaselinesKey{}, baselines)
}

// turnEndFingerprint returns the fingerprint captured before the pass snapshot.
// Without a captured set, as when a caller reconciles its own snapshot, it
// falls back to reading the repository now.
func turnEndFingerprint(ctx context.Context, task store.Task, fallback func() (string, error)) func() (string, error) {
	baselines, ok := ctx.Value(turnEndBaselinesKey{}).(map[int64]repositoryState)
	if !ok {
		return fallback
	}
	return func() (string, error) {
		state, ok := baselines[task.ID]
		if !ok {
			return "", errNoTurnEndBaseline
		}
		return state.fingerprint, state.err
	}
}
