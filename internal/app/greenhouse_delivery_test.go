package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestGreenhouseDeliveryBindsTheRepairedCandidateAndChecks(t *testing.T) {
	submitted := strings.Repeat("b", 40)
	head := strings.Repeat("a", 40)
	base := fmt.Sprintf(`{"outcome":"ok","run_id":"run-1","run":{"id":"run-1","submitted":{"branch":"posse/t1","head":"%s"},"candidate":"%s","status":"ci-ready","pipeline":{"status":"ci-ready","ref":"refs/heads/greenhouse/run-1"}},"delivery":{"pushed_head":"%s","pr_url":"https://github.com/acme/shop/pull/1","pr_number":1,"current":false,"checks":[{"name":"test","status":"completed","conclusion":"success","head":"%s"}]}}`, submitted, head, head, head)
	if _, err := parseGreenhouseDelivery([]byte(base), "run-1", "posse/t1", head); err != nil {
		t.Fatalf("parse valid repaired delivery after handback: %v", err)
	}
	for name, input := range map[string]string{
		"forged run id":       strings.Replace(base, `"run_id":"run-1"`, `"run_id":"forged"`, 1),
		"moved pushed head":   strings.Replace(base, `"pushed_head":"`+head+`"`, `"pushed_head":"`+submitted+`"`, 1),
		"candidate mismatch":  strings.Replace(base, `"candidate":"`+head+`"`, `"candidate":"`+submitted+`"`, 1),
		"failed CI":           strings.Replace(base, `"conclusion":"success"`, `"conclusion":"failure"`, 1),
		"wrong branch":        strings.Replace(base, `"branch":"posse/t1"`, `"branch":"posse/t2"`, 1),
		"wrong candidate ref": strings.Replace(base, `refs/heads/greenhouse/run-1`, `refs/heads/greenhouse/run-2`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseGreenhouseDelivery([]byte(input), "run-1", "posse/t1", head); err == nil {
				t.Fatal("accepted invalid Greenhouse delivery")
			}
		})
	}
}

func TestInspectGreenhouseDeliveryUsesConfiguredHomeAndRequiresCandidateAncestry(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	initRepo(t, repo)
	submitted := strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD"))
	gitTest(t, repo, "checkout", "-b", "posse/t1")
	if err := os.WriteFile(filepath.Join(repo, "change.txt"), []byte("repaired\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", "change.txt")
	gitTest(t, repo, "commit", "-m", "repaired candidate")
	head := strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD"))
	unrelated := strings.TrimSpace(gitTest(t, repo, "commit-tree", head+"^{tree}", "-m", "unrelated root"))
	home := filepath.Join(dir, "greenhouse-home")
	output := fmt.Sprintf(`{"outcome":"ok","run_id":"run-1","run":{"id":"run-1","submitted":{"branch":"posse/t1","head":"%s"},"candidate":"%s","status":"ci-ready","pipeline":{"status":"ci-ready","ref":"refs/heads/greenhouse/run-1"}},"delivery":{"pushed_head":"%s","pr_url":"https://github.com/acme/shop/pull/1","pr_number":1,"current":false,"checks":[{"name":"test","status":"completed","conclusion":"success","head":"%s"}]}}`, submitted, head, head, head)
	binary := filepath.Join(dir, "greenhouse")
	writeGreenhouseInspectStub(t, binary, home, output)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GREENHOUSE_HOME", home)
	if _, err := inspectGreenhouseDelivery(context.Background(), repo, "run-1", "posse/t1", head); err != nil {
		t.Fatalf("inspect repaired Greenhouse delivery: %v", err)
	}

	unrelatedOutput := strings.Replace(output, `"head":"`+submitted+`"`, `"head":"`+unrelated+`"`, 1)
	writeGreenhouseInspectStub(t, binary, home, unrelatedOutput)
	if _, err := inspectGreenhouseDelivery(context.Background(), repo, "run-1", "posse/t1", head); err == nil || !strings.Contains(err.Error(), "does not descend") {
		t.Fatalf("accepted unrelated Greenhouse candidate: %v", err)
	}
}

func writeGreenhouseInspectStub(t *testing.T, path, home, output string) {
	t.Helper()
	script := "#!/bin/sh\n[ \"$GREENHOUSE_HOME\" = \"" + home + "\" ] || exit 3\nrequest=$(cat)\ncase \"$request\" in *'\"run_id\":\"run-1\"'*) ;; *) exit 4 ;; esac\nprintf '%s\\n' '" + output + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestGreenhouseMergeRequiresTheVerifiedDoneSignalForItsGatedHead(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Delivery", MergeMode: "greenhouse"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	task, err := db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"greenhouse_run_id": "run-1", "greenhouse_pending_head": "abc", "pr_url": "https://github.com/acme/shop/pull/1"}
	if _, err := db.RecordWorkerSignal(ctx, task, "done", "Pipeline passed", data, "task_done"); err != nil {
		t.Fatal(err)
	}
	task, err = db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if err := requireGreenhouseDeliverySignal(ctx, db, task); err != nil {
		t.Fatalf("accepted delivery Signal was rejected: %v", err)
	}
	task.GatedSHA = "forged"
	if err := requireGreenhouseDeliverySignal(ctx, db, task); err == nil {
		t.Fatal("accepted Greenhouse run bound to a different gated head")
	}
}

func TestGreenhousePullRequestRequiresExactCandidateBranchHeadAndGreenChecks(t *testing.T) {
	base := `{"state":"OPEN","headRefOid":"abc","headRefName":"greenhouse/run-1","baseRefName":"main","headRepository":{"nameWithOwner":"acme/shop"},"url":"https://github.com/acme/shop/pull/1","number":1,"statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"}]}`
	validate := func(input string) error {
		return validateGreenhousePullRequest([]byte(input), "abc", "https://github.com/acme/shop/pull/1", 1, "greenhouse/run-1", "main", "acme/shop")
	}
	if err := validate(base); err != nil {
		t.Fatalf("validate green PR: %v", err)
	}
	for name, input := range map[string]string{
		"moved head":       strings.Replace(base, `"headRefOid":"abc"`, `"headRefOid":"other"`, 1),
		"wrong source":     strings.Replace(base, `"headRefName":"greenhouse/run-1"`, `"headRefName":"greenhouse/other"`, 1),
		"wrong target":     strings.Replace(base, `"baseRefName":"main"`, `"baseRefName":"release"`, 1),
		"wrong repository": strings.Replace(base, `acme/shop`, `acme/fork`, 1),
		"closed PR":        strings.Replace(base, `"state":"OPEN"`, `"state":"CLOSED"`, 1),
		"failed check":     strings.Replace(base, `"conclusion":"SUCCESS"`, `"conclusion":"FAILURE"`, 1),
		"pending check":    strings.Replace(base, `"status":"COMPLETED"`, `"status":"IN_PROGRESS"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validate(input); err == nil {
				t.Fatal("accepted PR that is not green at the gated head")
			}
		})
	}
}
