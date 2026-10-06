package app

import (
	"context"
	"errors"
	"fmt"
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
