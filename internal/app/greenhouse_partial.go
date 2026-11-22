package app

import (
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/greenhouse"
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
