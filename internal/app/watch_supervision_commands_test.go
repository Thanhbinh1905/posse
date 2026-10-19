package app

import (
	"bytes"
	"context"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
)

func TestNoticeOnlyCommandsSkipWatchSupervision(t *testing.T) {
	previous := watchExecutableVerifier
	t.Cleanup(func() { watchExecutableVerifier = previous })
	calls := 0
	watchExecutableVerifier = func(string) bool {
		calls++
		return false
	}
	service := New(t.TempDir(), nil)
	ctx := &axi.Context{Context: context.Background(), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}
	for _, command := range []string{"ack", "watch", "lookout"} {
		t.Run(command, func(t *testing.T) {
			before := calls
			service.superviseWatchRuntimes(ctx, command, nil)
			if calls != before {
				t.Fatalf("delivery-only %s command probed Project Watch supervision", command)
			}
		})
	}
}
