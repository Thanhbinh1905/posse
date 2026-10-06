package store

import (
	"context"
	"testing"
	"time"
)

func TestMemberPRPollClaimsAreIndependentAndDurable(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	good, err := db.ClaimMemberPRPoll(ctx, project.ID, "good", now, time.Minute, false)
	if err != nil || good == "" {
		t.Fatalf("claim good Member: token=%q err=%v", good, err)
	}
	offline, err := db.ClaimMemberPRPoll(ctx, project.ID, "offline", now, time.Minute, false)
	if err != nil || offline == "" {
		t.Fatalf("claim offline Member while good is active: token=%q err=%v", offline, err)
	}
	if duplicate, err := db.ClaimMemberPRPoll(ctx, project.ID, "good", now, time.Minute, false); err != nil || duplicate != "" {
		t.Fatalf("duplicate good Member claim: token=%q err=%v", duplicate, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, repo := range []string{"good", "offline"} {
		if duplicate, err := db.ClaimMemberPRPoll(ctx, project.ID, repo, now.Add(time.Second), time.Minute, false); err != nil || duplicate != "" {
			t.Fatalf("Member %s claim was not durable across reopen: token=%q err=%v", repo, duplicate, err)
		}
	}
	if err := db.FinishMemberPRPoll(ctx, project.ID, "good", good, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if due, err := db.ClaimMemberPRPoll(ctx, project.ID, "good", now.Add(30*time.Second), time.Minute, false); err != nil || due != "" {
		t.Fatalf("good Member poll ignored its interval: token=%q err=%v", due, err)
	}
	next, err := db.ClaimMemberPRPoll(ctx, project.ID, "good", now.Add(time.Minute+time.Millisecond), time.Minute, false)
	if err != nil || next == "" {
		t.Fatalf("good Member was not independently due: token=%q err=%v", next, err)
	}
	if err := db.FinishMemberPRPoll(ctx, project.ID, "good", good, now.Add(time.Minute).UnixMilli()); err != ErrStateRace {
		t.Fatalf("stale Member poll owner finished successor's claim: %v", err)
	}
	if err := db.FinishMemberPRPoll(ctx, project.ID, "good", next, now.Add(time.Minute+time.Millisecond).UnixMilli()); err != nil {
		t.Fatal(err)
	}
}
