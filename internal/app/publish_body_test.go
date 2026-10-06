package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
)

func TestRefreshPublishBodyReplacesManagedSectionAndPreservesHumanText(t *testing.T) {
	original := "Maintainer intro\n\n" + managedPublishBody("## Summary\n\nOld summary") + "\n\nMaintainer footer\n"
	updated, err := refreshPublishBody(original, "## Summary\n\nNew summary", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Maintainer intro", "New summary", "Maintainer footer"} {
		if !strings.Contains(updated, expected) {
			t.Errorf("refreshed body omitted %q: %s", expected, updated)
		}
	}
	if strings.Contains(updated, "Old summary") {
		t.Errorf("refreshed body retained stale Posse content: %s", updated)
	}
}

func TestRefreshPublishBodyAppendsManagedSectionAfterLegacyBody(t *testing.T) {
	legacy := "Human intro\n\n" + legacyPublishTemplate("Old summary") + "\n\nHuman footer\n"
	if _, err := refreshPublishBody(legacy, "## Summary\n\nNew summary", false); !isPublishBodyConflict(err) {
		t.Fatalf("legacy publish body did not require explicit refresh: %v", err)
	}
	updated, err := refreshPublishBody(legacy, "## Summary\n\nNew summary", true)
	if err != nil {
		t.Fatal(err)
	}
	assertLegacyBodyAppended(t, legacy, updated, "## Summary\n\nNew summary")

	republished, err := refreshPublishBody(updated, "## Summary\n\nLatest summary", false)
	if err != nil {
		t.Fatal(err)
	}
	assertLegacyBodyAppended(t, legacy, republished, "## Summary\n\nLatest summary")
	if strings.Contains(republished, managedPublishBody("## Summary\n\nNew summary")) {
		t.Errorf("refresh left the stale managed block: %s", republished)
	}
}

func TestRefreshPublishBodyPreservesIssueLinkedCRLFLegacyBody(t *testing.T) {
	legacy := strings.Replace(legacyPublishTemplate("old summary"), "## Issue Link\n\n## Changes", "## Issue Link\n\nCloses #12\nRefs #14\n\n## Changes", 1)
	legacy = "Human preface\n\n" + legacy + "\n\nHuman footer\n"
	legacy = strings.ReplaceAll(legacy, "\n", "\r\n")

	updated, err := refreshPublishBody(legacy, "## Summary\n\nnew summary", true)
	if err != nil {
		t.Fatal(err)
	}
	assertLegacyBodyAppended(t, legacy, updated, "## Summary\n\nnew summary")
}

func assertLegacyBodyAppended(t *testing.T, legacy, updated, next string) {
	t.Helper()
	wantSuffix := "\n\n" + managedPublishBody(next)
	if !strings.HasPrefix(updated, legacy) || !strings.HasSuffix(updated, wantSuffix) {
		t.Fatalf("legacy refresh was not append-only: got %q, want original prefix %q and suffix %q", updated, legacy, wantSuffix)
	}
}

func TestRefreshPublishBodyAppendsEditsWithoutParsingLegacyBody(t *testing.T) {
	legacy := legacyPublishTemplate("old summary")
	for _, insertion := range []string{
		"## Maintainer Review\n\nKeep this note.\n\n",
		"Maintainer Review\n=\n\nKeep this note.\n\n",
		"<h2>Maintainer Review</h2>\n\nKeep this note.\n\n",
		"Maintainer review requires deployment approval.\n\n",
	} {
		body := strings.Replace(legacy, "## Verification", insertion+"## Verification", 1)
		updated, err := refreshPublishBody(body, "new", true)
		if err != nil {
			t.Errorf("legacy content with inserted text was not appended: insertion=%q, err=%v", insertion, err)
			continue
		}
		assertLegacyBodyAppended(t, body, updated, "new")
	}
	partial := publishBodyStart + "\npartial\n"
	if _, err := refreshPublishBody(partial, "new", true); !isPublishBodyConflict(err) {
		t.Errorf("incomplete ownership markers were not rejected: %q, %v", partial, err)
	}
}

func TestRefreshPublishBodyConflictsWithUnstructuredHumanDescription(t *testing.T) {
	if _, err := refreshPublishBody("Human-authored description", "Current Posse content", true); !isPublishBodyConflict(err) {
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
