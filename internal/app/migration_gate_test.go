package app

import (
	"errors"
	"fmt"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestSchemaFailureMakesStoreContentionRetryable(t *testing.T) {
	err := schemaFailure(fmt.Errorf("open database: %w", store.ErrBusy))
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "store_busy" || !failure.Retryable {
		t.Fatalf("schemaFailure(%v) = %#v, want retryable store_busy", store.ErrBusy, err)
	}
}
