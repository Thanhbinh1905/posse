package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/thanhbinh1905/posse/internal/store"
)

type branchRewrite struct {
	FromHead string `json:"from_head"`
	ToHead   string `json:"to_head"`
}

// recordBranchRewrite keeps the User-approved replacement of a published Task
// head. Notices are never deleted, so the record also serves as lineage.
func recordBranchRewrite(ctx context.Context, db *store.DB, project store.Project, task store.Task, repo, fromHead, toHead, quote string) error {
	summary := fmt.Sprintf("%s: published Task branch rewritten from %s to %s with User approval: %q", task.Title, shortSHA(fromHead), shortSHA(toHead), quote)
	data := marshalJSON(map[string]any{"repo": repo, "from_head": fromHead, "to_head": toHead, "user_quote": quote})
	_, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "branch_rewritten", Summary: summary, DataJSON: data})
	return err
}

func branchRewrites(ctx context.Context, db *store.DB, taskID int64) ([]branchRewrite, error) {
	rows, err := db.QueryContext(ctx, `SELECT data_json FROM notices WHERE task_id=? AND kind='branch_rewritten' ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rewrites []branchRewrite
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var rewrite branchRewrite
		if err := json.Unmarshal([]byte(data), &rewrite); err != nil {
			return nil, fmt.Errorf("decode branch_rewritten Notice: %w", err)
		}
		rewrites = append(rewrites, rewrite)
	}
	return rewrites, rows.Err()
}

// descendsThroughRewrites reports whether head descends from ancestor in Git
// or through the Task's recorded branch rewrites.
func descendsThroughRewrites(ctx context.Context, db *store.DB, root string, taskID int64, ancestor, head string) (bool, error) {
	if gitAncestor(ctx, root, ancestor, head) {
		return true, nil
	}
	rewrites, err := branchRewrites(ctx, db, taskID)
	if err != nil {
		return false, err
	}
	reached := []string{ancestor}
	for _, rewrite := range rewrites {
		for _, from := range reached {
			if gitAncestor(ctx, root, from, rewrite.FromHead) {
				if gitAncestor(ctx, root, rewrite.ToHead, head) {
					return true, nil
				}
				reached = append(reached, rewrite.ToHead)
				break
			}
		}
	}
	return false, nil
}
