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
		"Restart the Rider in the same execution root, optionally with another Profile.",
		"Start a Rider from a Brief with a short name.",
		"List a Project's Remuda or prune safe, Posse-owned leftovers.",
		"Install or update the Herdr plugin and hooks; Posse skills load at launch, with optional global install/removal (--check previews; --human prints a checklist).",
		"Send a message, optionally queueing it or interrupting and redirecting a busy Rider.",
		"Withdraw a queued Rider message before delivery begins.",
		"Record a Rider's Signal; done is refused while queued instructions remain undelivered.",
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
	output.Reset()
	if code := cli.Run([]string{"send", "--help"}); code != 0 || !strings.Contains(output.String(), "send <task> --interrupt <message>") || !strings.Contains(output.String(), "--queue") {
		t.Fatalf("send help omitted interrupt or queue usage: code=%d output=%s", code, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"unsend", "--help"}); code != 0 || !strings.Contains(output.String(), "unsend <message-id>") {
		t.Fatalf("unsend help omitted its message ID: code=%d output=%s", code, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"interrupt", "--help"}); code != 0 || !strings.Contains(output.String(), "interrupt <task>") {
		t.Fatalf("interrupt help omitted its Task ID: code=%d output=%s", code, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"publish", "--help"}); code != 0 || !strings.Contains(output.String(), "[--verify <command -> result>]... [--proof <markdown>]... [--risk <markdown>]...") || !strings.Contains(output.String(), "Repeated --verify, --proof and --risk values are retained in order.") {
		t.Fatalf("publish help omitted repeatable metadata flags: code=%d output=%s", code, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"holler", "--help"}); code != 0 || !strings.Contains(output.String(), "[--findings <file>] [--report <file>]") {
		t.Fatalf("holler help omitted Signal file flags: code=%d output=%s", code, output.String())
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
