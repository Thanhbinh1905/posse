package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestInvalidURLDecisionIsIndependentPerTask(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateDone)
	defer f.db.Close()
	ctx := context.Background()
	url := "https://github.com/acme/shop/issues/17"
	first := f.task
	first.PRURL = url
	if err := f.db.UpdateTaskLanding(ctx, first.ID, url, ""); err != nil {
		t.Fatal(err)
	}
	if err := raiseInvalidPRDecision(ctx, f.db, f.project, first); err != nil {
		t.Fatal(err)
	}
	decisions, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil || len(decisions) != 1 {
		t.Fatalf("first Decision: %#v %v", decisions, err)
	}
	if _, err := f.db.AnswerDecision(ctx, f.project.ID, decisions[0].ID, "reopen-relaunch", "Use a PR URL"); err != nil {
		t.Fatal(err)
	}
	secondID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{Seq: 2, Type: "ship", Title: "second", State: store.StateSpawning, LandingMode: "pr", Branch: "posse/second", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateTaskLanding(ctx, secondID, url, ""); err != nil {
		t.Fatal(err)
	}
	second, err := f.db.TaskByID(ctx, f.project.ID, secondID)
	if err != nil {
		t.Fatal(err)
	}
	if err := raiseInvalidPRDecision(ctx, f.db, f.project, second); err != nil {
		t.Fatal(err)
	}
	all, err := f.db.Decisions(ctx, f.project.ID, false)
	if err != nil || len(all) != 2 {
		t.Fatalf("second Task received no Decision: %#v %v", all, err)
	}
}

func TestGuardBlocksLiteralNestedGitPush(t *testing.T) {
	scope, _ := guardFixture(t)
	for _, command := range []string{
		"git rebase -x 'git push origin HEAD' HEAD~1",
		"git submodule foreach git push origin HEAD",
	} {
		if refused, _ := guardCommand(command, scope); !refused {
			t.Errorf("remote write allowed: %s", command)
		}
	}
}

func TestConcurrentPollersClaimOnePRPollPerInterval(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateDone)
	defer f.db.Close()
	ctx := context.Background()
	if err := f.db.UpdateTaskLanding(ctx, f.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join(f.bin, "gh"))
	if err != nil {
		t.Fatal(err)
	}
	slow := strings.Replace(string(script), "case \"$1 $2\" in", "sleep 1\ncase \"$1 $2\" in", 1)
	if err := os.WriteFile(filepath.Join(f.bin, "gh"), []byte(slow), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.PRPoll != "2m" {
		t.Fatalf("unexpected default pr_poll: %s", cfg.Defaults.PRPoll)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, false)
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	log, err := os.ReadFile(f.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "api graphql"); got != 1 {
		t.Fatalf("two pollers made %d GraphQL calls within default 2m interval", got)
	}
}
