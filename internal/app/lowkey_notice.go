package app

import (
	"context"
	"fmt"
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
	for _, notice := range notices {
		if budget <= 0 {
			continue
		}
		summary := strings.Join(strings.Fields(notice.Summary), " ")
		label := "project"
		if db != nil && notice.TaskID != 0 {
			label = noticeTaskTitle(ctx, db, notice.ProjectID, notice.TaskID)
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
			break
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
	if !lowkey {
		return noticeEnvelope(notices, nil, strings.Join(parts, "; ")+extra+". Notice ids: "+strings.Join(ids, ",")+". Lowkey mode off: report every Notice and ack it.")
	}
	return noticeEnvelope(notices, nil, strings.Join(parts, "; ")+extra+". Notice ids: "+strings.Join(ids, ",")+". Lowkey mode on. "+lowkeyReportingRule)
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
