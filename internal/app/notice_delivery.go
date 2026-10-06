package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

var noticeDeliveryLeaseTimeout = 2 * time.Minute

func (s *Service) noticeDeliveryGeneration(ctx context.Context) (string, error) {
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return "", fmt.Errorf("cannot read Herdr generation for Notice delivery: %w", err)
	}
	if snapshot.ServerStartedAt == "" {
		return "", errors.New("cannot determine Herdr server generation for Notice delivery")
	}
	return snapshot.ServerStartedAt, nil
}

func int64Strings(values []int64) []string {
	strings := make([]string, len(values))
	for index, value := range values {
		strings[index] = fmt.Sprint(value)
	}
	return strings
}

func noticeDeliveryUncertaintyResult(delivery store.NoticeDelivery, reason string) axi.Object {
	return axi.Object{
		{Key: "state", Value: "uncertain"},
		{Key: "delivery", Value: noticeDeliveryRow(delivery, false)},
		{Key: "warning", Value: fmt.Sprintf("Receipt %s for Notice batch %s may have reached %s. %s for Notice IDs %s. Resolve it only after confirming the outcome with `posse lookout --receipt %s --receipt-outcome accepted|rejected`.", delivery.DeliveryID, delivery.BatchID, delivery.Destination, reason, strings.Join(int64Strings(delivery.NoticeIDs), ","), delivery.DeliveryID)},
		{Key: "help", Value: []any{"Inspect the adapter session or recorded effect; do not retry the Notice blindly", fmt.Sprintf("Resolve the confirmed outcome with `posse lookout --receipt %s --receipt-outcome accepted|rejected`", delivery.DeliveryID)}},
	}
}

func noticeDeliveryRow(delivery store.NoticeDelivery, includeToken bool) axi.Object {
	row := axi.Object{
		{Key: "delivery_id", Value: delivery.DeliveryID},
		{Key: "batch_id", Value: delivery.BatchID},
		{Key: "notice_ids", Value: delivery.NoticeIDs},
		{Key: "destination", Value: delivery.Destination},
		{Key: "generation", Value: delivery.Generation},
		{Key: "state", Value: delivery.State},
		{Key: "lease_until", Value: delivery.LeaseUntil},
	}
	if includeToken {
		row = append(row, axi.Field{Key: "owner_token", Value: delivery.OwnerToken})
	}
	return row
}

func waitNoticeDeliveryRetry(ctx context.Context) error {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
