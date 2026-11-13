package greenhouse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDaemonEnvironmentDropsTaskAndPaneIdentity(t *testing.T) {
	d := Daemon{Home: "/posse/greenhouse", Env: []string{"PATH=/bin", "POSSE_TASK_ID=marker", "POSSE_HOME=/posse", "HERDR_PANE_ID=p1", "GREENHOUSE_HOME=/user/greenhouse", "GIT_DIR=/repo/.git", "PI_CODING_AGENT_DIR=/agent"}}
	got := d.environment()
	want := []string{"PATH=/bin", "PI_CODING_AGENT_DIR=/agent", "GREENHOUSE_HOME=/posse/greenhouse"}
	if !slices.Equal(got, want) {
		t.Fatalf("environment() = %v, want %v", got, want)
	}
}

func TestAXIReportsTransientServerEOF(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "greenhouse")
	script := `#!/bin/sh
printf '%s\n' '{"version":1,"outcome":"error","error":"EOF"}'
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Binary: binary, Home: filepath.Join(dir, "home")}
	_, err := d.axi(context.Background(), dir, []byte(`{"version":2,"op":"attach"}`))
	if !errors.Is(err, errUnavailable) {
		t.Fatalf("axi error = %v, want transient daemon unavailability", err)
	}
}

func TestProtocolErrorNamesBothRanges(t *testing.T) {
	err := (&ProtocolError{Daemon: Range{Min: 1, Max: 1}}).Error()
	if want := "the Greenhouse daemon speaks protocol 1 and Posse needs protocol 2"; err[:len(want)] != want {
		t.Fatalf("ProtocolError = %q", err)
	}
	if (Range{Min: 1, Max: 2}).String() != "1-2" || !(Range{Min: 1, Max: 2}).Includes(Protocol) || (Range{Min: 1, Max: 1}).Includes(Protocol) {
		t.Fatal("Range comparison is wrong")
	}
}
