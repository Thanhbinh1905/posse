package dispatch

import (
	"strings"
	"testing"
)

func TestOldBriefVocabularyIsRejectedOnlyForNewInput(t *testing.T) {
	for _, tc := range []struct{ name, text, replacement string }{
		{"ship", "type: ship\n", "change"},
		{"scout", "type: scout\n", "research"},
		{"mode", "type: change\nlanding_mode: local\n", "merge_mode"},
		{"execution", "type: change\nexecution: mount\n", "sandbox"},
		{"autonomy", "type: change\nautonomy: { land: ask }\n", "autonomy.merge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := "---\n" + tc.text + "title: Exact historical User words\ndone_when: checked\n---\nUser said: Rider ship scout\n"
			if _, err := ParseBriefText(text); err == nil || !strings.Contains(err.Error(), "use "+tc.replacement) {
				t.Fatalf("old input still works or lacks replacement: %v", err)
			}
			historical, err := ParseHistoricalBriefText(text)
			if err != nil || !strings.Contains(historical.Body, "User said: Rider ship scout") {
				t.Fatalf("historical Brief lost compatibility or User words: %#v %v", historical, err)
			}
		})
	}
}
