package axi

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestCommandDispatchAndGlobalJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	root := &Command{
		Name: "posse",
		Handler: func(ctx *Context, args []string) error {
			return ctx.Print(Object{{Key: "status", Value: "ready"}})
		},
		Subcommands: []*Command{{
			Name: "doctor",
			Handler: func(ctx *Context, args []string) error {
				return ctx.Print(Object{{Key: "doctor", Value: "ok"}})
			},
		}},
	}
	app := App{Name: "posse", Root: root, Out: &out, ErrOut: &errOut, Context: context.Background()}
	if code := app.Run([]string{"doctor", "--json"}); code != 0 {
		t.Fatalf("exit code = %d, err=%s", code, errOut.String())
	}
	if out.String() != "{\"doctor\":\"ok\",\"help\":[]}\n" {
		t.Fatalf("JSON output = %q", out.String())
	}
}

func TestPrintWithoutHelpOmitsOnlyGeneratedHelp(t *testing.T) {
	var out bytes.Buffer
	ctx := &Context{Out: &out}
	if err := ctx.PrintWithoutHelp(Object{{Key: "role", Value: "worker"}}); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "role: worker\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	out.Reset()
	if err := ctx.PrintWithoutHelp(Object{{Key: "role", Value: "worker"}, {Key: "help", Value: []string{"read brief"}}}); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "role: worker\nhelp[1]: read brief\n"; got != want {
		t.Fatalf("explicit help output = %q, want %q", got, want)
	}
}

func TestUsagePlaceholderDoesNotDuplicateCommandPath(t *testing.T) {
	var out bytes.Buffer
	app := App{Name: "posse", Out: &out, Root: &Command{Subcommands: []*Command{{Name: "project", Subcommands: []*Command{{Name: "add", Usage: "$ project add [--name n]"}}}}}}
	if code := app.Run([]string{"project", "add", "--help"}); code != 0 {
		t.Fatalf("help exit=%d", code)
	}
	if !strings.HasPrefix(out.String(), "posse project add [--name n]\n") {
		t.Fatalf("help usage = %q", out.String())
	}
}

func TestVersionFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	app := App{
		Name:    "posse",
		Version: "v1.2.3",
		Out:     &out,
		ErrOut:  &errOut,
	}
	if code := app.Run([]string{"--version"}); code != 0 {
		t.Fatalf("exit code = %d, err=%s", code, errOut.String())
	}
	if out.String() != "posse v1.2.3\n" {
		t.Fatalf("version output = %q", out.String())
	}
	out.Reset()
	if code := app.Run([]string{"--version", "--json"}); code != 0 {
		t.Fatalf("JSON exit code = %d, err=%s", code, errOut.String())
	}
	if out.String() != "{\"name\":\"posse\",\"version\":\"v1.2.3\"}\n" {
		t.Fatalf("JSON version output = %q", out.String())
	}
}

func TestStructuredErrorsAndUsageExit(t *testing.T) {
	for _, testCase := range []struct {
		wantCode string
		wantExit int
		wantJSON bool
	}{
		{wantCode: "worker_limit", wantExit: 1},
		{wantCode: "usage", wantExit: 2, wantJSON: true},
	} {
		var out, errOut bytes.Buffer
		root := &Command{Handler: func(ctx *Context, args []string) error {
			if testCase.wantCode == "usage" {
				return Usage("bad input", "use a valid command")
			}
			return Failure("worker_limit", "worker cap reached", false, "wait for a Task")
		}}
		app := App{Name: "posse", Root: root, Out: &out, ErrOut: &errOut}
		args := []string{}
		if testCase.wantJSON {
			args = append(args, "--json")
		}
		if code := app.Run(args); code != testCase.wantExit {
			t.Fatalf("exit code = %d, want %d", code, testCase.wantExit)
		}
		if !strings.Contains(out.String(), testCase.wantCode) || errOut.Len() != 0 {
			t.Fatalf("error output stdout=%q stderr=%q, want code %q on stdout", out.String(), errOut.String(), testCase.wantCode)
		}
	}
}

func TestUnknownRootArgumentNamesUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	app := App{Name: "posse", Root: &Command{Handler: func(*Context, []string) error { return nil }}, Out: &out, ErrOut: &errOut}
	if code := app.Run([]string{"bogus"}); code != 2 {
		t.Fatalf("unknown command exit = %d", code)
	}
	if !strings.Contains(out.String(), "unknown command") || !strings.Contains(out.String(), "bogus") || errOut.Len() != 0 {
		t.Fatalf("unknown command output stdout=%q stderr=%q", out.String(), errOut.String())
	}
}
