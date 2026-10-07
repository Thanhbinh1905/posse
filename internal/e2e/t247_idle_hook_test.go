//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	posseRuntime "github.com/thanhbinh1905/posse/internal/runtime"
)

// Keep the real Herdr/CLI path, but let normal observation run between an idle
// snapshot and its end-of-turn hook. No runtime or database rows are patched.
func TestT247IdleObservationBeforeEndHookDoesNotStrand(t *testing.T) {
	f := newPiModelErrorFixture(t, t242StreamError, true)
	task := f.base.mustTask(t, "t1")
	for _, marker := range []string{"clear-model-screen", "pause-retry-idle-hook"} {
		if err := os.WriteFile(filepath.Join(f.base.root, marker), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.emitError(t)
	if !waitForCondition(4*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(f.base.root, "retry-idle-ready"))
		return err == nil
	}) {
		t.Fatal("retry did not reach its idle snapshot")
	}
	show := runPosse(t, f.base.binary, f.base.repo, f.base.leadEnv, "show", "t1", "--full")
	current := f.base.mustTask(t, "t1")
	episode, err := f.base.db.TaskModelErrorEpisode(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("before second idle hook: IdleSince=%d prompts=%d episode=%#v", current.IdleSince, countPromptLines(f.prompts, "continue"), episode)
	if current.IdleSince == 0 || episode.TurnState != 0 || episode.NextAttemptAt != 0 || episode.Attempts != 1 {
		t.Fatalf("probe did not establish an observed idle resumed turn: task=%#v episode=%#v show=%s", current, episode, show)
	}
	if err := os.WriteFile(filepath.Join(f.base.root, "release-retry-idle-hook"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	// Restart the hook in a fresh process, then exercise normal CLI recovery.
	event := fmt.Sprintf(`{"event":"pane.agent_status_changed","data":{"pane_id":%q,"agent_status":"idle"}}`, task.PaneID)
	runPosse(t, f.base.binary, f.base.repo, setEnv(f.base.env, "HERDR_PLUGIN_EVENT_JSON", event), "_ingest")
	runPosse(t, f.base.binary, f.base.repo, f.base.leadEnv, "recover", "--all")
	show = runPosse(t, f.base.binary, f.base.repo, f.base.leadEnv, "show", "t1", "--full")
	snapshot, err := f.client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := posseRuntime.ReconcileSnapshot(context.Background(), f.base.db, f.base.project.ID, snapshot, time.Now().Add(time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	episode, err = f.base.db.TaskModelErrorEpisode(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after fresh ingest/recover/show and advanced idle deadline: prompts=%d model_notice=%t idle_notice=%t episode=%#v", countPromptLines(f.prompts, "continue"), f.hasNotice("model_stream_error"), f.hasNotice("worker_idle"), episode)
	if countPromptLines(f.prompts, "continue") == 1 && !f.hasNotice("model_stream_error") && !f.hasNotice("worker_idle") {
		t.Fatalf("new failed turn silently stranded by earlier idle observation: episode=%#v show=%s", episode, show)
	}
}
