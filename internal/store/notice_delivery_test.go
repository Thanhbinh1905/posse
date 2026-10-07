package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
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
	if err := db.ResolveNoticeDelivery(ctx, project.ID, third.DeliveryID, "owner-b", "accepted", 1212); !errors.Is(err, ErrStateRace) {
		t.Fatalf("stale owner receipt = %v, want ErrStateRace", err)
	}
	if err := db.ResolveNoticeDelivery(ctx, project.ID, third.DeliveryID, "owner-c", "rejected", 1212); err != nil {
		t.Fatal(err)
	}

	retry, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{firstID, secondID}, "pi:session-1", "herdr:g3", "owner-d", 1310, 100)
	if err != nil || !claimed || retry.DeliveryID != first.DeliveryID || retry.BatchID != first.BatchID || retry.State != "claimed" {
		t.Fatalf("exact rejected retry = %#v, %v, %v", retry, claimed, err)
	}
	if err := db.MarkNoticeDeliveryPrinted(ctx, retry.DeliveryID, "owner-d", 1311); err != nil {
		t.Fatal(err)
	}
	if err := db.ResolveNoticeDelivery(ctx, project.ID, retry.DeliveryID, "owner-d", "accepted", 1312); err != nil {
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

func TestPrintedNoticeCanBeAcknowledgedBeforeReceiptAcceptance(t *testing.T) {
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
	noticeID, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "task_done", Summary: "handled in OpenCode turn"})
	if err != nil {
		t.Fatal(err)
	}
	delivery, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{noticeID}, "opencode:session-1", "herdr:g1", "owner-a", 1000, 2000)
	if err != nil || !claimed {
		t.Fatalf("claim Notice: %#v, %v, %v", delivery, claimed, err)
	}
	if err := db.MarkNoticeDeliveryPrinted(ctx, delivery.DeliveryID, "owner-a", 1010); err != nil {
		t.Fatal(err)
	}
	acked, err := db.AckNotices(ctx, project.ID, []string{fmt.Sprint(noticeID)})
	if err != nil || acked != 1 {
		t.Fatalf("Lead ack while OpenCode turn owns printed receipt: acknowledged=%d err=%v", acked, err)
	}
	before, err := db.NoticesByIDs(ctx, project.ID, []int64{noticeID})
	if err != nil || before[0].AckedAt == 0 || before[0].DeliveredAt != 0 {
		t.Fatalf("acknowledgment before receipt acceptance = %#v, %v", before, err)
	}
	if err := db.ResolveNoticeDelivery(ctx, project.ID, delivery.DeliveryID, "owner-a", "accepted", 1020); err != nil {
		t.Fatalf("accept receipt after in-turn acknowledgment: %v", err)
	}
	after, err := db.NoticesByIDs(ctx, project.ID, []int64{noticeID})
	if err != nil || after[0].DeliveredAt != 1020 || after[0].AckedAt != before[0].AckedAt {
		t.Fatalf("accepted receipt did not preserve earlier acknowledgment: %#v, %v", after, err)
	}

	allNoticeID, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "task_done", Summary: "ack all while OpenCode turn owns receipt"})
	if err != nil {
		t.Fatal(err)
	}
	allDelivery, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{allNoticeID}, "opencode:session-1", "herdr:g1", "owner-b", 1100, 2000)
	if err != nil || !claimed {
		t.Fatalf("ack-all claim Notice: %#v, %v, %v", allDelivery, claimed, err)
	}
	if err := db.MarkNoticeDeliveryPrinted(ctx, allDelivery.DeliveryID, "owner-b", 1110); err != nil {
		t.Fatal(err)
	}
	acked, err = db.AckNotices(ctx, project.ID, []string{"all"})
	if err != nil || acked != 1 {
		t.Fatalf("Lead ack all while OpenCode turn owns printed receipt: acknowledged=%d err=%v", acked, err)
	}
	if err := db.ResolveNoticeDelivery(ctx, project.ID, allDelivery.DeliveryID, "owner-b", "accepted", 1120); err != nil {
		t.Fatalf("accept receipt after ack all: %v", err)
	}
	allAfter, err := db.NoticesByIDs(ctx, project.ID, []int64{allNoticeID})
	if err != nil || allAfter[0].DeliveredAt != 1120 || allAfter[0].AckedAt == 0 {
		t.Fatalf("accepted ack-all receipt did not preserve acknowledgment: %#v, %v", allAfter, err)
	}
}

func TestAckedPrintedReceiptCanBeTakenOverAfterLeaseExpiry(t *testing.T) {
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
	noticeID, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "task_done", Summary: "handled before receipt acceptance"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	first, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{noticeID}, "opencode:session-1", "herdr:g1", "owner-a", now, 100)
	if err != nil || !claimed {
		t.Fatalf("claim = %#v, %v, %v", first, claimed, err)
	}
	if err := db.MarkNoticeDeliveryPrinted(ctx, first.DeliveryID, "owner-a", now+1); err != nil {
		t.Fatal(err)
	}
	if acked, err := db.AckNotices(ctx, project.ID, []string{fmt.Sprint(noticeID)}); err != nil || acked != 1 {
		t.Fatalf("ack printed Notice = %d, %v", acked, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE notice_delivery_receipts SET lease_until=0 WHERE delivery_id=?`, first.DeliveryID); err != nil {
		t.Fatal(err)
	}
	retry, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{noticeID}, "opencode:session-1", "herdr:g2", "owner-b", time.Now().UnixMilli(), 1000)
	if err != nil || !claimed || retry.DeliveryID != first.DeliveryID || retry.BatchID != first.BatchID || retry.State != "printed" || retry.OwnerToken != "owner-b" {
		t.Fatalf("expired acknowledged receipt takeover = %#v claimed=%v err=%v", retry, claimed, err)
	}
	var token string
	var ackedAt int64
	if err := db.QueryRowContext(ctx, `SELECT claim_token,acked_at FROM notices WHERE id=?`, noticeID).Scan(&token, &ackedAt); err != nil || token != "owner-b" || ackedAt == 0 {
		t.Fatalf("takeover lost ownership or acknowledgment: claim_token=%q acked_at=%d err=%v", token, ackedAt, err)
	}
}

func TestRejectedPrintedReceiptReopensAcknowledgedNotice(t *testing.T) {
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
	noticeID, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "task_done", Summary: "reopen after known rejection"})
	if err != nil {
		t.Fatal(err)
	}
	delivery, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{noticeID}, "opencode:session-1", "herdr:g1", "owner-a", 1000, 1000)
	if err != nil || !claimed {
		t.Fatalf("claim = %#v, %v, %v", delivery, claimed, err)
	}
	if err := db.MarkNoticeDeliveryPrinted(ctx, delivery.DeliveryID, "owner-a", 1010); err != nil {
		t.Fatal(err)
	}
	if acked, err := db.AckNotices(ctx, project.ID, []string{fmt.Sprint(noticeID)}); err != nil || acked != 1 {
		t.Fatalf("ack before known rejection = %d, %v", acked, err)
	}
	if err := db.ResolveNoticeDelivery(ctx, project.ID, delivery.DeliveryID, "owner-a", "rejected", 1020); err != nil {
		t.Fatal(err)
	}
	notices, err := db.Notices(ctx, project.ID, true)
	if err != nil || len(notices) != 1 || notices[0].ID != noticeID || notices[0].AckedAt != 0 {
		t.Fatalf("known rejection did not reopen acknowledged Notice: %#v, %v", notices, err)
	}
}

func TestAckedUncertainReceiptOwnershipSurvivesRebuild(t *testing.T) {
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
	noticeID, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "task_done", Summary: "acknowledged uncertain receipt"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	delivery, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{noticeID}, "opencode:session-1", "herdr:g1", "owner-a", now, 1000)
	if err != nil || !claimed {
		t.Fatalf("claim = %#v, %v, %v", delivery, claimed, err)
	}
	if err := db.MarkNoticeDeliveryPrinted(ctx, delivery.DeliveryID, "owner-a", now+1); err != nil {
		t.Fatal(err)
	}
	if acked, err := db.AckNotices(ctx, project.ID, []string{fmt.Sprint(noticeID)}); err != nil || acked != 1 {
		t.Fatalf("ack printed Notice = %d, %v", acked, err)
	}
	if err := db.ResolveNoticeDelivery(ctx, project.ID, delivery.DeliveryID, "owner-a", "uncertain", now+2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
		t.Fatalf("rebuild uncertain receipt: %v", err)
	}
	var token string
	if err := db.QueryRowContext(ctx, `SELECT claim_token FROM notices WHERE id=?`, noticeID).Scan(&token); err != nil || token != "uncertain:"+delivery.DeliveryID {
		t.Fatalf("rebuilt uncertain receipt claim token=%q err=%v", token, err)
	}
	if err := db.ResolveUncertainNoticeDelivery(ctx, project.ID, delivery.DeliveryID, "accepted", now+3); err != nil {
		t.Fatalf("resolve rebuilt uncertain receipt: %v", err)
	}
	notices, err := db.NoticesByIDs(ctx, project.ID, []int64{noticeID})
	if err != nil || notices[0].DeliveredAt != now+3 || notices[0].AckedAt == 0 {
		t.Fatalf("resolution lost accepted delivery or acknowledgment: %+v err=%v", notices, err)
	}
}

func TestNoticeDeliveryResolutionIsProjectScoped(t *testing.T) {
	ctx := context.Background()
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	caller, err := db.CreateProject(ctx, "caller", "/missing/caller", "main")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := db.CreateProject(ctx, "owner", "/missing/owner", "main")
	if err != nil {
		t.Fatal(err)
	}
	noticeID, err := db.CreateNotice(ctx, Notice{ProjectID: owner.ID, Kind: "task_done", Summary: "private batch"})
	if err != nil {
		t.Fatal(err)
	}
	delivery, claimed, err := db.ClaimNoticeDelivery(ctx, owner.ID, []int64{noticeID}, "pi:owner-session", "herdr:g1", "owner-token", 1000, 500)
	if err != nil || !claimed {
		t.Fatalf("foreign Project claim = %#v, %v, %v", delivery, claimed, err)
	}
	if err := db.MarkNoticeDeliveryPrinted(ctx, delivery.DeliveryID, "owner-token", 1010); err != nil {
		t.Fatal(err)
	}
	if err := db.ResolveNoticeDelivery(ctx, caller.ID, delivery.DeliveryID, "owner-token", "uncertain", 1020); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("token-backed cross-Project resolution = %v, want sql.ErrNoRows", err)
	}
	if err := db.ResolveNoticeDelivery(ctx, owner.ID, delivery.DeliveryID, "owner-token", "uncertain", 1020); err != nil {
		t.Fatal(err)
	}
	if err := db.ResolveUncertainNoticeDelivery(ctx, caller.ID, delivery.DeliveryID, "accepted", 1030); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("tokenless cross-Project resolution = %v, want sql.ErrNoRows", err)
	}
	current, err := db.NoticeDelivery(ctx, delivery.DeliveryID)
	if err != nil || current.State != "uncertain" {
		t.Fatalf("cross-Project resolution changed receipt: %#v, %v", current, err)
	}
	notices, err := db.NoticesByIDs(ctx, owner.ID, []int64{noticeID})
	if err != nil || notices[0].DeliveredAt != 0 {
		t.Fatalf("cross-Project resolution changed Notice delivery: %#v, %v", notices, err)
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

func TestExpiredUnprintedClaimCanMoveToReplacementSession(t *testing.T) {
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
	noticeID, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "task_done", Summary: "unprinted old-session batch"})
	if err != nil {
		t.Fatal(err)
	}
	old, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{noticeID}, "pi:old-session", "herdr:g1", "owner-a", 1000, 50)
	if err != nil || !claimed {
		t.Fatalf("old-session claim = %#v, %v, %v", old, claimed, err)
	}
	if err := db.RejectExpiredNoticeDeliveryClaim(ctx, old.DeliveryID, 1100); err != nil {
		t.Fatalf("reject expired unprinted claim: %v", err)
	}
	current, err := db.NoticeDelivery(ctx, old.DeliveryID)
	if err != nil || current.State != "rejected" {
		t.Fatalf("expired old receipt = %#v, %v", current, err)
	}
	replacement, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{noticeID}, "pi:new-session", "herdr:g2", "owner-b", 1110, 100)
	if err != nil || !claimed || replacement.DeliveryID == old.DeliveryID || replacement.BatchID != old.BatchID || len(replacement.NoticeIDs) != 1 || replacement.NoticeIDs[0] != noticeID {
		t.Fatalf("replacement did not preserve the exact stable batch: old=%#v new=%#v claimed=%v err=%v", old, replacement, claimed, err)
	}
}

func TestReplacementSessionHoldsPrintedBatchAsUncertain(t *testing.T) {
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
	noticeID, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "task_done", Summary: "old session batch"})
	if err != nil {
		t.Fatal(err)
	}
	old, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{noticeID}, "pi:old-session", "herdr:g1", "owner-a", 1000, 50)
	if err != nil || !claimed {
		t.Fatalf("old-session claim = %#v, %v, %v", old, claimed, err)
	}
	if err := db.MarkNoticeDeliveryPrinted(ctx, old.DeliveryID, "owner-a", 1010); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE notice_delivery_receipts SET lease_until=0 WHERE delivery_id=?`, old.DeliveryID); err != nil {
		t.Fatal(err)
	}
	prior, found, err := db.NoticeDeliveryForOtherSession(ctx, project.ID, "pi:new-session")
	if err != nil || !found || prior.DeliveryID != old.DeliveryID || prior.NoticeIDs[0] != noticeID {
		t.Fatalf("replacement lookup = %#v found=%v err=%v", prior, found, err)
	}
	if err := db.MarkNoticeDeliveryUncertain(ctx, prior.DeliveryID, 1200); err != nil {
		t.Fatalf("hold old-session printed batch: %v", err)
	}
	current, err := db.NoticeDelivery(ctx, old.DeliveryID)
	if err != nil || current.State != "uncertain" || current.BatchID != old.BatchID || current.OwnerToken != "" {
		t.Fatalf("replacement receipt = %#v, %v", current, err)
	}
	if _, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{noticeID}, "pi:new-session", "herdr:g2", "owner-b", 1300, 100); claimed || !errors.Is(err, ErrStateRace) {
		t.Fatalf("replacement claim bypassed uncertain hold: claimed=%v err=%v", claimed, err)
	}
	notices, err := db.Notices(ctx, project.ID, true)
	if err != nil || len(notices) != 2 || notices[0].ID != noticeID || notices[1].Kind != "notice_delivery_uncertain" {
		t.Fatalf("replacement uncertainty Notices = %#v, %v", notices, err)
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
	if err := db.ResolveNoticeDelivery(ctx, project.ID, delivery.DeliveryID, "owner-a", "uncertain", 1300); err != nil {
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
