package app

import (
	"bytes"
	"strings"
	"testing"
)

func TestListUsageErrorUsesCommandName(t *testing.T) {
	output := &bytes.Buffer{}
	cli := testService(t.TempDir(), nil).CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"list", "unexpected"}); code != 2 || !strings.Contains(output.String(), "list") {
		t.Fatalf("list usage error code=%d output=%s", code, output.String())
	}
}
