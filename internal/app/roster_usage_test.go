package app

import (
	"bytes"
	"strings"
	"testing"
)

func TestRosterUsageErrorUsesCommandName(t *testing.T) {
	output := &bytes.Buffer{}
	cli := testService(t.TempDir(), nil).CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"roster", "unexpected"}); code != 2 || !strings.Contains(output.String(), "roster") {
		t.Fatalf("roster usage error code=%d output=%s", code, output.String())
	}
}
