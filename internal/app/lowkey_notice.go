package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/thanhbinh1905/posse/internal/store"
)

// Each lowkey wake is self-contained, even when the Lead was launched before
// lowkey mode was enabled. Keep untrusted Notice text on one bounded line.
func noticeWakeMessage(ctx context.Context, db *store.DB, notices []store.Notice, lowkey bool) string {
	// The header is routing metadata, not a Task label. Keep it constant so
	// an untrusted title cannot forge a Posse envelope.
	parts := make([]string, 0, len(notices))
	ids := make([]string, 0, len(notices))
	budget := 500
	messageBudget := 4000
	capReached := false
	for _, notice := range notices {
		capReached = capReached || notice.Kind == "review_cap_reached"
		if notice.Kind == leadMessageNoticeKind {
			entry := fmt.Sprintf("Notice %d: %s", notice.ID, strconv.Quote(notice.Summary))
			if len(entry)+2 > messageBudget {
				entry = fmt.Sprintf("Notice %d: Lead message (read the full text with `posse --full`)", notice.ID)
			}
			if len(entry)+2 <= messageBudget {
				parts = append(parts, entry)
				ids = append(ids, fmt.Sprint(notice.ID))
				messageBudget -= len(entry) + 2
			}
			continue
		}
		if budget <= 0 {
			continue
		}
		summary := strings.Join(strings.Fields(notice.Summary), " ")
		label := "project"
		if db != nil && notice.TaskID != 0 {
			label = noticeTaskID(ctx, db, notice.ProjectID, notice.TaskID)
		}
		if label == "" {
			label = "project"
		}
		prefix := label + ": "
		if label == "project" {
			prefix = "project " + strings.ReplaceAll(strings.TrimPrefix(notice.Kind, "task_"), "_", "-") + " - "
		} else if strings.HasPrefix(summary, prefix) {
			prefix = ""
		}
		if utf8.RuneCountInString(prefix) >= budget {
			budget = 0
			continue
		}
		entry := prefix + clipRunes(summary, min(150, budget-utf8.RuneCountInString(prefix)))
		parts = append(parts, entry)
		ids = append(ids, fmt.Sprint(notice.ID))
		budget -= utf8.RuneCountInString(entry) + 2
	}
	extra := ""
	if len(parts) < len(notices) {
		extra = fmt.Sprintf("; +%d more (run `posse --full`)", len(notices)-len(parts))
	}
	body := strings.Join(parts, "; ") + extra + ". Notice ids: " + strings.Join(ids, ",") + "."
	if capReached {
		body += " Check `posse decisions` for pending Review-cap Decisions. When a User choice is required, present the exact head and available options and ask for a plain-language reply here. Do not give the User a command to run."
	}
	if !lowkey {
		return noticeEnvelope(notices, nil, body+" Lowkey mode off: report every Notice and ack it.")
	}
	return noticeEnvelope(notices, nil, body+" "+lowkeyReportingRule)
}

func clipRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	if limit < 4 {
		return string([]rune(value)[:max(0, limit)])
	}
	return string([]rune(value)[:limit-3]) + "..."
}
