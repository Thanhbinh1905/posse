package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestHelpHidesInternalContextAndUsesCanonicalCommandSummaries(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "")
	service := testService(t.TempDir(), herdr.NewFake())
	cli := service.CLI()
	var output bytes.Buffer
	cli.Out = &output
	cli.ErrOut = &output

	if code := cli.Run([]string{"--help"}); code != 0 {
		t.Fatalf("posse --help exit = %d: %s", code, output.String())
	}
	help := output.String()
	if strings.Contains(help, "_context") {
		t.Fatalf("posse --help exposed an internal command: %s", help)
	}
	for _, want := range []string{
		"Restart the Rider in the same Mount, optionally with another Profile.",
		"Start a Rider from a Brief with a short name.",
		"Install or update the Herdr plugin, skills, SessionStart hooks and default config (--check previews; --exit-code makes it exit 3 when changes are pending; --human prints a checklist)",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("posse --help omitted summary %q: %s", want, help)
		}
	}
	if code := cli.Run([]string{"_context"}); code != 0 {
		t.Fatalf("hidden _context command stopped working: %d", code)
	}
	output.Reset()
	if code := cli.Run([]string{"relaunch", "--help"}); code != 0 || !strings.Contains(output.String(), "posse relaunch <task> [--profile <name>]") {
		t.Fatalf("relaunch help omitted the Profile option: code=%d output=%s", code, output.String())
	}

	var checkSummary func(*axi.Command)
	checkSummary = func(command *axi.Command) {
		if command.Summary != "" && strings.ContainsAny(command.Summary, "\r\n") {
			t.Errorf("%s summary is not one line: %q", command.Name, command.Summary)
		}
		for _, child := range command.Subcommands {
			checkSummary(child)
		}
	}
	checkSummary(service.commands())
}
