//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
)

func TestPublishRetainsRepeatedMetadataFlagsOnCreateAndRefresh(t *testing.T) {
	fixture := t205NewPRFixture(t, "github")
	firstVerification := []string{"first verify", "second verify", "third verify"}
	firstProof := []string{"First proof paragraph.", "Second proof paragraph.", "Third proof paragraph."}
	firstRisk := []string{"Risk: first risk.", "Rollback: first rollback.", "Additional: first note."}
	fixture.publishOK(t, repeatedMetadataArgs("first", firstVerification, firstProof, firstRisk)...)
	marker, err := fixture.db.GetPRBodyMarker(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	createdBlock := t205ManagedBlock(t, fixture.body(t), marker.Token)
	if createdBlock == "" {
		t.Fatalf("created PR has no A11 managed block for token %q: %s", marker.Token, fixture.body(t))
	}
	assertRepeatedMetadata(t, "create", createdBlock, firstVerification, firstProof, firstRisk)

	updatedVerification := []string{"updated verify one", "updated verify two", "updated verify three"}
	updatedProof := []string{"Updated proof one.", "Updated proof two.", "Updated proof three."}
	updatedRisk := []string{"Risk: updated risk.", "Rollback: updated rollback.", "Additional: updated note."}
	fixture.publishOK(t, repeatedMetadataArgs("updated", updatedVerification, updatedProof, updatedRisk)...)
	refreshedBlock := t205ManagedBlock(t, fixture.body(t), marker.Token)
	if refreshedBlock == "" {
		t.Fatalf("refreshed PR has no A11 managed block for token %q: %s", marker.Token, fixture.body(t))
	}
	assertRepeatedMetadata(t, "refresh", refreshedBlock, updatedVerification, updatedProof, updatedRisk)
	staleValues := append(append(append([]string{}, firstVerification...), firstProof...), firstRisk...)
	for _, stale := range staleValues {
		if strings.Contains(refreshedBlock, stale) {
			t.Errorf("refreshed managed block retained stale value %q: %s", stale, refreshedBlock)
		}
	}
}

func repeatedMetadataArgs(label string, verification, proof, risk []string) []string {
	args := []string{label + " summary"}
	for _, value := range verification {
		args = append(args, "--verify", value)
	}
	for _, value := range proof {
		args = append(args, "--proof", value)
	}
	for _, value := range risk {
		args = append(args, "--risk", value)
	}
	return args
}

func assertRepeatedMetadata(t *testing.T, operation, block string, verification, proof, risk []string) {
	t.Helper()
	for _, want := range []struct {
		heading string
		values  []string
		sep     string
	}{
		{heading: "## Verification", values: verification, sep: "\n"},
		{heading: "## Proof", values: proof, sep: "\n\n"},
		{heading: "## Risk And Rollback", values: risk, sep: "\n\n"},
	} {
		start := strings.Index(block, want.heading+"\n\n")
		if start < 0 {
			t.Fatalf("%s managed block omitted %q: %s", operation, want.heading, block)
		}
		start += len(want.heading) + 2
		end := strings.Index(block[start:], "\n\n## ")
		if end < 0 {
			t.Fatalf("%s managed block has no section after %q: %s", operation, want.heading, block)
		}
		got := block[start : start+end]
		values := want.values
		if want.heading == "## Verification" {
			values = make([]string, len(want.values))
			for i, value := range want.values {
				values[i] = "- [x] " + value
			}
		}
		if expected := strings.Join(values, want.sep); got != expected {
			t.Errorf("%s %s values = %q, want %q", operation, want.heading, got, expected)
		}
	}
}
