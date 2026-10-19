//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func TestAdmittedPlanFreezesRelaunchConfiguration(t *testing.T) {
	fixture := reviewIdentityFixture(t)
	if code, output := startReviewIdentityTask(t, fixture, "Frozen candidate", "author", ""); code != 0 {
		t.Fatalf("start: code=%d %s", code, output)
	}
	task := fixture.mustTask(t, "t1")
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.author.model", "edited-model")
	if code, output := runReviewIdentityCLI(t, fixture, "relaunch", "t1"); code != 0 {
		t.Fatalf("relaunch: code=%d %s", code, output)
	}
	identities, err := fixture.db.TaskLaunchIdentities(context.Background(), task.ID)
	if err != nil || len(identities) != 2 || identities[1].ConfiguredModel != "model-a" {
		log, _ := os.ReadFile(filepath.Join(fixture.root, "review-identity-args.log"))
		t.Fatalf("admitted settings changed after config edit: identities=%#v err=%v\nagent args:\n%s", identities, err, log)
	}
	workerEnv := setEnv(fixture.env, "HERDR_PANE_ID", task.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", fixture.home)
	output := runPosse(t, fixture.binary, task.WorktreePath, workerEnv, "status", "--json")
	if !strings.Contains(output, `"admitted"`) || !strings.Contains(output, `"model-a"`) || strings.Contains(output, "edited-model") {
		t.Fatalf("Worker status did not expose its frozen plan: %s", output)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET gated_sha='old-evidence' WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if code, output := runReviewIdentityCLI(t, fixture, "relaunch", "t1", "--profile", "author"); code != 0 {
		t.Fatalf("explicit reconfiguration: code=%d %s", code, output)
	}
	reconfigured := fixture.mustTask(t, "t1")
	plan, err := reconfigured.AdmittedPlan()
	if err != nil || plan == nil || plan.Revision != 2 || plan.Launch.ConfiguredModel != "edited-model" || reconfigured.GatedSHA != "" {
		t.Fatalf("reconfiguration did not revise admission and invalidate evidence: %#v %v", reconfigured, err)
	}
	identities, err = fixture.db.TaskLaunchIdentities(context.Background(), task.ID)
	if err != nil || len(identities) != 3 || identities[0].PlanRevision != 1 || identities[1].PlanRevision != 1 || identities[2].PlanRevision != 2 {
		t.Fatalf("launch revision binding lost: %#v %v", identities, err)
	}
	before, err := fixture.db.TaskPlanRevisions(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "recover", "--rebuild")
	after, err := fixture.db.TaskPlanRevisions(context.Background(), task.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rebuild changed frozen history: %#v %v", after, err)
	}
}

func TestAdmittedPublishUsesSelectedBriefInsteadOfMutableCopy(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	briefPath := filepath.Join(fixture.root, "change.md")
	original := "---\ntype: change\ntitle: PR lifecycle change\ndone_when: committed change exists\nticket: 12\nrefs: [18]\n---\nCreate a committed change.\n"
	if err := os.WriteFile(briefPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.runStartWithStoreBusyRetry(t, briefPath, "pr-lifecycle-change")
	stored := filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "brief.md")
	edited := strings.ReplaceAll(strings.ReplaceAll(original, "ticket: 12", "ticket: 999"), "refs: [18]", "refs: [998]")
	if err := os.WriteFile(stored, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "signal-gates", "t1"), []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.waitTaskState(t, "t1", store.StateDone)
	calls := readTestFile(t, fixture.ghLog)
	if !strings.Contains(calls, "Closes #12") || !strings.Contains(calls, "Refs #18") || strings.Contains(calls, "Closes #999") || strings.Contains(calls, "Refs #998") {
		t.Fatalf("publish followed mutable Brief instead of admitted inputs: %s", calls)
	}
}

func TestWorkerAdmittedStatusDoesNotRecoverOrExposeSibling(t *testing.T) {
	fixture := reviewIdentityFixture(t)
	if code, output := startReviewIdentityTask(t, fixture, "Own candidate", "author", ""); code != 0 {
		t.Fatalf("start: code=%d %s", code, output)
	}
	own := fixture.mustTask(t, "t1")
	ctx := context.Background()
	siblingID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 2, Type: "research", Title: "Private sibling", Profile: "sibling-profile"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.StartIntent(ctx, fixture.project.ID, siblingID, "start", "done:task.create", "{}", 999999); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.db.Intents(ctx, fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	workerEnv := setEnv(fixture.env, "HERDR_PANE_ID", own.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", fixture.home)
	// No server is reachable, and the current config is deliberately invalid.
	workerEnv = setEnv(workerEnv, "HERDR_SOCKET_PATH", filepath.Join(fixture.root, "not-running.sock"))
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte("invalid TOML ["), 0o600); err != nil {
		t.Fatal(err)
	}
	output := runPosse(t, fixture.binary, own.WorktreePath, workerEnv, "status", "--json")
	var view struct {
		Task      string        `json:"task"`
		Admission string        `json:"admission"`
		Plan      taskplan.Plan `json:"plan"`
	}
	if err := json.Unmarshal([]byte(output), &view); err != nil {
		t.Fatal(err)
	}
	if view.Task != "t1" || view.Admission != "admitted" || view.Plan.Version != taskplan.Version || strings.Contains(output, "sibling-profile") {
		t.Fatalf("status exposed sibling or lost admission: %s", output)
	}
	command := exec.Command(fixture.binary, "status", "t2")
	command.Dir, command.Env = own.WorktreePath, workerEnv
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "only its own status") {
		t.Fatalf("Worker selected sibling: %v %s", err, output)
	}
	after, err := fixture.db.Intents(ctx, fixture.project.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("status recovered or cleaned sibling: %#v %v", after, err)
	}
}
