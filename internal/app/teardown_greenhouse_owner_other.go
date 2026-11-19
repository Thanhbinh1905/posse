//go:build !linux

package app

import (
	"context"

	"github.com/thanhbinh1905/posse/internal/store"
)

func stopTaskGreenhouseReviewOwners(context.Context, *store.DB, string, store.Project, store.Task) error {
	return nil
}
