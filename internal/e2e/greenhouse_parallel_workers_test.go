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
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestGreenhousePipelinesRunConcurrentlyForTwoPosseWorkers(t *testing.T) {
	greenhouseBinary := os.Getenv("POSSE_T1202_GREENHOUSE_BIN")
	if greenhouseBinary == "" {
		t.Skip("set POSSE_T1202_GREENHOUSE_BIN to the Greenhouse main binary")
	}
	f := newWorkerTabsFixtureWithOptions(t, workerTabsOptions{manualWatchOwner: true, greenhouseHome: true})
	greenhouseHome := filepath.Join(f.root, "greenhouse")
	f.leadEnv = setEnv(f.leadEnv, "POSSE_TEST_ROOT", f.root)
	remote := filepath.Join(f.root, "origin.git")
	gitTest(t, f.env, f.repo, "remote", "set-url", "origin", "https://github.com/acme/shop.git")
	gitTest(t, f.env, f.repo, "config", "url.file://"+remote+".insteadOf", "https://github.com/acme/shop.git")
	runPosse(t, f.binary, f.repo, f.leadEnv, "config", "set", "defaults.merge_mode", "greenhouse", "--project", "shop")

	control := filepath.Join(f.root, "greenhouse-control")
	agentHome := filepath.Join(f.root, "pi-home")
	for _, path := range []string{greenhouseHome, control, agentHome} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(control, "review-mode"), []byte("clean\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "hold-reviews"), []byte("hold\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pi := filepath.Join(f.root, "bin", "pi")
	if err := os.WriteFile(pi, []byte(fmt.Sprintf(greenhouseDeliveryFakePi, control)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentHome, "models.json"), []byte(`{"providers":{"fixture":{"models":[{"id":"model","reasoning":true}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(greenhouseHome, "backends.json"), map[string]any{"version": 2, "writable": map[string][]string{"reviewer": {control}, "fixer": {control}}})
	writeJSONFile(t, filepath.Join(greenhouseHome, "fixer.json"), map[string]any{"version": 2, "name": "E2E", "email": "e2e@example.invalid", "checks": [][]string{{"/usr/bin/true"}}})
	writeJSONFile(t, filepath.Join(greenhouseHome, "pipeline.json"), map[string]any{
		"version": 2, "budget_unit": "rounds", "name": "E2E", "email": "e2e@example.invalid",
		"remote": remote, "github_owner": "acme", "github_repo": "shop", "pull_base": "main",
		"required_checks": []string{"fixture-ci"}, "test": [][]string{{"/usr/bin/true"}}, "lint": [][]string{{"/usr/bin/true"}},
		"not_applicable": map[string]string{"docs": "No documentation changes"},
	})
	ghScript := `#!/bin/sh
case "$1 $2" in
  "auth status") exit 0 ;;
  "pr list") printf '[]\\n' ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(f.root, "bin", "gh"), []byte(ghScript), 0o700); err != nil {
		t.Fatal(err)
	}

	type pull struct {
		number int
		branch string
		head   string
	}
	var mu sync.Mutex
	pulls := map[int]pull{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/repos/acme/shop/pulls" && r.Method == http.MethodGet:
			list := make([]map[string]any, 0, len(pulls))
			for _, item := range pulls {
				list = append(list, map[string]any{"number": item.number, "html_url": fmt.Sprintf("https://github.com/acme/shop/pull/%d", item.number), "head": map[string]string{"sha": item.head}, "base": map[string]string{"ref": "main"}})
			}
			_ = json.NewEncoder(w).Encode(list)
		case r.URL.Path == "/repos/acme/shop/pulls" && r.Method == http.MethodPost:
			var request struct {
				Head string `json:"head"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode PR request: %v", err)
				return
			}
			branch := strings.TrimPrefix(request.Head, "acme:")
			head, err := exec.Command("git", "--git-dir="+remote, "rev-parse", "refs/heads/"+branch).Output()
			if err != nil {
				t.Errorf("read pushed PR head %s: %v", branch, err)
				return
			}
			number := 40 + len(pulls)
			item := pull{number: number, branch: branch, head: strings.TrimSpace(string(head))}
			pulls[number] = item
			_ = json.NewEncoder(w).Encode(map[string]any{"number": number, "html_url": fmt.Sprintf("https://github.com/acme/shop/pull/%d", number), "head": map[string]string{"sha": item.head}})
		case strings.HasPrefix(r.URL.Path, "/repos/acme/shop/pulls/") && r.Method == http.MethodPatch:
			var number int
			if _, err := fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/repos/acme/shop/pulls/"), "%d", &number); err != nil {
				t.Errorf("decode pull request path: %v", err)
				return
			}
			item, ok := pulls[number]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": number, "html_url": fmt.Sprintf("https://github.com/acme/shop/pull/%d", number), "head": map[string]string{"sha": item.head}})
		case strings.HasPrefix(r.URL.Path, "/repos/acme/shop/pulls/") && r.Method == http.MethodGet:
			var number int
			if _, err := fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/repos/acme/shop/pulls/"), "%d", &number); err != nil {
				t.Errorf("decode pull request path: %v", err)
				return
			}
			item, ok := pulls[number]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": number, "head": map[string]string{"sha": item.head}})
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			parts := strings.Split(r.URL.Path, "/")
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "check_runs": []map[string]string{{"name": "fixture-ci", "status": "completed", "conclusion": "success", "head_sha": parts[len(parts)-2]}}})
		default:
			t.Errorf("unexpected forge request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ghBin := filepath.Join(f.root, "bin", "greenhouse")
	greenhousePath, err := filepath.Abs(greenhouseBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(greenhousePath, ghBin); err != nil {
		t.Fatal(err)
	}
	greenhouseEnv := setEnv(f.leadEnv, "GREENHOUSE_GITHUB_API", server.URL)
	greenhouseEnv = setEnv(greenhouseEnv, "GH_TOKEN", "fixture-token")
	greenhouseEnv = setEnv(greenhouseEnv, "PI_CODING_AGENT_DIR", agentHome)
	greenhouseEnv = setEnv(greenhouseEnv, "POSSE_TEST_REMOTE", remote)
	greenhouseEnv = setEnv(greenhouseEnv, "PATH", filepath.Join(f.root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	daemonStartedAt := startGreenhouseDaemon(t, ghBin, greenhouseHome, greenhouseEnv)
	settings := map[string]any{"mode": "auto", "preset": "full", "limit": 6, "rules": []string{"unslop"}, "reviewer_backend": "pi", "reviewer_model": "fixture/model", "fixer_backend": "pi", "fixer_model": "fixture/model"}
	type worker struct {
		task         store.Task
		runID        string
		runStartedAt int64
		dir          string
		cmd          *exec.Cmd
		output       strings.Builder
	}
	workers := make([]worker, 2)
	for index := range workers {
		id := fmt.Sprintf("t%d", index+1)
		title := fmt.Sprintf("parallel Greenhouse Worker %d", index+1)
		workers[index].task = f.start(t, id, title, fmt.Sprintf("greenhouse-worker-%d", index+1))
		workers[index].dir = workers[index].task.WorktreePath
		change := filepath.Join(workers[index].dir, fmt.Sprintf("worker-%d.txt", index+1))
		if err := os.WriteFile(change, []byte(fmt.Sprintf("worker %d\n", index+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, greenhouseEnv, workers[index].dir, "add", filepath.Base(change))
		gitTest(t, greenhouseEnv, workers[index].dir, "commit", "-m", fmt.Sprintf("Worker %d change", index+1))
		attached := greenhouseAXI(t, ghBin, workers[index].dir, greenhouseEnv, map[string]any{"version": 2, "op": "attach"})
		attachedRun := attached["run"].(map[string]any)
		runStartedAt, err := time.Parse(time.RFC3339, attachedRun["started_at"].(string))
		if err != nil {
			t.Fatalf("Greenhouse attach for Worker %d omitted run.started_at: %v, response=%v", index+1, err, attached)
		}
		workers[index].runStartedAt = runStartedAt.UnixMilli()
		if attached["outcome"] != "ok" {
			t.Fatalf("attach Greenhouse for %s: %v", id, attached)
		}
		applied := greenhouseAXI(t, ghBin, workers[index].dir, greenhouseEnv, map[string]any{
			"version": 2, "op": "apply", "run_id": attached["run_id"], "input_id": attached["input_id"], "gate_id": attached["gate_id"], "expected": attached["token"],
			"action_id": "apply-full-" + id, "reason": "Configure parallel full-pipeline test", "settings": settings,
		})
		if applied["outcome"] != "ok" {
			t.Fatalf("apply Greenhouse settings for %s: %v", id, applied)
		}
	}
	defer func() {
		_ = os.Remove(filepath.Join(control, "hold-reviews"))
		for index := range workers {
			if workers[index].cmd != nil && workers[index].cmd.Process != nil && workers[index].cmd.ProcessState == nil {
				_ = workers[index].cmd.Wait()
			}
		}
		for _, item := range workers {
			stop := exec.Command(ghBin, "stop")
			stop.Dir, stop.Env = item.dir, greenhouseEnv
			_, _ = stop.CombinedOutput()
		}
		find := func() ([]isolatedHerdrServerProcess, error) {
			processes, err := fixtureSpawnedProcesses(f.root)
			var daemons []isolatedHerdrServerProcess
			for _, process := range processes {
				if process.PID > 1 && strings.Contains(process.Args, "greenhouse daemon") {
					daemons = append(daemons, process)
				}
			}
			return daemons, err
		}
		if err := stopProcesses("Greenhouse daemon", find, 3*time.Second); err != nil {
			t.Errorf("stop parallel fixture Greenhouse daemon: %v", err)
		}
	}()

	for index := range workers {
		workers[index].cmd = exec.CommandContext(context.Background(), ghBin, "axi", "pipeline", "--intent", fmt.Sprintf("Review Worker %d change", index+1), "--target", "main", "--wait", "8m")
		workers[index].cmd.Dir = workers[index].dir
		workers[index].cmd.Env = greenhouseEnv
		workers[index].cmd.Stdout = &workers[index].output
		workers[index].cmd.Stderr = &workers[index].output
		if err := workers[index].cmd.Start(); err != nil {
			t.Fatalf("start Greenhouse pipeline for Worker %d: %v", index+1, err)
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		lines, _ := os.ReadFile(filepath.Join(control, "review-invocations"))
		if len(strings.Fields(string(lines))) >= 2 {
			break
		}
		for index := range workers {
			if workers[index].cmd.ProcessState != nil {
				t.Fatalf("Worker %d pipeline exited before concurrent review began: %s", index+1, workers[index].output.String())
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	lines, err := os.ReadFile(filepath.Join(control, "review-worktrees"))
	if err != nil || len(strings.Fields(string(lines))) < 2 {
		t.Fatalf("both Worker pipelines did not reach the held reviewer concurrently: worktrees=%q error=%v", lines, err)
	}

	watchEnv := setEnv(greenhouseEnv, "POSSE_WATCH_TOKEN", "")
	if output, err := runBinaryAllowFailure(f.binary, f.repo, watchEnv, "watch", "--poll-only", "--timeout", "1000"); err != nil {
		t.Fatalf("observe concurrent Greenhouse runs: %v\n%s", err, output)
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	for index := range workers {
		workers[index].task, err = db.TaskByID(context.Background(), f.projectID, workers[index].task.ID)
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		watch, err := db.GreenhouseTaskWatch(context.Background(), workers[index].task.ID)
		if err != nil || watch.RunID == "" || watch.Status != "running" || watch.Stage == "" {
			db.Close()
			t.Fatalf("Worker %d Greenhouse state was not isolated and persisted: %+v, %v", index+1, watch, err)
		}
		workers[index].runID = watch.RunID
		inspectRequest, _ := json.Marshal(map[string]any{"version": 2, "op": "inspect", "run_id": watch.RunID})
		inspectCmd := exec.CommandContext(context.Background(), "greenhouse", "axi")
		inspectCmd.Dir = workers[index].dir
		inspectCmd.Env = greenhouseEnv
		inspectCmd.Stdin = strings.NewReader(string(inspectRequest))
		inspectOutput, inspectErr := inspectCmd.Output()
		if inspectErr != nil {
			db.Close()
			t.Fatalf("inspect Worker %d Greenhouse run: %v", index+1, inspectErr)
		}
		var inspected struct {
			Run struct {
				StartedAt string `json:"started_at"`
				Pipeline  struct {
					Stages []struct {
						Name       string `json:"name"`
						StartedAt  string `json:"started_at"`
						FinishedAt string `json:"finished_at"`
					} `json:"stages"`
				} `json:"pipeline"`
			} `json:"run"`
		}
		if err := json.Unmarshal(inspectOutput, &inspected); err != nil {
			db.Close()
			t.Fatalf("decode Worker %d Greenhouse inspect: %v", index+1, err)
		}
		stageFound, hasStageTimestamp := false, false
		stageStartedAt := ""
		for _, stage := range inspected.Run.Pipeline.Stages {
			if stage.Name != watch.Stage {
				continue
			}
			stageFound = true
			if stage.StartedAt != "" {
				stageStartedAt = stage.StartedAt
				started, parseErr := time.Parse(time.RFC3339, stage.StartedAt)
				if parseErr != nil {
					db.Close()
					t.Fatalf("Worker %d stage has invalid started_at %q: %v", index+1, stage.StartedAt, parseErr)
				}
				if watch.StageStartedAt > 0 && watch.StageStartedAt != started.UnixMilli() {
					db.Close()
					t.Fatalf("Worker %d persisted stage timestamp %d differs from inspect %q", index+1, watch.StageStartedAt, stage.StartedAt)
				}
				hasStageTimestamp = true
			}
			if stage.FinishedAt != "" {
				db.Close()
				t.Fatalf("Worker %d active stage unexpectedly has finished_at: %+v", index+1, stage)
			}
		}
		if watch.RunStartedAt != workers[index].runStartedAt {
			db.Close()
			t.Fatalf("Worker %d persisted run timestamp %d, want attach run.started_at %d", index+1, watch.RunStartedAt, workers[index].runStartedAt)
		}
		if inspected.Run.StartedAt != "" {
			runStarted, parseErr := time.Parse(time.RFC3339, inspected.Run.StartedAt)
			if parseErr != nil || runStarted.UnixMilli() != watch.RunStartedAt {
				db.Close()
				t.Fatalf("Worker %d run timestamp mismatch: inspect=%q persisted=%+v parse=%v", index+1, inspected.Run.StartedAt, watch, parseErr)
			}
		}
		if !stageFound {
			db.Close()
			t.Fatalf("Worker %d inspect omitted active stage %s: %s", index+1, watch.Stage, inspectOutput)
		}
		if !hasStageTimestamp && watch.StageObservedAt == 0 {
			db.Close()
			t.Fatalf("Worker %d missing stage timestamp has no observation-time fallback: %+v", index+1, watch)
		}
		t.Logf("Greenhouse timing evidence Worker %d: daemon_started_at=%s run_started_at=%s stage=%s stage_started_at=%q", index+1, daemonStartedAt.Format(time.RFC3339), time.UnixMilli(watch.RunStartedAt).UTC().Format(time.RFC3339), watch.Stage, stageStartedAt)
	}
	db.Close()
	if workers[0].runID == workers[1].runID {
		t.Fatalf("both Posse Tasks followed the same Greenhouse run %s", workers[0].runID)
	}
	for index := range workers {
		statusEnv := workerSignalEnv(f, workers[index].task)
		statusEnv = setEnv(statusEnv, "PATH", filepath.Join(f.root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
		status := runPosse(t, f.binary, workers[index].dir, statusEnv, "status")
		if !strings.Contains(status, "greenhouse:") {
			t.Fatalf("posse status omitted Worker %d's Greenhouse status line for run %s: %s", index+1, workers[index].runID, status)
		}
		workspaceID := workers[index].task.HerdrWorkspaceID
		snapshot, err := f.client.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, pane := range snapshot.Panes {
			if pane.WorkspaceID == workspaceID && strings.HasPrefix(pane.Label, "greenhouse:") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("Worker %d sub-workspace %s has no Greenhouse status token: %+v", index+1, workspaceID, snapshot.Panes)
		}
	}
	list := runPosse(t, f.binary, f.repo, greenhouseEnv, "--json", "list", "--full")
	var listed struct {
		Tasks []struct {
			ID         string `json:"id"`
			Greenhouse string `json:"greenhouse"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(list), &listed); err != nil {
		t.Fatalf("decode posse list: %v\n%s", err, list)
	}
	for index := range workers {
		id := fmt.Sprintf("t%d", index+1)
		found := false
		for _, task := range listed.Tasks {
			if task.ID == id && task.Greenhouse != "" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("posse list omitted Worker %d Greenhouse status: %s", index+1, list)
		}
	}

	if err := os.Remove(filepath.Join(control, "hold-reviews")); err != nil {
		t.Fatal(err)
	}
	for index := range workers {
		if err := workers[index].cmd.Wait(); err != nil {
			t.Fatalf("Worker %d Greenhouse pipeline was refused or failed: %v\n%s", index+1, err, workers[index].output.String())
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(workers[index].output.String()), &result); err != nil {
			t.Fatalf("decode Worker %d pipeline: %v\n%s", index+1, err, workers[index].output.String())
		}
		if result["outcome"] != "ok" || result["run_id"] != workers[index].runID {
			t.Fatalf("Worker %d pipeline result does not match its observed run: %+v", index+1, result)
		}
	}
}
