//go:build e2e && linux

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGreenhouseParallelRetryExhaustionCountsOnceAfterRecovery(t *testing.T) {
	f := newWorkerTabsFixture(t)
	projectConfig := filepath.Join(f.home, "projects", "shop", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfig, []byte("review_parallel = \"greenhouse\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newGreenhouseContract(t, f)
	c.change = f.start(t, "t1", "Greenhouse retry exhaustion", "greenhouse-retry-exhaust")
	c.authorCommit(t)
	c.hold(t)

	binary := filepath.Join(f.root, "bin", "greenhouse")
	realBinary := binary + ".real"
	if err := os.Rename(binary, realBinary); err != nil {
		t.Fatal(err)
	}
	wrapper := fmt.Sprintf(`#!/bin/sh
if [ "$1" = axi ]; then
    request=$(cat)
    if printf '%%s' "$request" | grep -q '"op":"review"'; then
        printf '%%s' "$request" | '%s' axi >/dev/null 2>&1
        exit 1
    fi
    printf '%%s' "$request" | '%s' axi
    exit $?
fi
exec '%s' "$@"
`, realBinary, realBinary, realBinary)
	if err := os.WriteFile(binary, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}

	output, err := c.posse(c.env, f.repo, "_greenhouse", "review", "t1", "--reviewer-backend", "pi", "--reviewer-model", "fixture/model", "--timeout", "60000", "--json")
	if err == nil || !strings.Contains(output, "greenhouse_failed") {
		t.Fatalf("exhausted transport retry result: %v\n%s", err, output)
	}
	c.waitInvocations(t, 1)
	assertGreenhouseRequestStats(t, f, c.env, 1, 0)

	if err := os.Rename(realBinary, binary); err != nil {
		t.Fatal(err)
	}
	c.release(t)
	var recovered map[string]any
	if !waitForCondition(30*time.Second, func() bool {
		output, err := c.posse(c.env, f.repo, "_greenhouse", "review", "t1", "--reviewer-backend", "pi", "--reviewer-model", "fixture/model", "--timeout", "60000", "--json")
		if err != nil {
			return false
		}
		recovered = decodeGreenhouseOutput(t, output)
		return recovered["outcome"] == "settled"
	}) {
		t.Fatalf("retry-exhausted request did not recover: %v", recovered)
	}
	assertGreenhouseRequestStats(t, f, c.env, 1, 1)
}
