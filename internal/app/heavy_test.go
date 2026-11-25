package app

import (
	"strings"
	"testing"
)

func TestHeavyCommandRequiresSeparator(t *testing.T) {
	code, output := runCLI(t, testService(t.TempDir(), nil), "heavy", "printf", "missing-separator")
	if code != 2 || !strings.Contains(output, "usage: posse heavy") {
		t.Fatalf("heavy without separator exit=%d output=%s", code, output)
	}
}
