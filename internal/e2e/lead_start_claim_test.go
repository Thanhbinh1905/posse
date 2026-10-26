//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestUpReportsRestoreLeadStartClaim(t *testing.T) {
	f := newWorkerTabsFixture(t)
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(f.root, "hold-lead-start"))
		stopIsolatedFinalizers(t, f.root)
	})
	hold := filepath.Join(f.root, "hold-lead-start")
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	closeWorkerGroup(t, f)
	recovery := exec.Command(f.binary, "up", "--all")
	recovery.Dir, recovery.Env = f.repo, f.leadEnv
	var recoveryLog bytes.Buffer
	recovery.Stdout, recovery.Stderr = &recoveryLog, &recoveryLog
	recoveryOutput := make(chan struct {
		output []byte
		err    error
	}, 1)
	if err := recovery.Start(); err != nil {
		t.Fatalf("start isolated restore: %v", err)
	}
	go func() {
		err := recovery.Wait()
		recoveryOutput <- struct {
			output []byte
			err    error
		}{recoveryLog.Bytes(), err}
	}()
	t.Cleanup(func() {
		select {
		case <-recoveryOutput:
		default:
			_ = recovery.Process.Kill()
		}
	})

	var claim store.LeadStartClaim
	if !waitForCondition(30*time.Second, func() bool {
		db, err := store.OpenReadOnly(f.home)
		if err != nil {
			return false
		}
		defer db.Close()
		claim, err = db.LeadStartClaim(context.Background(), f.projectID)
		return err == nil && claim.Holder == "restore"
	}) {
		t.Fatalf("restore did not acquire the Lead start claim: %+v", claim)
	}

	for _, args := range [][]string{{"up", "--name", "shop"}, {"up", "--name", "shop", "--replace"}} {
		command := exec.Command(f.binary, args...)
		command.Dir, command.Env = f.repo, f.leadEnv
		output, runErr := command.CombinedOutput()
		if runErr == nil || !strings.Contains(string(output), `"lead_starting"`) {
			t.Fatalf("posse %s was not refused by an active restore: err=%v output=%s", strings.Join(args, " "), runErr, output)
		}
		for _, want := range []string{"restore", "pid " + strconv.FormatInt(claim.PID, 10), "claimed at", "claim expires at", "(in "} {
			if !strings.Contains(string(output), want) {
				t.Errorf("posse %s claim refusal omitted %q: %s", strings.Join(args, " "), want, output)
			}
		}
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-recoveryOutput:
		if result.err != nil {
			t.Fatalf("recovery after readiness release: %v: %s", result.err, result.output)
		}
	case <-time.After(35 * time.Second):
		t.Fatal("recovery did not finish restarting the Lead after readiness was released")
	}
	if _, found := findLeadInSnapshot(f.snapshot(t)); !found {
		t.Fatal("recovery completed without restoring the Lead")
	}
}
