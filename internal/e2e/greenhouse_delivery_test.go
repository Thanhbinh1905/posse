//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestGreenhouseFullPipelineDeliveryGatesRepairedHead(t *testing.T) {
	greenhouseBinary := os.Getenv("POSSE_T1202_GREENHOUSE_BIN")
	if greenhouseBinary == "" {
		t.Skip("set POSSE_T1202_GREENHOUSE_BIN to the Greenhouse main binary")
	}
	f := newWorkerTabsFixture(t)
	if err := os.WriteFile(filepath.Join(f.root, "quiet-agent"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(f.root, "origin.git")
	gitTest(t, f.env, f.repo, "remote", "set-url", "origin", "https://github.com/acme/shop.git")
	gitTest(t, f.env, f.repo, "config", "url.file://"+remote+".insteadOf", "https://github.com/acme/shop.git")
	runPosse(t, f.binary, f.repo, f.leadEnv, "config", "set", "defaults.merge_mode", "greenhouse", "--project", "shop")

	greenhouseHome := filepath.Join(f.root, "greenhouse-custom-home")
	control := filepath.Join(f.root, "greenhouse-control")
	agentHome := filepath.Join(f.root, "pi-home")
	for _, path := range []string{greenhouseHome, control, agentHome} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(agentHome, "models.json"), []byte(`{"providers":{"fixture":{"models":[{"id":"model","reasoning":true}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pi := filepath.Join(f.root, "bin", "pi")
	fakePi := fmt.Sprintf(greenhouseDeliveryFakePi, control)
	if err := os.WriteFile(pi, []byte(fakePi), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "review-mode"), []byte("findings\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(greenhouseHome, "backends.json"), map[string]any{"version": 2, "writable": map[string][]string{"reviewer": {control}, "fixer": {control}}})
	writeJSONFile(t, filepath.Join(greenhouseHome, "fixer.json"), map[string]any{"version": 2, "name": "E2E", "email": "e2e@example.invalid", "checks": [][]string{{"/usr/bin/true"}}})

	var mu sync.Mutex
	prBranch := ""
	prHead := ""
	checksGreen := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/repos/acme/shop/pulls" && r.Method == http.MethodGet:
			if prBranch == "" {
				_, _ = w.Write([]byte("[]"))
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 17, "html_url": "https://github.com/acme/shop/pull/17", "head": map[string]string{"sha": prHead}, "base": map[string]string{"ref": "main"}}})
		case r.URL.Path == "/repos/acme/shop/pulls" && r.Method == http.MethodPost:
			var request struct {
				Head string `json:"head"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode PR request: %v", err)
				return
			}
			prBranch = strings.TrimPrefix(request.Head, "acme:")
			sha, err := exec.Command("git", "--git-dir="+remote, "rev-parse", "refs/heads/"+prBranch).Output()
			if err != nil {
				t.Errorf("read pushed PR head %s: %v", prBranch, err)
				return
			}
			prHead = strings.TrimSpace(string(sha))
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 17, "html_url": "https://github.com/acme/shop/pull/17", "head": map[string]string{"sha": prHead}})
		case r.URL.Path == "/repos/acme/shop/pulls/17" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 17, "head": map[string]string{"sha": prHead}})
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			sha := strings.Split(r.URL.Path, "/")
			conclusion := "success"
			if !checksGreen {
				conclusion = "failure"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "check_runs": []map[string]string{{"name": "fixture-ci", "status": "completed", "conclusion": conclusion, "head_sha": sha[len(sha)-2]}}})
		default:
			t.Errorf("unexpected forge request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	gh := filepath.Join(f.root, "bin", "gh")
	ghWrapper := `#!/bin/sh
set -eu
case "$1 $2" in
  "pr view")
    branch=$(cat "$POSSE_TEST_ROOT/greenhouse-pr-branch")
    head=$(git --git-dir="$POSSE_TEST_REMOTE" rev-parse "refs/heads/$branch")
    state=OPEN
    if [ -f "$POSSE_TEST_ROOT/greenhouse-pr-moved" ]; then head=$(cat "$POSSE_TEST_ROOT/greenhouse-pr-moved"); fi
    conclusion=SUCCESS
    if [ -f "$POSSE_TEST_ROOT/greenhouse-checks-red" ]; then conclusion=FAILURE; fi
    printf '{"state":"%s","headRefName":"%s","headRefOid":"%s","baseRefName":"main","url":"https://github.com/acme/shop/pull/17","number":17,"headRepository":{"nameWithOwner":"acme/shop"},"baseRepository":{"nameWithOwner":"acme/shop"},"statusCheckRollup":[{"__typename":"CheckRun","name":"fixture-ci","status":"COMPLETED","conclusion":"%s"}]}\n' "$state" "$branch" "$head" "$conclusion" ;;
  "pr merge")
    printf '%s\n' "$*" > "$POSSE_TEST_ROOT/greenhouse-merge-args"
    expected=$(printf '%s\n' "$*" | sed -n 's/.*--match-head-commit \([0-9a-f]*\).*/\1/p')
    branch=$(cat "$POSSE_TEST_ROOT/greenhouse-pr-branch")
    actual=$(git --git-dir="$POSSE_TEST_REMOTE" rev-parse "refs/heads/$branch")
    [ "$expected" = "$actual" ] || { printf 'PR head moved\n' >&2; exit 1; }
    git --git-dir="$POSSE_TEST_REMOTE" update-ref refs/heads/main "$expected"
    printf 'Merged\n' ;;
  "pr list") printf '[]\n' ;;
  "auth status") exit 0 ;;
  "api graphql") printf '{"data":{}}\n' ;;
  *) printf 'unexpected fake gh command: %s\n' "$*" >&2; exit 90 ;;
esac
`
	if err := os.WriteFile(gh, []byte(ghWrapper), 0o700); err != nil {
		t.Fatal(err)
	}

	pipeline := map[string]any{
		"version": 2, "budget_unit": "rounds", "name": "E2E", "email": "e2e@example.invalid",
		"remote": remote, "github_owner": "acme", "github_repo": "shop", "pull_base": "main",
		"required_checks": []string{"fixture-ci"}, "test": [][]string{{"/usr/bin/true"}}, "lint": [][]string{{"/usr/bin/true"}},
		"not_applicable": map[string]string{"docs": "No documentation changes"},
	}
	writeJSONFile(t, filepath.Join(greenhouseHome, "pipeline.json"), pipeline)
	greenhouseEnv := setEnv(f.env, "GREENHOUSE_HOME", greenhouseHome)
	greenhouseEnv = setEnv(greenhouseEnv, "GREENHOUSE_GITHUB_API", server.URL)
	greenhouseEnv = setEnv(greenhouseEnv, "GH_TOKEN", "fixture-token")
	greenhouseEnv = setEnv(greenhouseEnv, "PI_CODING_AGENT_DIR", agentHome)
	greenhouseEnv = setEnv(greenhouseEnv, "POSSE_TEST_ROOT", f.root)
	greenhouseEnv = setEnv(greenhouseEnv, "POSSE_TEST_REMOTE", remote)
	greenhouseEnv = setEnv(greenhouseEnv, "PATH", filepath.Dir(pi)+string(os.PathListSeparator)+os.Getenv("PATH"))

	task := f.start(t, "t1", "Greenhouse full delivery", "greenhouse-full-delivery")
	change := filepath.Join(task.WorktreePath, "change.txt")
	if err := os.WriteFile(change, []byte("needs repair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, greenhouseEnv, task.WorktreePath, "add", "change.txt")
	gitTest(t, greenhouseEnv, task.WorktreePath, "commit", "-m", "add change for Greenhouse review")

	ghBin := filepath.Join(f.root, "bin", "greenhouse")
	if err := os.Symlink(greenhouseBinary, ghBin); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop := exec.Command(ghBin, "stop")
		stop.Dir, stop.Env = task.WorktreePath, greenhouseEnv
		_, _ = stop.CombinedOutput()
	}()
	attach := greenhouseAXI(t, ghBin, task.WorktreePath, greenhouseEnv, map[string]any{"version": 2, "op": "attach"})
	if attach["outcome"] != "ok" {
		t.Fatalf("attach Greenhouse: %v", attach)
	}
	settings := map[string]any{"mode": "auto", "preset": "full", "limit": 6, "rules": []string{"unslop"}, "reviewer_backend": "pi", "reviewer_model": "fixture/model", "fixer_backend": "pi", "fixer_model": "fixture/model"}
	applied := greenhouseAXI(t, ghBin, task.WorktreePath, greenhouseEnv, map[string]any{
		"version": 2, "op": "apply", "run_id": attach["run_id"], "input_id": attach["input_id"], "gate_id": attach["gate_id"], "expected": attach["token"],
		"action_id": "apply-full", "reason": "Configure deterministic full-pipeline test", "settings": settings,
	})
	if applied["outcome"] != "ok" {
		t.Fatalf("apply Greenhouse full settings: %v", applied)
	}
	command := exec.CommandContext(context.Background(), ghBin, "axi", "pipeline", "--intent", "Repair the reported test defect", "--target", "main", "--wait", "8m")
	command.Dir, command.Env = task.WorktreePath, greenhouseEnv
	pipelineOutput, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run Greenhouse pipeline: %v\n%s", err, pipelineOutput)
	}
	var pipelineResult map[string]any
	if err := json.Unmarshal(pipelineOutput, &pipelineResult); err != nil {
		t.Fatalf("decode Greenhouse pipeline result: %v\n%s", err, pipelineOutput)
	}
	if pipelineResult["outcome"] != "ok" || pipelineResult["run_id"] == "" {
		t.Fatalf("Greenhouse pipeline did not complete: %v", pipelineResult)
	}
	runID := pipelineResult["run_id"].(string)
	inspected := greenhouseAXI(t, ghBin, task.WorktreePath, greenhouseEnv, map[string]any{"version": 2, "op": "inspect", "run_id": runID})
	run := inspected["run"].(map[string]any)
	if run["status"] != "ci-ready" || run["pipeline"].(map[string]any)["outcome"] != "repaired-pass" {
		t.Fatalf("Greenhouse run did not repair, rereview and reach ci-ready: %v", run)
	}
	delivery := inspected["delivery"].(map[string]any)
	if delivery["current"] != true || delivery["pushed_head"] != run["candidate"] || delivery["pr_url"] != "https://github.com/acme/shop/pull/17" {
		t.Fatalf("Greenhouse did not confirm delivery of the repaired candidate: %v", delivery)
	}
	if got, err := os.ReadFile(filepath.Join(control, "review-invocations")); err != nil || len(strings.Fields(string(got))) != 2 {
		t.Fatalf("reviewer calls = %q, %v; want findings then rereview", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(control, "fix-invocations")); err != nil || len(strings.Fields(string(got))) != 1 {
		t.Fatalf("fixer calls = %q, %v; want one repair", got, err)
	}

	branch := task.Branch
	if err := os.WriteFile(filepath.Join(f.root, "greenhouse-pr-branch"), []byte("greenhouse/"+runID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	actualBranch, actualHead := prBranch, prHead
	mu.Unlock()
	if actualBranch != "greenhouse/"+runID || actualHead != run["candidate"] {
		t.Fatalf("Greenhouse delivered %s@%s, want its private candidate branch at %s", actualBranch, actualHead, run["candidate"])
	}

	handedBack := greenhouseAXI(t, ghBin, task.WorktreePath, greenhouseEnv, map[string]any{
		"version": 2, "op": "handback", "run_id": runID, "input_id": inspected["input_id"], "gate_id": inspected["gate_id"],
		"expected": inspected["token"], "action_id": "handback-repaired-candidate", "reason": "Return the reviewed candidate to the Task branch",
	})
	if handedBack["outcome"] != "ok" {
		t.Fatalf("Greenhouse handback: %v", handedBack)
	}
	branchHead := strings.TrimSpace(gitTest(t, greenhouseEnv, task.WorktreePath, "rev-parse", "HEAD"))
	if branchHead != actualHead {
		t.Fatalf("Greenhouse handback set Task branch to %s, want repaired head %s", branchHead, actualHead)
	}
	workerEnv := workerSignalEnv(f, task)
	workerEnv = setEnv(workerEnv, "GREENHOUSE_HOME", greenhouseHome)
	workerEnv = setEnv(workerEnv, "POSSE_TEST_ROOT", f.root)
	workerEnv = setEnv(workerEnv, "POSSE_TEST_REMOTE", remote)
	workerEnv = setEnv(workerEnv, "PATH", filepath.Dir(pi)+string(os.PathListSeparator)+os.Getenv("PATH"))
	mergeEnv := setEnv(f.leadEnv, "POSSE_TEST_ROOT", f.root)
	mergeEnv = setEnv(mergeEnv, "POSSE_TEST_REMOTE", remote)
	mergeEnv = setEnv(mergeEnv, "PATH", filepath.Dir(pi)+string(os.PathListSeparator)+os.Getenv("PATH"))
	gitTest(t, greenhouseEnv, task.WorktreePath, "push", "origin", branch)
	if pushed := strings.TrimSpace(gitTest(t, f.env, remote, "--git-dir", remote, "rev-parse", "refs/heads/"+branch)); pushed != actualHead {
		t.Fatalf("published Task branch head=%s, want repaired candidate %s", pushed, actualHead)
	}

	turn := greenhouseTaskTurn(t, f, task.ID)
	if output, err := runBinaryAllowFailure(f.binary, task.WorktreePath, workerEnv, "signal", "done", "Greenhouse run passed", "--greenhouse-run", "forged-run-id", "--turn", turn); err == nil {
		t.Fatalf("Posse accepted a forged Greenhouse run ID: %s", output)
	}
	if err := os.WriteFile(filepath.Join(f.root, "greenhouse-pr-moved"), []byte(strings.Repeat("f", 40)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	turn = greenhouseTaskTurn(t, f, task.ID)
	if output, err := runBinaryAllowFailure(f.binary, task.WorktreePath, workerEnv, "signal", "done", "Greenhouse run passed", "--greenhouse-run", runID, "--turn", turn); err == nil || !strings.Contains(output, "PR is not open with checks at gated head") {
		t.Fatalf("Posse accepted a PR with a forged head: err=%v output=%s", err, output)
	}
	_ = os.Remove(filepath.Join(f.root, "greenhouse-pr-moved"))
	turn = greenhouseTaskTurn(t, f, task.ID)
	if output, err := runBinaryAllowFailure(f.binary, task.WorktreePath, workerEnv, "signal", "done", "Greenhouse run passed", "--greenhouse-run", runID, "--turn", turn); err != nil {
		t.Fatalf("Posse rejected the real Greenhouse run at repaired head %s: %v", actualHead, output)
	}
	if done := f.task(t, "t1"); done.State != store.StateDone || done.GatedSHA != actualHead || done.PRURL != "https://github.com/acme/shop/pull/17" {
		t.Fatalf("verified done Signal did not gate the exact Greenhouse delivery: %+v", done)
	}

	if err := os.WriteFile(filepath.Join(f.root, "greenhouse-pr-moved"), []byte(strings.Repeat("f", 40)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := runBinaryAllowFailure(f.binary, f.repo, mergeEnv, "merge", "t1", "--execute", "--user-approved", "Merge the reviewed and green PR"); err == nil {
		t.Fatalf("Posse allowed merge after the PR head moved: %s", output)
	}
	_ = os.Remove(filepath.Join(f.root, "greenhouse-pr-moved"))
	if err := os.WriteFile(filepath.Join(f.root, "greenhouse-checks-red"), []byte("red\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := runBinaryAllowFailure(f.binary, f.repo, mergeEnv, "merge", "t1", "--execute", "--user-approved", "Merge the reviewed and green PR"); err == nil {
		t.Fatalf("Posse allowed merge with failing PR checks: %s", output)
	}
	_ = os.Remove(filepath.Join(f.root, "greenhouse-checks-red"))
	if output := runPosse(t, f.binary, f.repo, mergeEnv, "merge", "t1", "--execute", "--user-approved", "Merge the reviewed and green PR"); !strings.Contains(output, "merged") && !strings.Contains(output, "Merged") {
		t.Fatalf("exact green head did not pass merge gating: %s", output)
	}
	if merged := gitTest(t, f.env, remote, "--git-dir", remote, "rev-parse", "refs/heads/main"); strings.TrimSpace(merged) != actualHead {
		t.Fatalf("bare remote main=%s after merge, want gated head %s", strings.TrimSpace(merged), actualHead)
	}
}

func greenhouseTaskTurn(t *testing.T, f *workerTabsFixture, taskID int64) string {
	t.Helper()
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := db.TaskTurnToken(context.Background(), taskID)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

func greenhouseAXI(t *testing.T, binary, dir string, env []string, request map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "axi")
	command.Dir, command.Env, command.Stdin = dir, env, strings.NewReader(string(body))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("greenhouse axi: %v\n%s", err, output)
	}
	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("decode greenhouse axi: %v\n%s", err, output)
	}
	return response
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	contents, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

const greenhouseDeliveryFakePi = `#!/usr/bin/env python3
import json, os, sys
control = %q
if sys.argv[1:] == ['--list-models']:
    print('provider model context\nfixture model 128K')
    sys.exit(0)
request = json.load(sys.stdin)
if 'Selected' in request:
    with open(os.path.join(control, 'fix-invocations'), 'a') as out:
        out.write(request.get('Stage', 'review') + '\n')
    worktree = os.getcwd()
    for path in request['Paths']:
        with open(os.path.join(worktree, path), 'w') as out:
            out.write('repaired\n')
    with open(os.path.join(control, 'review-mode'), 'w') as out:
        out.write('clean')
    responses = [{'finding': finding['id'], 'response': 'fixed', 'reason': 'Repaired the reported defect'} for finding in request['Selected']]
    answer = {'responses': responses}
else:
    with open(os.path.join(control, 'review-invocations'), 'a') as out:
        out.write('review\n')
    mode = open(os.path.join(control, 'review-mode')).read().strip()
    answer = {'head': request['Head'], 'verdict': 'clean', 'coverage': request['Paths'], 'findings': [], 'documentation': {'status': 'not-needed', 'reason': 'No public documentation changed'}}
    if mode == 'findings':
        path = request['Paths'][0]
        answer['verdict'] = 'findings'
        answer['findings'] = [{'id': 'fixture-p1', 'priority': 'P1', 'origin': 'correctness', 'file': path, 'start': 1, 'end': 1, 'explanation': 'The fixture contains the reported defect', 'action': 'Repair the first line', 'rule': '', 'head': request['Head']}]
    if request.get('Responses'):
        answer['resolutions'] = [{'finding': response['finding'], 'check': 'confirmed', 'reason': 'The repaired candidate no longer has the finding'} for response in request['Responses']]
provider, model = sys.argv[sys.argv.index('--model') + 1].split('/', 1)
print(json.dumps({'type': 'message_end', 'message': {'role': 'assistant', 'provider': provider, 'model': model, 'stopReason': 'stop', 'content': [{'type': 'text', 'text': json.dumps(answer)}]}}))
print(json.dumps({'type': 'agent_settled', 'aborted': False}))
`

func runBinaryAllowFailure(binary, cwd string, env []string, args ...string) (string, error) {
	command := exec.Command(binary, args...)
	command.Dir, command.Env = cwd, taskAuthorGenerationEnv(env)
	output, err := command.CombinedOutput()
	return string(output), err
}
