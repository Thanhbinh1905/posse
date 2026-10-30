package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWatchServesAPassRequestWithoutWaitingOutTheInterval(t *testing.T) {
	fixture := newStartFixture(t)
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	previous := watchPassInterval
	watchPassInterval = time.Hour
	t.Cleanup(func() { watchPassInterval = previous })

	ctx, cancel := context.WithCancel(context.Background())
	output := &bytes.Buffer{}
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- fixture.service.runWatchLoop(&axi.Context{Context: ctx, Out: output, ErrOut: output}, db, fixture.project, 0, nil, nil)
	}()
	t.Cleanup(func() {
		cancel()
		<-loopDone
	})

	done := watchPassDonePath(fixture.home, fixture.project)
	if err := os.MkdirAll(filepath.Dir(done), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"first", "second"} {
		if err := writeFile(watchPassRequestPath(fixture.home, fixture.project), []byte(token+"\n")); err != nil {
			t.Fatal(err)
		}
		served := time.NewTimer(time.Minute)
		for readWatchPassToken(done) != token {
			select {
			case err := <-loopDone:
				t.Fatalf("Watch loop exited before serving %s: %v\n%s", token, err, output.String())
			case <-served.C:
				t.Fatalf("Watch with an hour-long pass interval did not serve pass request %s\n%s", token, output.String())
			case <-time.After(10 * time.Millisecond):
			}
		}
		served.Stop()
	}
}
