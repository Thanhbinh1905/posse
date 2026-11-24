package greenhouse

import (
	"encoding/json"
	"testing"
)

func TestReviewResultMergesCoverageFromRunSnapshot(t *testing.T) {
	var result response
	if err := json.Unmarshal([]byte(`{"host_result":{"grant":"shop/t1/shop/r1","verdict":"partial"},"run":{"review":{"uncovered":[{"path":"large.sql","reason":"prompt exceeds model context (90000 tokens > 64000)"}],"generated":[{"path":"schema.sql","added":1200,"removed":30,"generators":["tools/schema.go"]}]}}}`), &result); err != nil {
		t.Fatal(err)
	}
	hostResult := result.reviewResult()
	if len(hostResult.Uncovered) != 1 || hostResult.Uncovered[0].Path != "large.sql" || len(hostResult.Generated) != 1 || hostResult.Generated[0].Path != "schema.sql" {
		t.Fatalf("run review coverage was not merged: %#v", hostResult)
	}
}

func TestHostResultCarriesPartialCoverageProvenance(t *testing.T) {
	var result HostResult
	if err := json.Unmarshal([]byte(`{"status":"partial-review","verdict":"partial","uncovered":[{"path":"large.sql","reason":"hunk windows exceed 65536 bytes"}],"generated":[{"path":"schema.sql","added":1200,"removed":30,"generators":["tools/schema.go"]}]}`), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "partial-review" || result.Verdict != "partial" {
		t.Fatalf("status/verdict = %q/%q", result.Status, result.Verdict)
	}
	if len(result.Uncovered) != 1 || result.Uncovered[0].Path != "large.sql" || result.Uncovered[0].Reason != "hunk windows exceed 65536 bytes" {
		t.Fatalf("uncovered = %#v", result.Uncovered)
	}
	if len(result.Generated) != 1 || result.Generated[0].Path != "schema.sql" || result.Generated[0].Added != 1200 || result.Generated[0].Removed != 30 || len(result.Generated[0].Generators) != 1 || result.Generated[0].Generators[0] != "tools/schema.go" {
		t.Fatalf("generated = %#v", result.Generated)
	}
}
