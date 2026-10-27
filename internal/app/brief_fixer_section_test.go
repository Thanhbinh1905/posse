package app

import (
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

const briefWithIntent = "---\ntype: change\ntitle: T\ndone_when: done\n---\nLead assumption text.\n\n## Intent\nUser's words (verbatim): \"ship it\"\n\n## Notes\nLead notes.\n"
const briefWithoutIntent = "---\ntype: change\ntitle: T\ndone_when: done\n---\nLead assumption text.\n"

func briefPlan(brief string) *taskplan.Plan {
	return &taskplan.Plan{Inputs: []taskplan.Input{taskplan.Select("brief", "change.md", []byte(brief))}}
}

func TestBriefFixerSectionPrintsOnlyIntentBlock(t *testing.T) {
	label, text := briefFixerSection(briefPlan(briefWithIntent))
	if label != "User intent (verbatim)" || text != "User's words (verbatim): \"ship it\"" {
		t.Fatalf("label=%q text=%q", label, text)
	}
}

func TestBriefFixerSectionFallsBackToWholeBody(t *testing.T) {
	label, text := briefFixerSection(briefPlan(briefWithoutIntent))
	if label != "Change Brief (verbatim)" || text != "Lead assumption text." {
		t.Fatalf("label=%q text=%q", label, text)
	}
}

func TestGeneratedReviewBriefSeparatesIntentFromLeadText(t *testing.T) {
	with := string(generatedReviewBrief(store.Task{}, *briefPlan(briefWithIntent), 1, nil, false))
	if !strings.Contains(with, "## User intent (verbatim)\n\nUser's words") || strings.Contains(with, "Lead assumption text.") || strings.Contains(with, "Change Brief (verbatim)") {
		t.Fatalf("Review Round brief with Intent block: %s", with)
	}
	without := string(generatedReviewBrief(store.Task{}, *briefPlan(briefWithoutIntent), 1, nil, false))
	if !strings.Contains(without, "## Change Brief (verbatim)\n\nLead assumption text.") {
		t.Fatalf("Review Round brief without Intent block: %s", without)
	}
}
