package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

// raiseIdleSlotNotice raises one project Notice per distinct set of idle and
// waiting Tasks, so an ack is not followed by a repeat for the same situation.
func raiseIdleSlotNotice(ctx context.Context, db *store.DB, projectID int64, idleFor time.Duration, now time.Time) ([]store.Notice, error) {
	idle, waiting, err := db.IdleSlotTasks(ctx, projectID, idleFor, now)
	if err != nil || len(idle) == 0 {
		return nil, err
	}
	idleIDs, waitingIDs := taskIDs(idle), taskIDs(waiting)
	data, err := json.Marshal(map[string][]string{"idle": idleIDs, "waiting": waitingIDs})
	if err != nil {
		return nil, err
	}
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notices WHERE project_id=? AND kind='idle_pool_slot' AND data_json=?)`, projectID, string(data)).Scan(&exists); err != nil || exists {
		return nil, err
	}
	summary := fmt.Sprintf("%s hold Pool slots with nothing pending for %s while %s wait for a slot", strings.Join(idleIDs, ", "), idleFor, strings.Join(waitingIDs, ", "))
	notice, err := createNoticeWithData(ctx, db, projectID, 0, "idle_pool_slot", summary, string(data), now)
	if err != nil {
		return nil, err
	}
	return []store.Notice{notice}, nil
}

func taskIDs(tasks []store.Task) []string {
	ids := make([]string, len(tasks))
	for i, task := range tasks {
		ids[i] = taskID(task.Seq)
	}
	return ids
}
