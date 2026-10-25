package app

import (
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestAggregateProfileStats(t *testing.T) {
	tasks := []statsTask{
		{profile: "deep", state: store.StateMerged, landed: true, doneSecs: 600, tokens: 1000, tokensSeen: true, relaunches: 1},
		{profile: "deep", state: store.StateMerged, landed: true, doneSecs: 1200, tokens: 3000, tokensSeen: true},
		{profile: "deep", state: store.StateMerged, landed: true, doneSecs: -1},
		{profile: "deep", state: store.StateFailed, doneSecs: -1, relaunches: 2},
		{profile: "deep", state: store.StateWorking, doneSecs: -1},
		{profile: "fast", state: store.StateWorking, doneSecs: -1},
	}
	got := aggregateProfileStats(tasks)
	if len(got) != 2 || got[0].Profile != "deep" || got[1].Profile != "fast" {
		t.Fatalf("profiles not grouped and ordered: %#v", got)
	}
	deep := got[0]
	if deep.Tasks != 5 || deep.Finished != 4 || deep.Landed != 3 || deep.LandedRate != 0.75 || deep.Relaunches != 3 {
		t.Fatalf("deep counts: %#v", deep)
	}
	if deep.MedianDoneSeconds == nil || *deep.MedianDoneSeconds != 900 {
		t.Fatalf("median to done: %v", deep.MedianDoneSeconds)
	}
	if deep.TokensPerLanded == nil || *deep.TokensPerLanded != 2000 {
		t.Fatalf("tokens per landed averages Tasks with known usage: %v", deep.TokensPerLanded)
	}
	fast := got[1]
	if fast.Finished != 0 || fast.LandedRate != 0 || fast.MedianDoneSeconds != nil || fast.TokensPerLanded != nil {
		t.Fatalf("in-flight only Profile must have no rate, median or tokens: %#v", fast)
	}
}

func TestStatsSummaryHandlesMissingData(t *testing.T) {
	if got := statsSummary(ProfileStats{Profile: "new"}); !strings.Contains(got, "no Tasks") {
		t.Fatalf("empty summary: %s", got)
	}
	if got := statsSummary(ProfileStats{Profile: "p", Tasks: 1}); !strings.Contains(got, "landed n/a") || !strings.Contains(got, "median to done n/a") || !strings.Contains(got, "tokens per landed n/a") {
		t.Fatalf("unknown values must read n/a: %s", got)
	}
	secs, tokens := int64(90), int64(1500)
	got := statsSummary(ProfileStats{Profile: "p", Tasks: 4, Finished: 4, Landed: 3, LandedRate: 0.75, MedianDoneSeconds: &secs, TokensPerLanded: &tokens, Relaunches: 2})
	for _, want := range []string{"landed 75% (3/4)", "median to done 1m30s", "tokens per landed 1500", "relaunches 2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary omits %q: %s", want, got)
		}
	}
}
