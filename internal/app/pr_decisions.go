package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/store"
)

func raiseInvalidPRDecision(ctx context.Context, db *store.DB, project store.Project, task store.Task) error {
	origin := "pr_closed:invalid:" + strconv.FormatInt(task.ID, 10) + ":" + task.PRURL
	decisions, err := db.Decisions(ctx, project.ID, false)
	if err != nil {
		return err
	}
	for _, decision := range decisions {
		if decision.TaskID == task.ID && decision.Origin == origin && decision.AnsweredAt != 0 {
			return nil
		}
	}
	_, err = db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: project.ID, TaskID: task.ID, Kind: "pr_closed", Origin: origin, Question: "The recorded URL " + task.PRURL + " is not a pull request. Discard the Task or relaunch the Worker to publish a real PR URL?", Options: []string{"reopen-relaunch", "discard"}})
	if err != nil {
		return err
	}
	return db.ObsoletePendingPRDecision(ctx, task.ID, "pr_closed:"+task.PRURL, "PR URL no longer resolves to a pull request")
}

func closedPRDecisionURL(origin string) string {
	url := strings.TrimPrefix(origin, "pr_closed:")
	if strings.HasPrefix(url, "invalid:") {
		_, url, _ = strings.Cut(strings.TrimPrefix(url, "invalid:"), ":")
	}
	return url
}

func raiseClosedPRDecision(ctx context.Context, db *store.DB, project store.Project, task store.Task, url string) error {
	invalid := "pr_closed:invalid:" + strconv.FormatInt(task.ID, 10) + ":" + url
	decisions, err := db.Decisions(ctx, project.ID, true)
	if err != nil {
		return err
	}
	for _, decision := range decisions {
		if decision.TaskID == task.ID && decision.Origin == invalid {
			return nil
		}
	}
	_, err = db.RaiseDecision(ctx, store.DecisionRequest{
		ProjectID: project.ID, TaskID: task.ID, Kind: "pr_closed", Origin: "pr_closed:" + url,
		Question: "Pull request " + url + " closed without merging. Reopen and relaunch the Task or discard it?",
		Options:  []string{"reopen-relaunch", "discard"},
	})
	return err
}

// A forge merged a head Posse never gated, usually after history was rewritten
// outside `posse publish`. The guard keeps refusing it; the Lead gets one
// Notice per merged head instead of a pr_watch_failing streak.
func raiseUngatedMergedHeadNotice(ctx context.Context, db *store.DB, project store.Project, task store.Task, worktree, repo, prURL, gatedSHA, mergedHead string) error {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notices WHERE task_id=? AND kind='pr_merged_ungated' AND COALESCE(json_extract(data_json,'$.repo'),'')=? AND json_extract(data_json,'$.merged_head')=?)`, task.ID, repo, mergedHead).Scan(&exists); err != nil || exists {
		return err
	}
	trees := "unknown"
	if worktree != "" && gatedSHA != "" {
		gatedTree, gatedErr := gitOutput(ctx, worktree, "rev-parse", "--verify", "--quiet", gatedSHA+"^{tree}")
		mergedTree, mergedErr := gitOutput(ctx, worktree, "rev-parse", "--verify", "--quiet", mergedHead+"^{tree}")
		if gatedErr == nil && mergedErr == nil {
			trees = map[bool]string{true: "match", false: "differ"}[gatedTree == mergedTree]
		}
	}
	recovery := "Check that " + mergedHead + " holds only reviewed work, then run `git -C " + worktree + " reset --keep " + mergedHead + "` on branch " + task.Branch + " (fetch it first if missing); the next PR poll settles pr_merged."
	target := prURL
	if repo != "" {
		target = repo + " " + prURL
	}
	summary := fmt.Sprintf("%s: %s merged source head %s, but the gated head is %s (trees %s). Posse leaves the Task %s. %s", task.Title, target, mergedHead, gatedSHA, trees, task.State, recovery)
	data := map[string]any{"pr_url": prURL, "gated_head": gatedSHA, "merged_head": mergedHead, "trees": trees, "recovery": recovery}
	if repo != "" {
		data["repo"] = repo
	}
	_, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "pr_merged_ungated", Summary: summary, DataJSON: marshalJSON(data)})
	return err
}

func headOnTaskBranch(ctx context.Context, worktree, branch, head string) bool {
	current, err := gitOutput(ctx, worktree, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || current != branch {
		return false
	}
	_, err = gitOutput(ctx, worktree, "merge-base", "--is-ancestor", head, "HEAD")
	return err == nil
}
