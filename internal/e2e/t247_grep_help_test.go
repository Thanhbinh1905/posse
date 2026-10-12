//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"
)

func TestT247SuccessfulGrepOfPiErrorResultIsNotRetried(t *testing.T) {
	// grep -A1 returns the standalone error line and the next /bug help line.
	// The fixture appends that help line, just as the captured log contains it.
	f := newPiModelErrorFixture(t, "────────────────\n$ grep -A1 stream-error rider.log\n"+t242StreamError, false)
	f.emitError(t)
	time.Sleep(1200 * time.Millisecond)
	task := f.base.mustTask(t, "t1")
	episode, err := f.base.db.TaskModelErrorEpisode(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if episode.Status != "" || countPromptLines(f.prompts, "continue") != 0 {
		t.Fatalf("successful grep of a captured Pi error result was classified or retried: episode=%#v continues=%d", episode, countPromptLines(f.prompts, "continue"))
	}
}
