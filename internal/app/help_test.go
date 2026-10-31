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
		"Restart the Worker in the same execution root, optionally with another Profile.",
		"Start a Worker from a Brief with an optional display name.",
		"List a Project's Pool or prune safe, Posse-owned orphan panes.",
		"Install or update the Herdr plugin and hooks; Posse skills load at launch, with optional global install/removal (--check previews; --human prints a checklist).",
		"Send a message or a Markdown instruction file, optionally queueing it or interrupting and redirecting a busy Worker.",
		"Withdraw a queued Worker message before delivery begins.",
		"Interrupt a working Worker and verify it stopped; report when no working turn can be interrupted.",
		"Record a Worker's Signal; done requires the current turn token and is refused while queued instructions remain undelivered.",
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
	if code := cli.Run([]string{"send", "--help"}); code != 0 || !strings.Contains(output.String(), "send <task> --interrupt <message>") || !strings.Contains(output.String(), "send <task> --file <path.md>") || !strings.Contains(output.String(), "--queue") {
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
	if code := cli.Run([]string{"publish", "--help"}); code != 0 || !strings.Contains(output.String(), "[--verify <command -> result>]... [--proof <markdown>]... [--risk <markdown>]...") || !strings.Contains(output.String(), "Repeated --verify, --proof and --risk values are retained in order.") || !strings.Contains(output.String(), "Risk must state how to roll back") {
		t.Fatalf("publish help omitted repeatable metadata flags: code=%d output=%s", code, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"signal", "--help"}); code != 0 || !strings.Contains(output.String(), "[--findings <file>] [--resolutions <file>] [--report <file>]") {
		t.Fatalf("signal help omitted Signal file flags: code=%d output=%s", code, output.String())
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
