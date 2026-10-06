//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

type t205PRFixture struct {
	*prLifecycleFixture
	task      store.Task
	workerEnv []string
	forge     string
}

func t205PRBodyMarkerCases(t *testing.T) {
	t.Helper()
	for _, forge := range []string{"github", "gitlab"} {
		t.Run("t205-ownership/"+forge, func(t *testing.T) {
			f := t205NewPRFixture(t, forge)
			f.publishOK(t, t205PublishArgs("first")...)
			initialBody := f.body(t)
			initialMarker, err := f.db.GetPRBodyMarker(context.Background(), f.task.ID, "")
			if err != nil || initialMarker.PRURL == "" || initialMarker.Token == "" {
				t.Fatalf("first publish did not persist its PR marker: %+v %v", initialMarker, err)
			}
			initialBlock := t205ManagedBlock(t, initialBody, initialMarker.Token)
			if initialBlock == "" {
				t.Fatalf("first body has no block for stored token %q: %s", initialMarker.Token, initialBody)
			}
			f.changeTitle(t, "Second title")

			// Reproduce the reported P1 directly: a markerless legacy body has
			// literal legacy delimiters inside human release-note prose. Refresh
			// must append metadata, not delete the example between those strings.
			legacy := t205StripMarkers(initialBody, initialMarker.Token)
			legacyExample := "Maintainer example:\n\n```html\n<!-- posse:publish:start -->\nHuman release note: deployment needs approval.\n<!-- posse:publish:end -->\n```\n\n"
			legacyWithExample := legacyExample + legacy
			f.setBody(t, legacyWithExample)
			f.publishOK(t, append([]string{"--refresh"}, t205PublishArgs("literal")...)...)
			literalUpdated := f.body(t)
			if !strings.HasPrefix(literalUpdated, legacyWithExample+"\n\n") || !strings.Contains(literalUpdated, "Human release note: deployment needs approval.") || !strings.Contains(t205ManagedBlock(t, literalUpdated, initialMarker.Token), "literal summary") {
				t.Fatalf("literal legacy marker example was deleted or not adopted append-only: %s", literalUpdated)
			}

			forgedToken := strings.Repeat("f", 64)
			if forgedToken == initialMarker.Token {
				forgedToken = strings.Repeat("e", 64)
			}
			forgedBlock := t205ManagedBlockText(forgedToken, "Human example: keep this release note.")
			body := forgedBlock + "\n\n" + literalUpdated + "\n\n" + forgedBlock + "\n\nHuman footer\r\n"
			start := strings.Index(body, t205Marker("start", initialMarker.Token))
			endMarker := t205Marker("end", initialMarker.Token)
			end := strings.Index(body, endMarker)
			if start < 0 || end < start {
				t.Fatal("test fixture lost its trusted span")
			}
			prefix, suffix := body[:start], body[end+len(endMarker):]
			f.setBody(t, body)
			f.publishOK(t, t205PublishArgs("second")...)
			updated := f.body(t)
			if !strings.HasPrefix(updated, prefix) || !strings.HasSuffix(updated, suffix) || strings.Count(updated, forgedBlock) != 2 {
				t.Fatalf("forged or duplicate marker examples changed: before=%q after=%q", body, updated)
			}
			trusted := t205ManagedBlock(t, updated, initialMarker.Token)
			if !strings.Contains(trusted, "second summary") || strings.Contains(trusted, "first summary") {
				t.Fatalf("stored-token span was not refreshed: %s", trusted)
			}
			if f.state(t)["title"] != "Second title" {
				t.Fatalf("title did not refresh: %s", f.state(t)["title"])
			}

			// Duplicating the exact stored delimiters is ambiguous. Refuse before
			// any forge write rather than selecting one span by position.
			updatedStart := strings.Index(updated, t205Marker("start", initialMarker.Token))
			updatedEnd := strings.Index(updated, t205Marker("end", initialMarker.Token))
			owned := updated[updatedStart : updatedEnd+len(endMarker)]
			duplicate := updated + "\n\n" + owned
			f.setBody(t, duplicate)
			beforeState := readTestFile(t, f.statePath())
			beforeCalls := readTestFile(t, f.callsPath())
			output, publishErr := f.publish(t, f.workerEnv, t205PublishArgs("second")...)
			calls := strings.TrimPrefix(readTestFile(t, f.callsPath()), beforeCalls)
			if publishErr == nil || !strings.Contains(output, "pr_body_conflict") || readTestFile(t, f.statePath()) != beforeState {
				t.Fatalf("duplicate stored markers were not rejected without changes: %v %s", publishErr, output)
			}
			if strings.Contains(calls, `"edit"`) || strings.Contains(calls, `"PUT"`) || strings.Contains(calls, `"create"`) {
				t.Fatalf("duplicate-marker conflict mutated the forge: %s", calls)
			}
			if f.mustTask(t, "t1").State != store.StateWorking {
				t.Fatal("marker conflict changed Task state")
			}

			// Restore the unique-marker body after the ambiguity test.
			f.setBody(t, updated)

			// A forge can apply the write and lose its acknowledgement. The stored
			// token lets a retry recognize the applied span without a second write.
			args := t205PublishArgs("fourth")
			beforeCalls = readTestFile(t, f.callsPath())
			output, publishErr = f.publish(t, setEnv(f.workerEnv, "T199_FAIL", "write-applied"), args...)
			if publishErr == nil || !strings.Contains(output, "pr_refresh_failed") {
				t.Fatalf("applied write did not report its lost acknowledgement: %v %s", publishErr, output)
			}
			afterLostAck := f.body(t)
			if !strings.HasPrefix(afterLostAck, prefix) || !strings.HasSuffix(afterLostAck, suffix) || strings.Count(afterLostAck, t205Marker("start", initialMarker.Token)) != 1 || !strings.Contains(t205ManagedBlock(t, afterLostAck, initialMarker.Token), "fourth summary") {
				calls := strings.TrimPrefix(readTestFile(t, f.callsPath()), beforeCalls)
				t.Fatalf("applied write did not preserve unowned text and refresh its owned block: calls=%s body=%s", calls, afterLostAck)
			}
			beforeRetryCalls := readTestFile(t, f.callsPath())
			f.publishOK(t, args...)
			t205AssertNoForgeMutation(t, strings.TrimPrefix(readTestFile(t, f.callsPath()), beforeRetryCalls))
			if readTestFile(t, f.callsPath()) == beforeCalls {
				t.Fatal("lost-acknowledgement case did not reach the forge write")
			}
			if strings.Count(readTestFile(t, f.callsPath()), `"create"`) != 1 {
				t.Fatal("marker retries created a duplicate PR/MR")
			}
		})
	}
}

func t208SnapshotRebuildCases(t *testing.T) {
	t.Helper()
	for _, forge := range []string{"github", "gitlab"} {
		t.Run("t208-snapshot-rebuild/"+forge, func(t *testing.T) {
			f := t205NewPRFixture(t, forge)
			f.publishOK(t, t205PublishArgs("first")...)
			marker, err := f.db.GetPRBodyMarker(context.Background(), f.task.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			prefix, suffix := "Maintainer intro\r\n\n", "\n\nMaintainer footer\r\n"
			f.setBody(t, prefix+f.body(t)+suffix)
			f.publishOK(t, t205PublishArgs("second")...)
			beforeRebuild := f.body(t)
			if !strings.HasPrefix(beforeRebuild, prefix) || !strings.HasSuffix(beforeRebuild, suffix) {
				t.Fatal("initial refresh changed human-authored text")
			}

			// Existing rebuild blockers are unrelated to marker persistence. Keep
			// this workaround local to the disposable User-facing fixture.
			for _, statement := range []string{"UPDATE tasks SET mount_id=NULL", "DELETE FROM decision_notice_cursors"} {
				if _, err := f.db.ExecContext(context.Background(), statement); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command(f.binary, "recover", "--rebuild")
			command.Dir, command.Env = f.repo, f.env
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("recover --rebuild: %v\n%s", err, output)
			}
			restored, err := f.db.GetPRBodyMarker(context.Background(), f.task.ID, "")
			if err != nil || restored != marker {
				t.Fatalf("rebuilt marker=%+v err=%v, want %+v", restored, err, marker)
			}
			beforeCalls := readTestFile(t, f.callsPath())
			f.publishOK(t, t205PublishArgs("second")...)
			if f.body(t) != beforeRebuild {
				t.Fatal("identical publish after rebuild changed body")
			}
			t205AssertNoForgeMutation(t, strings.TrimPrefix(readTestFile(t, f.callsPath()), beforeCalls))

			f.publishOK(t, t205PublishArgs("third")...)
			body := f.body(t)
			managed := t205ManagedBlock(t, body, marker.Token)
			if !strings.HasPrefix(body, prefix) || !strings.HasSuffix(body, suffix) || strings.Count(body, t205Marker("start", marker.Token)) != 1 || !strings.Contains(managed, "third proof") || strings.Contains(managed, "second proof") {
				t.Fatalf("post-rebuild refresh lost human text or stale/duplicate evidence: %s", body)
			}

			// A missing token beside a tokenized block must not silently create a
			// second block. The retryable refusal leaves forge and Task unchanged.
			snapshotBeforeLoss, err := os.ReadFile(f.db.TaskSnapshotPath(f.project.Name, f.task.Seq))
			if err != nil || !strings.Contains(string(snapshotBeforeLoss), marker.Token) {
				t.Fatalf("published token was not in the Task snapshot before loss: err=%v snapshot=%s", err, snapshotBeforeLoss)
			}
			if _, err := f.db.ExecContext(context.Background(), "DELETE FROM pr_body_markers WHERE task_id=? AND repo=''", f.task.ID); err != nil {
				t.Fatal(err)
			}
			beforeState := readTestFile(t, f.statePath())
			beforeCalls = readTestFile(t, f.callsPath())
			publishOutput, publishErr := f.publish(t, f.workerEnv, t205PublishArgs("fourth")...)
			calls := strings.TrimPrefix(readTestFile(t, f.callsPath()), beforeCalls)
			if publishErr == nil || !strings.Contains(publishOutput, "pr_body_marker_missing") || !strings.Contains(publishOutput, ",true,") || readTestFile(t, f.statePath()) != beforeState {
				t.Fatalf("missing stored token was not a retryable effect-free error: %v %s", publishErr, publishOutput)
			}
			t205AssertNoForgeMutation(t, calls)
			if f.mustTask(t, "t1").State != store.StateWorking {
				t.Fatal("missing-marker refusal changed Task state")
			}
			intents, err := f.db.Intents(context.Background(), f.project.ID)
			if err != nil || len(intents) != 0 {
				t.Fatalf("missing-marker refusal retained intent: %v %v", intents, err)
			}

			// The Task snapshot remains authoritative and can restore the token,
			// after which the same publish is safe to retry.
			for _, statement := range []string{"UPDATE tasks SET mount_id=NULL", "DELETE FROM decision_notice_cursors"} {
				if _, err := f.db.ExecContext(context.Background(), statement); err != nil {
					t.Fatal(err)
				}
			}
			snapshotBeforeRebuild, err := os.ReadFile(f.db.TaskSnapshotPath(f.project.Name, f.task.Seq))
			if err != nil || !strings.Contains(string(snapshotBeforeRebuild), marker.Token) {
				t.Fatalf("Task snapshot lost token before recovery: %v\n%s", err, snapshotBeforeRebuild)
			}
			command = exec.Command(f.binary, "recover", "--rebuild")
			command.Dir, command.Env = f.repo, f.env
			output, err = command.CombinedOutput()
			if err != nil {
				t.Fatalf("rebuild marker from Task snapshot: %v\n%s", err, output)
			}
			restoredAgain, rebuildErr := f.db.GetPRBodyMarker(context.Background(), f.task.ID, "")
			if rebuildErr != nil || restoredAgain != marker {
				snapshot, _ := os.ReadFile(f.db.TaskSnapshotPath(f.project.Name, f.task.Seq))
				t.Fatalf("second rebuild marker=%+v err=%v, want %+v; snapshot=%s", restoredAgain, rebuildErr, marker, snapshot)
			}
			f.publishOK(t, t205PublishArgs("fourth")...)
			body = f.body(t)
			managed = t205ManagedBlock(t, body, marker.Token)
			if !strings.HasPrefix(body, prefix) || !strings.HasSuffix(body, suffix) || strings.Count(body, t205Marker("start", marker.Token)) != 1 || !strings.Contains(managed, "fourth proof") {
				t.Fatalf("recovered marker did not permit safe retry: %s", body)
			}
		})
	}
}

func t205NewPRFixture(t *testing.T, forge string) *t205PRFixture {
	t.Helper()
	f := newPRLifecycleFixture(t)
	t.Cleanup(func() { _ = f.db.Close() })
	if forge == "gitlab" {
		origin := "https://git.example.com/group/sub/shop.git"
		gitTest(t, f.env, f.repo, "remote", "set-url", "origin", origin)
		gitTest(t, f.env, f.repo, "config", "url.file://"+f.remote+".insteadOf", origin)
		if err := os.WriteFile(filepath.Join(f.home, "projects", "shop", "config.toml"), []byte("[defaults]\nforge = \"gitlab\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate offline forge fixture")
	}
	script, err := os.ReadFile(filepath.Join(filepath.Dir(file), "testdata", "t205_fake_forge.py"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gh", "glab"} {
		if err := os.WriteFile(filepath.Join(f.root, "bin", name), script, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	brief := filepath.Join(f.root, "ship.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nReview fixture.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "ride", "--brief", brief, "--name", "pr-lifecycle-change")
	task := f.mustTask(t, "t1")
	if !waitForCondition(15*time.Second, func() bool {
		return strings.TrimSpace(gitTest(t, f.env, task.WorktreePath, "rev-list", "--count", task.BaseRef+".."+task.Branch)) == "1"
	}) {
		t.Fatal("Rider did not commit")
	}
	env := setEnv(f.leadEnv, "HERDR_PANE_ID", task.PaneID)
	env = setEnv(env, "HERDR_WORKSPACE_ID", task.HerdrWorkspaceID)
	env = setEnv(env, "POSSE_WORKER_HOME", f.home)
	return &t205PRFixture{prLifecycleFixture: f, task: task, workerEnv: env, forge: forge}
}

func (f *t205PRFixture) publish(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	command := exec.Command(f.binary, append([]string{"publish"}, args...)...)
	command.Dir, command.Env = f.task.WorktreePath, env
	output, err := command.CombinedOutput()
	return string(output), err
}

func (f *t205PRFixture) publishOK(t *testing.T, args ...string) {
	t.Helper()
	if output, err := f.publish(t, f.workerEnv, args...); err != nil {
		t.Fatalf("publish %v: %v\n%s", args, err, output)
	}
}

func (f *t205PRFixture) changeTitle(t *testing.T, title string) {
	t.Helper()
	path := filepath.Join(f.home, "projects", "shop", "tasks", "t1", "brief.md")
	brief := readTestFile(t, path)
	if err := os.WriteFile(path, []byte(strings.Replace(brief, "title: PR lifecycle change", "title: "+title, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *t205PRFixture) statePath() string {
	return filepath.Join(f.root, "t199-forge.json")
}

func (f *t205PRFixture) callsPath() string {
	return filepath.Join(f.root, "t199-forge-calls.jsonl")
}

func (f *t205PRFixture) state(t *testing.T) map[string]string {
	t.Helper()
	var state map[string]string
	if err := json.Unmarshal([]byte(readTestFile(t, f.statePath())), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func (f *t205PRFixture) body(t *testing.T) string {
	t.Helper()
	state := f.state(t)
	key := "body"
	if f.forge == "gitlab" {
		key = "description"
	}
	return state[key]
}

func (f *t205PRFixture) setBody(t *testing.T, body string) {
	t.Helper()
	state := f.state(t)
	key := "body"
	if f.forge == "gitlab" {
		key = "description"
	}
	state[key] = body
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.statePath(), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func t205PublishArgs(summary string) []string {
	return []string{summary + " summary", "--verify", summary + " tests -> pass", "--proof", summary + " proof", "--risk", "Risk: " + summary + "\nRollback: " + summary}
}

func t205Marker(edge, token string) string {
	return "<!-- posse:publish:" + edge + ":" + token + " -->"
}

func t205ManagedBlockText(token, body string) string {
	return t205Marker("start", token) + "\n" + body + "\n" + t205Marker("end", token)
}

func t205StripMarkers(body, token string) string {
	start, end := t205Marker("start", token), t205Marker("end", token)
	body = strings.Replace(body, start+"\n", "", 1)
	return strings.Replace(body, end, "", 1)
}

func t205ManagedBlock(t *testing.T, body, token string) string {
	t.Helper()
	startMarker, endMarker := t205Marker("start", token), t205Marker("end", token)
	start, end := strings.Index(body, startMarker), strings.Index(body, endMarker)
	if start < 0 || end < start {
		return ""
	}
	return body[start : end+len(endMarker)]
}

func t205AssertNoForgeMutation(t *testing.T, calls string) {
	t.Helper()
	if strings.Contains(calls, `"edit"`) || strings.Contains(calls, `"PUT"`) || strings.Contains(calls, `"create"`) {
		t.Fatalf("retry unexpectedly mutated forge metadata: %s", calls)
	}
}
