package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

type idleSlotNoticeData struct {
	Idle    []string `json:"idle"`
	Waiting []string `json:"waiting"`
	Release []string `json:"release,omitempty"`
	Active  bool     `json:"episode_active"`
}

func raiseIdleSlotNotice(ctx context.Context, db *store.DB, projectID int64, idleFor time.Duration, now time.Time) ([]store.Notice, error) {
	idle, waiting, err := db.IdleSlotTasks(ctx, projectID, idleFor, now)
	if err != nil {
		return nil, err
	}
	var latestID int64
	var latestData string
	err = db.QueryRowContext(ctx, `SELECT id,data_json FROM notices WHERE project_id=? AND kind='idle_pool_slot' ORDER BY id DESC LIMIT 1`, projectID).Scan(&latestID, &latestData)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	hasPrior := err == nil
	var prior idleSlotNoticeData
	if hasPrior {
		if err := json.Unmarshal([]byte(latestData), &prior); err != nil {
			return nil, err
		}
	}
	if len(idle) == 0 {
		if prior.Active {
			prior.Active = false
			encoded, err := json.Marshal(prior)
			if err != nil {
				return nil, err
			}
			if _, err := db.ExecContext(ctx, `UPDATE notices SET data_json=? WHERE id=?`, string(encoded), latestID); err != nil {
				return nil, err
			}
			if err := db.PersistProject(ctx, projectID); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}

	idleIDs, waitingIDs := taskIDs(idle), taskIDs(waiting)
	data := idleSlotNoticeData{Idle: idleIDs, Waiting: waitingIDs, Active: true}
	project, err := db.ProjectByID(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if !project.IsWorkspace() {
		for _, task := range idle {
			if task.MergeMode == "pr" && task.PRURL != "" {
				data.Release = append(data.Release, taskID(task.Seq))
			}
		}
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	summary := fmt.Sprintf("%s hold Pool slots with nothing pending for %s while %s wait for a slot", strings.Join(idleIDs, ", "), idleFor, strings.Join(waitingIDs, ", "))
	if len(data.Release) > 0 {
		commands := make([]string, len(data.Release))
		for index, id := range data.Release {
			commands[index] = "`posse pr release " + id + " --policy release`"
		}
		summary += ". Free a PR slot with " + strings.Join(commands, " or ")
	}
	if hasPrior && prior.Active {
		if _, err := db.ExecContext(ctx, `UPDATE notices SET summary=?,data_json=? WHERE id=?`, summary, string(encoded), latestID); err != nil {
			return nil, err
		}
		if err := db.PersistProject(ctx, projectID); err != nil {
			return nil, err
		}
		return nil, nil
	}
	notice, err := createNoticeWithData(ctx, db, projectID, 0, "idle_pool_slot", summary, string(encoded), now)
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
