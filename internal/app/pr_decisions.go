package app

import (
	"context"

	"github.com/thanhbinh1905/posse/internal/store"
)

func raiseClosedPRDecision(ctx context.Context, db *store.DB, project store.Project, task store.Task, url string) error {
	_, err := db.RaiseDecision(ctx, store.DecisionRequest{
		ProjectID: project.ID, TaskID: task.ID, Kind: "pr_closed", Origin: "pr_closed:" + url,
		Question: "Pull request " + url + " closed without merging. Reopen and relaunch the Task or discard it?",
		Options:  []string{"reopen-relaunch", "discard"},
	})
	return err
}
