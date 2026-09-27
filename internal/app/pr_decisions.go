package app

import (
	"context"

	"github.com/thanhbinh1905/posse/internal/store"
)

func raiseInvalidPRDecision(ctx context.Context, db *store.DB, project store.Project, task store.Task) error {
	origin := "pr_closed:" + task.PRURL
	decisions, err := db.Decisions(ctx, project.ID, false)
	if err != nil {
		return err
	}
	for _, decision := range decisions {
		if decision.Origin == origin && decision.AnsweredAt != 0 {
			return nil
		}
	}
	_, err = db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: project.ID, TaskID: task.ID, Kind: "pr_closed", Origin: origin, Question: "The recorded URL " + task.PRURL + " is not a pull request. Discard the Task or relaunch the Rider to publish a real PR URL?", Options: []string{"reopen-relaunch", "discard"}})
	return err
}

func raiseClosedPRDecision(ctx context.Context, db *store.DB, project store.Project, task store.Task, url string) error {
	_, err := db.RaiseDecision(ctx, store.DecisionRequest{
		ProjectID: project.ID, TaskID: task.ID, Kind: "pr_closed", Origin: "pr_closed:" + url,
		Question: "Pull request " + url + " closed without merging. Reopen and relaunch the Task or discard it?",
		Options:  []string{"reopen-relaunch", "discard"},
	})
	return err
}
