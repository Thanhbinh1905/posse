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

func TestRefreshPublishBodyRequiresExplicitLegacyAdoption(t *testing.T) {
	legacy := "Human intro\n\n" + legacyPublishTemplate("Old summary") + "\n\nHuman footer\n"
	if _, err := refreshPublishBody(legacy, "## Summary\n\nNew summary", false); !isPublishBodyConflict(err) {
		t.Fatalf("legacy publish body did not require explicit adoption: %v", err)
	}
	updated, err := refreshPublishBody(legacy, "## Summary\n\nNew summary", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Human intro", "New summary", "Human footer"} {
		if !strings.Contains(updated, expected) {
			t.Errorf("adopted body omitted %q: %s", expected, updated)
		}
	}
	if strings.Contains(updated, "Old summary") {
		t.Errorf("adopted body retained stale Posse content: %s", updated)
	}
}

func TestRefreshPublishBodyRefusesAmbiguousLegacyAndMarkers(t *testing.T) {
	legacyWithHumanSection := strings.Replace(legacyPublishTemplate("old summary"), "## Verification", "## Maintainer Review\n\nKeep this note.\n\n## Verification", 1)
	legacyWithSetextHumanSection := strings.Replace(legacyPublishTemplate("old summary"), "## Verification", "Maintainer Review\n---\n\nKeep this note.\n\n## Verification", 1)
	for _, body := range []string{
		"## Summary\n\nold, edited legacy description",
		publishBodyStart + "\npartial\n",
		legacyWithHumanSection,
		legacyWithSetextHumanSection,
	} {
		if _, err := refreshPublishBody(body, "new", true); !isPublishBodyConflict(err) {
			t.Errorf("ambiguous description was not rejected: %q, %v", body, err)
		}
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
		"## Issue Link\n\n\n" +
		"## Changes\n\n- change\n\n" +
		"## Verification\n\n- [x] old verification\n\n" +
		"## Proof\n\nold proof\n\n" +
		"## Risk And Rollback\n\nRisk: old\nRollback: old\n\n" +
		"## Documentation\n\n" + documentationChecklist
}
