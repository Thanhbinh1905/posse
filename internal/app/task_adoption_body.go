package app

import (
	"context"

	"github.com/thanhbinh1905/posse/internal/store"
)

func adoptedPullRequestBodyToken(ctx context.Context, db *store.DB, task store.Task, prURL, body string) (string, error) {
	for _, token := range tokenizedPublishMarkerPairs(body) {
		var authorized bool
		err := db.QueryRowContext(ctx, `WITH RECURSIVE lineage(id) AS (
   SELECT source_task_id FROM task_adoptions WHERE task_id=? AND pr_url=?
   UNION SELECT a.source_task_id FROM task_adoptions a JOIN lineage l ON a.task_id=l.id WHERE a.pr_url=?
  ) SELECT EXISTS(SELECT 1 FROM lineage l JOIN pr_body_markers m ON m.task_id=l.id JOIN tasks t ON t.id=? WHERE m.marker_token=? AND m.pr_url=? AND t.pr_url=?)`, task.ID, prURL, prURL, task.ID, token, prURL, prURL).Scan(&authorized)
		if err != nil {
			return "", err
		}
		if authorized {
			return token, nil
		}
	}
	return "", nil
}
