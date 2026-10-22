package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
)

func TestDoctorHumanOutputGolden(t *testing.T) {
	checks := []axi.Object{
		{{Key: "check", Value: "config"}, {Key: "status", Value: "ok"}, {Key: "detail", Value: "2 profiles"}},
		{{Key: "check", Value: "database"}, {Key: "status", Value: "fail"}, {Key: "detail", Value: "database cannot open: permission denied"}},
		{{Key: "check", Value: "Project disk usage demo"}, {Key: "status", Value: "warn"}, {Key: "detail", Value: "scratch usage is high"}},
		{{Key: "check", Value: "Herdr"}, {Key: "status", Value: "ok"}, {Key: "detail", Value: "protocol 3"}},
		{{Key: "check", Value: "setup manifest"}, {Key: "status", Value: "ok"}, {Key: "detail", Value: "installed"}},
	}
	help := []axi.Object{{{Key: "check", Value: "database"}, {Key: "action", Value: "Inspect the database"}}}
	var output bytes.Buffer
	ctx := &axi.Context{Out: &output, Width: 36}
	if err := renderDoctorHuman(ctx, checks, help, false); err != nil {
		t.Fatal(err)
	}
	want := "Storage\n  ✓ config\n  ✗ database\n      database cannot open:\n      permission denied\nProjects\n  ! Project disk usage demo\n      scratch usage is high\nIntegrations\n  ✓ Herdr\nSetup\n  ✓ setup manifest\n\nSummary: 5 checks, 3 passed, 1 warning, 0 informational, 1 error\n\nNext\n  - database: Inspect the database\n"
	if output.String() != want {
		t.Fatalf("doctor output:\n%s\nwant:\n%s", output.String(), want)
	}
	output.Reset()
	if err := renderDoctorHuman(ctx, checks, nil, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "      2 profiles\n") || !strings.Contains(output.String(), "      protocol 3\n") {
		t.Fatalf("verbose output omitted successful details:\n%s", output.String())
	}
}

func TestUpdateHumanOutputGolden(t *testing.T) {
	var output bytes.Buffer
	ctx := &axi.Context{Out: &output}
	if err := renderUpdateCheckHuman(ctx, "v1.2.0", "v1.3.0", []watchProcess{{Project: "demo", Kind: "lead"}}); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "Posse update\n  Current: v1.2.0\n  Latest:  v1.3.0\n  Watches: 1 running; they restart automatically after installation\n"; got != want {
		t.Fatalf("update check output = %q, want %q", got, want)
	}
	output.Reset()
	if err := renderUpdateInstalledHuman(ctx, "v1.2.0", "v1.3.0", "/opt/posse", nil, []watchProcess{{Project: "demo", Kind: "lead"}}, "", ""); err != nil {
		t.Fatal(err)
	}
	want := "Posse update\n  Current:  v1.2.0\n  Latest:   v1.3.0\n  Download: complete\n  Verify:   SHA-256 checksum passed\n  Installed: v1.3.0\n  Binary:    /opt/posse\n  Watches:  restart requested for 1\n    demo (lead): Lead restarts its background Watch on its next wake\nNext: run `posse doctor` to verify the update.\n"
	if output.String() != want {
		t.Fatalf("update install output:\n%s\nwant:\n%s", output.String(), want)
	}
}
