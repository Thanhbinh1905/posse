package app

import (
	"context"
	"testing"
)

func TestDownWaitsForWatchMaintenanceBeforeTaskTeardown(t *testing.T) {
	watchStopped := make(chan struct{})
	maintenanceReleased := make(chan struct{})
	teardownStarted := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- stopProjectWatchesBeforeDownTeardown(context.Background(), func(context.Context) error {
			close(watchStopped)
			<-maintenanceReleased
			return nil
		}, func() error {
			close(teardownStarted)
			return nil
		})
	}()
	<-watchStopped
	select {
	case <-teardownStarted:
		t.Fatal("Task teardown started while Watch maintenance was still active")
	default:
	}
	close(maintenanceReleased)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	select {
	case <-teardownStarted:
	default:
		t.Fatal("Task teardown did not start after Watch maintenance stopped")
	}
}
