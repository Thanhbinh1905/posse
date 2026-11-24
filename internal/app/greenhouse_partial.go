package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/greenhouse"
	"github.com/thanhbinh1905/posse/internal/store"
)

func greenhouseCoverageDecisionOrigin(taskSeq int, repository, head string) string {
	return fmt.Sprintf("greenhouse-coverage:%s:%s:%s", taskIDString(taskSeq), repository, head)
}

func parseGreenhouseCoverageDecisionOrigin(origin string, taskSeq int) (string, string, bool) {
	prefix := "greenhouse-coverage:" + taskIDString(taskSeq) + ":"
	value, ok := strings.CutPrefix(origin, prefix)
	if !ok {
		return "", "", false
	}
	separator := strings.LastIndex(value, ":")
	if separator < 1 {
		return "", "", false
	}
	repository, head := value[:separator], value[separator+1:]
	return repository, head, true
}

func greenhousePartialReviewApplies(ctx context.Context, db *store.DB, task store.Task, repository, head string) (bool, error) {
	var noticeExists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notices WHERE task_id=? AND kind='greenhouse_partial_review' AND json_extract(data_json,'$.repo')=? AND json_extract(data_json,'$.head')=?)`, task.ID, repository, head).Scan(&noticeExists); err != nil {
		return false, err
	}
	if !noticeExists {
		return false, nil
	}
	verdicts, err := db.VerdictsFor(ctx, task.ID)
	if err != nil {
		return false, err
	}
	var latest *store.Verdict
	for index := len(verdicts) - 1; index >= 0; index-- {
		verdict := verdicts[index]
		if verdict.Provider == store.GreenhouseReviewProvider && verdict.Repository == repository && verdict.Head == head && verdict.Mode == "gate" {
			latest = &verdict
			break
		}
	}
	if latest == nil || !latest.Qualifies(head) {
		return true, nil
	}
	if err := db.ObsoleteGreenhouseCoverageDecision(ctx, task.ID, repository, head, "A later complete Greenhouse Verdict superseded the partial review"); err != nil {
		return false, err
	}
	return false, nil
}

func greenhousePartialDecisionQuestion(repository, head string, result greenhouse.HostResult) string {
	var question strings.Builder
	fmt.Fprintf(&question, "Greenhouse reviewed Member %s at Task head %s partially. The uncovered paths were not reviewed:\n", repository, head)
	for _, path := range result.Uncovered {
		fmt.Fprintf(&question, "- %s: %s\n", path.Path, path.Reason)
	}
	if len(result.Generated) > 0 {
		question.WriteString("Generated paths were reviewed by provenance, not omitted:\n")
		for _, path := range result.Generated {
			fmt.Fprintf(&question, "- %s (+%d/-%d)", path.Path, path.Added, path.Removed)
			if len(path.Generators) > 0 {
				fmt.Fprintf(&question, "; generators: %s", strings.Join(path.Generators, ", "))
			}
			question.WriteByte('\n')
		}
	}
	question.WriteString("Choose `accept-coverage-at-exact-head` to accept coverage of the reviewed paths at this exact head, or `hold` to stop.")
	return question.String()
}
