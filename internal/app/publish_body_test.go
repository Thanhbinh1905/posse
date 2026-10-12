package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
)

const testPublishToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestRefreshPublishBodyReplacesOnlyStoredTokenSpan(t *testing.T) {
	forged := managedPublishBody("Human release note", "forged-token")
	original := "Maintainer intro\n\n" + forged + "\n\n" + managedPublishBody("## Summary\n\nOld summary", testPublishToken) + "\n\nMaintainer footer\n"
	updated, err := refreshPublishBody(original, "## Summary\n\nNew summary", testPublishToken, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Maintainer intro", forged, "New summary", "Maintainer footer"} {
		if !strings.Contains(updated, expected) {
			t.Errorf("refreshed body omitted %q: %s", expected, updated)
		}
	}
	if strings.Contains(updated, "Old summary") {
		t.Errorf("refreshed body retained stale Posse content: %s", updated)
	}
	if strings.Count(updated, publishBodyMarker("start", testPublishToken)) != 1 || strings.Count(updated, publishBodyMarker("end", testPublishToken)) != 1 {
		t.Errorf("trusted section markers were duplicated: %s", updated)
	}
}

func TestRefreshPublishBodyAppendsManagedSectionAfterMarkerlessLegacyBody(t *testing.T) {
	legacy := "Human intro\n\n" + legacyPublishTemplate("Old summary") + "\n\nHuman footer\n"
	if _, err := refreshPublishBody(legacy, "## Summary\n\nNew summary", testPublishToken, false); !isPublishBodyConflict(err) {
		t.Fatalf("legacy publish body did not require explicit refresh: %v", err)
	}
	updated, err := refreshPublishBody(legacy, "## Summary\n\nNew summary", testPublishToken, true)
	if err != nil {
		t.Fatal(err)
	}
	assertLegacyBodyAppended(t, legacy, updated, "## Summary\n\nNew summary", testPublishToken)

	republished, err := refreshPublishBody(updated, "## Summary\n\nLatest summary", testPublishToken, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(republished, legacy+"\n\n") || !strings.Contains(republished, "Latest summary") || strings.Contains(republished, "New summary") {
		t.Fatalf("refresh did not preserve legacy bytes and replace only the appended section: %q", republished)
	}
}

func TestRefreshPublishBodyPreservesIssueLinkedCRLFLegacyBody(t *testing.T) {
	legacy := strings.Replace(legacyPublishTemplate("old summary"), "## Issue Link\n\n## Changes", "## Issue Link\n\nCloses #12\nRefs #14\n\n## Changes", 1)
	legacy = "Human preface\n\n" + legacy + "\n\nHuman footer\n"
	legacy = strings.ReplaceAll(legacy, "\n", "\r\n")

	updated, err := refreshPublishBody(legacy, "## Summary\n\nnew summary", testPublishToken, true)
	if err != nil {
		t.Fatal(err)
	}
	assertLegacyBodyAppended(t, legacy, updated, "## Summary\n\nnew summary", testPublishToken)
}

func managedPublishSection(body, token string) (string, bool) {
	startMarker, endMarker := publishBodyMarker("start", token), publishBodyMarker("end", token)
	start, end := strings.Index(body, startMarker), strings.Index(body, endMarker)
	if start < 0 || end < start {
		return "", false
	}
	return body[start : end+len(endMarker)], true
}

func managedPublishTokenFromLog(log string) string {
	start := strings.Index(log, publishBodyStartPrefix)
	if start < 0 {
		return ""
	}
	end := strings.IndexByte(log[start:], '\n')
	if end < 0 {
		return ""
	}
	marker := log[start : start+end]
	return strings.TrimSuffix(strings.TrimPrefix(marker, publishBodyStartPrefix), " -->")
}

func assertLegacyBodyAppended(t *testing.T, legacy, updated, next, token string) {
	t.Helper()
	wantSuffix := "\n\n" + managedPublishBody(next, token)
	if !strings.HasPrefix(updated, legacy) || !strings.HasSuffix(updated, wantSuffix) {
		t.Fatalf("legacy refresh was not append-only: got %q, want original prefix %q and suffix %q", updated, legacy, wantSuffix)
	}
}

func TestRefreshPublishBodyTreatsUnstoredMarkersAsHumanContent(t *testing.T) {
	forged := "```html\n" + managedPublishBody("Human release note: deployment needs approval.", "forged-token") + "\n```\n"
	legacy := forged + "\n" + legacyPublishTemplate("Old summary")
	updated, err := refreshPublishBody(legacy, "new metadata", testPublishToken, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(updated, legacy+"\n\n") || !strings.Contains(updated, "Human release note: deployment needs approval.") {
		t.Fatalf("untrusted markers changed human text: %q", updated)
	}
	if strings.Contains(updated, "Old summary") == false {
		t.Fatal("append-only legacy refresh should retain the old legacy content byte-for-byte")
	}
}

func TestRefreshPublishBodyRejectsDuplicateOrIncompleteStoredMarkers(t *testing.T) {
	owned := managedPublishBody("old", testPublishToken)
	for _, body := range []string{
		owned + "\n\n" + owned,
		publishBodyMarker("start", testPublishToken) + "\npartial\n",
		publishBodyMarker("end", testPublishToken) + "\nreversed\n" + publishBodyMarker("start", testPublishToken),
	} {
		if _, err := refreshPublishBody(body, "new", testPublishToken, true); !isPublishBodyConflict(err) {
			t.Errorf("ambiguous stored markers were not rejected: %q, %v", body, err)
		}
	}
}

func TestRefreshPublishBodyConflictsWithUnstructuredHumanDescription(t *testing.T) {
	if _, err := refreshPublishBody("Human-authored description", "Current Posse content", testPublishToken, true); !isPublishBodyConflict(err) {
		t.Fatalf("unstructured description was not reported as a conflict: %v", err)
	}
}

func isPublishBodyConflict(err error) bool {
	var failure *axi.Error
	return errors.As(err, &failure) && failure.Code == "pr_body_conflict"
}

func legacyPublishTemplate(summary string) string {
	return "## Summary\n\n" + summary + "\n\n" +
		"## Issue Link\n\n" +
		"## Changes\n\n- change\n\n" +
		"## Verification\n\n- [x] old verification\n\n" +
		"## Proof\n\nold proof\n\n" +
		"## Risk And Rollback\n\nRisk: old\nRollback: old\n\n" +
		"## Documentation\n\n" + documentationChecklist
}
