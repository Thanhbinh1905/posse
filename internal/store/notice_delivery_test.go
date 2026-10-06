package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestNoticeDeliveryOwnershipAndExactRetry(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", "/missing/shop", "main")
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "test", Summary: "first"})
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "test", Summary: "second"})
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{secondID, firstID}

	first, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, ids, "pi:session-1", "herdr:g1", "owner-a", 1000, 100)
	if err != nil || !claimed {
		t.Fatalf("initial delivery claim = %#v, %v, %v", first, claimed, err)
	}
	if acked, err := db.AckNotices(ctx, project.ID, []string{"all"}); err != nil || acked != 0 {
		t.Fatalf("acknowledged a Notice owned by an active delivery: count=%d err=%v", acked, err)
	}
	competitor, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, ids, "pi:session-1", "herdr:g1", "owner-b", 1050, 100)
	if err != nil || claimed || competitor.OwnerToken != "owner-a" {
		t.Fatalf("live lease competitor = %#v, %v, %v", competitor, claimed, err)
	}
	second, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, ids, "pi:session-1", "herdr:g2", "owner-b", 1100, 100)
	if err != nil || !claimed || second.DeliveryID != first.DeliveryID || second.BatchID != first.BatchID || second.State != "claimed" || second.OwnerToken != "owner-b" {
		t.Fatalf("expired claim-only lease takeover = %#v, %v, %v", second, claimed, err)
	}
	if err := db.MarkNoticeDeliveryPrinted(ctx, second.DeliveryID, "owner-b", 1110); err != nil {
		t.Fatal(err)
	}
	third, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, ids, "pi:session-1", "herdr:g3", "owner-c", 1210, 100)
	if err != nil || !claimed || third.DeliveryID != first.DeliveryID || third.BatchID != first.BatchID || third.State != "printed" || third.OwnerToken != "owner-c" {
		t.Fatalf("expired printed lease takeover = %#v, %v, %v", third, claimed, err)
	}
	if err := db.MarkNoticeDeliveryPrinted(ctx, third.DeliveryID, "owner-c", 1211); err != nil {
		t.Fatal(err)
	}
	if err := db.ResolveNoticeDelivery(ctx, third.DeliveryID, "owner-b", "accepted", 1212); !errors.Is(err, ErrStateRace) {
		t.Fatalf("stale owner receipt = %v, want ErrStateRace", err)
	}
	if err := db.ResolveNoticeDelivery(ctx, third.DeliveryID, "owner-c", "rejected", 1212); err != nil {
		t.Fatal(err)
	}

	retry, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{firstID, secondID}, "pi:session-1", "herdr:g3", "owner-d", 1310, 100)
	if err != nil || !claimed || retry.DeliveryID != first.DeliveryID || retry.BatchID != first.BatchID || retry.State != "claimed" {
		t.Fatalf("exact rejected retry = %#v, %v, %v", retry, claimed, err)
	}
	if err := db.MarkNoticeDeliveryPrinted(ctx, retry.DeliveryID, "owner-d", 1311); err != nil {
		t.Fatal(err)
	}
	if err := db.ResolveNoticeDelivery(ctx, retry.DeliveryID, "owner-d", "accepted", 1312); err != nil {
		t.Fatal(err)
	}
	delivery, err := db.NoticeDelivery(ctx, first.DeliveryID)
	if err != nil || delivery.State != "accepted" || delivery.AcceptedAt != 1312 {
		t.Fatalf("accepted delivery = %#v, %v", delivery, err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 2 {
		t.Fatalf("notices after acceptance = %#v, %v", notices, err)
	}
	for _, notice := range notices {
		if notice.DeliveredAt != 1312 {
			t.Fatalf("Notice #%d delivered_at = %d, want 1312", notice.ID, notice.DeliveredAt)
		}
	}
	if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
		t.Fatalf("rebuild accepted delivery: %v", err)
	}
	delivery, err = db.NoticeDelivery(ctx, first.DeliveryID)
	if err != nil || delivery.State != "accepted" || delivery.AcceptedAt != 1312 {
		t.Fatalf("accepted receipt after rebuild = %#v, %v", delivery, err)
	}
	notices, err = db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 2 {
		t.Fatalf("accepted Notices after rebuild = %#v, %v", notices, err)
	}
	for _, notice := range notices {
		if notice.DeliveredAt != 1312 {
			t.Fatalf("rebuilt Notice #%d delivered_at = %d, want 1312", notice.ID, notice.DeliveredAt)
		}
	}
}

func TestConcurrentNoticeDeliveryClaimsHaveOneOwner(t *testing.T) {
	ctx := context.Background()
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", "/missing/shop", "main")
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "test", Summary: "first batch"})
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "test", Summary: "second batch"})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	type claimResult struct {
		claimed bool
		owner   string
		err     error
	}
	results := make(chan claimResult, 2)
	for _, claim := range []struct {
		token    string
		noticeID int64
	}{{"owner-a", firstID}, {"owner-b", secondID}} {
		claim := claim
		go func() {
			<-start
			delivery, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{claim.noticeID}, "pi:session", "herdr:g1", claim.token, 1000, 500)
			results <- claimResult{claimed: claimed, owner: delivery.OwnerToken, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent claim errors: first=%v second=%v", first.err, second.err)
	}
	if first.claimed == second.claimed {
		t.Fatalf("concurrent destination claims = %#v, %#v; want exactly one owner", first, second)
	}
}

func TestNoticeDeliveryUncertaintyAndRebuildPreserveOwnership(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", "/missing/shop", "main")
	if err != nil {
		t.Fatal(err)
	}
	noticeID, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "test", Summary: "recover me"})
	if err != nil {
		t.Fatal(err)
	}
	delivery, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{noticeID}, "opencode:session-1", "herdr:g1", "owner-a", 1000, 500)
	if err != nil || !claimed {
		t.Fatalf("initial claim = %#v, %v, %v", delivery, claimed, err)
	}
	if err := db.MarkNoticeDeliveryPrinted(ctx, delivery.DeliveryID, "owner-a", 1010); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	restored, err := db.NoticeDelivery(ctx, delivery.DeliveryID)
	if err != nil || restored.State != "printed" || restored.OwnerToken != "owner-a" || restored.LeaseUntil != 1500 {
		t.Fatalf("restored live receipt = %#v, %v", restored, err)
	}
	competing, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{noticeID}, "opencode:session-1", "herdr:g1", "owner-b", 1200, 500)
	if err != nil || claimed || competing.OwnerToken != "owner-a" {
		t.Fatalf("rebuilt lease competitor = %#v, %v, %v", competing, claimed, err)
	}
	if err := db.ResolveNoticeDelivery(ctx, delivery.DeliveryID, "owner-a", "uncertain", 1300); err != nil {
		t.Fatal(err)
	}
	uncertain, err := db.NoticeDelivery(ctx, delivery.DeliveryID)
	if err != nil || uncertain.State != "uncertain" || uncertain.OwnerToken != "" {
		t.Fatalf("uncertain delivery = %#v, %v", uncertain, err)
	}
	if _, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{noticeID}, "opencode:session-1", "herdr:g1", "owner-c", 1400, 500); err != nil || claimed {
		t.Fatalf("uncertain delivery was retryable: claimed=%v err=%v", claimed, err)
	}
	notices, err := db.Notices(ctx, project.ID, true)
	if err != nil || len(notices) != 2 {
		t.Fatalf("open Notices after uncertainty = %#v, %v", notices, err)
	}
	foundUncertainty := false
	for _, notice := range notices {
		foundUncertainty = foundUncertainty || notice.Kind == "notice_delivery_uncertain"
	}
	if !foundUncertainty {
		t.Fatalf("missing uncertainty Notice: %#v", notices)
	}
	if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
		t.Fatalf("rebuild uncertain delivery: %v", err)
	}
	restored, err = db.NoticeDelivery(ctx, delivery.DeliveryID)
	if err != nil || restored.State != "uncertain" {
		t.Fatalf("uncertain receipt after rebuild = %#v, %v", restored, err)
	}
	notices, err = db.Notices(ctx, project.ID, true)
	if err != nil || len(notices) != 2 {
		t.Fatalf("uncertainty Notice after rebuild = %#v, %v", notices, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}
