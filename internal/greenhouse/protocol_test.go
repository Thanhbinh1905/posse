package greenhouse

import (
	"encoding/json"
	"testing"
)

func TestHostResultCarriesUsageAndBreakdown(t *testing.T) {
	var result HostResult
	body := `{"usage":{"input_tokens":11,"output_tokens":5,"cached_input_tokens":2,"cache_write_tokens":1,"tokens":19,"cost_usd":0.004,"cost_source":"provider","model_calls":3,"wall_ms":1200},"usage_breakdown":[{"stage":"review","backend":"api","model":"model-x","usage":{"input_tokens":11,"output_tokens":5,"cache_write_tokens":1}}]}`
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	if result.Usage == nil || result.Usage.InputTokens != 11 || result.Usage.CostUSD == nil || *result.Usage.CostUSD != 0.004 || len(result.UsageBreakdown) != 1 || result.UsageBreakdown[0].Stage != "review" {
		t.Fatalf("usage result = %#v", result)
	}
}

func TestReviewResultMergesUsageFromRunSnapshot(t *testing.T) {
	var result response
	if err := json.Unmarshal([]byte(`{"host_result":{"grant":"grant"},"run":{"review":{"usage":{"input_tokens":7,"output_tokens":3,"cost_usd":0.002},"usage_breakdown":[{"stage":"review","usage":{"input_tokens":7}}]}}}`), &result); err != nil {
		t.Fatal(err)
	}
	got := result.reviewResult()
	if got == nil || got.Usage == nil || got.Usage.InputTokens != 7 || len(got.UsageBreakdown) != 1 {
		t.Fatalf("review usage = %#v", got)
	}
}

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
