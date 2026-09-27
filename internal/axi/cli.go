package axi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
)

type Error struct {
	Code      string   `json:"code"`
	Message   string   `json:"message"`
	Retryable bool     `json:"retryable"`
	Help      []string `json:"help"`
	ExitCode  int      `json:"-"`
	// Reported marks an error the command already explained in its own output.
	Reported bool `json:"-"`
}

func (e *Error) Error() string { return e.Message }

func Failure(code, message string, retryable bool, help ...string) *Error {
	return &Error{Code: code, Message: message, Retryable: retryable, Help: help, ExitCode: 1}
}

// Reported converts err into an Error that exits non-zero without printing it again.
func Reported(err error) *Error {
	var structured *Error
	if !errors.As(err, &structured) {
		structured = Failure("internal_error", err.Error(), false)
	}
	reported := *structured
	reported.Reported = true
	return &reported
}

func Usage(message string, help ...string) *Error {
	return &Error{Code: "usage", Message: message, Retryable: false, Help: help, ExitCode: 2}
}

type Command struct {
	Name        string
	Usage       string
	Summary     string
	Hidden      bool
	Handler     func(*Context, []string) error
	Subcommands []*Command
}

type Context struct {
	Context context.Context
	JSON    bool
	Out     io.Writer
	ErrOut  io.Writer
}

func (c *Context) Print(value any) error {
	return c.print(value, true)
}

func (c *Context) PrintWithoutHelp(value any) error {
	return c.print(value, false)
}

func (c *Context) print(value any, addDefaultHelp bool) error {
	normalized, err := normalize(value)
	if err != nil {
		return err
	}
	normalized = outputFields(normalized)
	if object, ok := normalized.(Object); ok && addDefaultHelp {
		found := false
		for _, field := range object {
			if field.Key == "help" {
				found = true
				break
			}
		}
		if !found {
			normalized = append(object, Field{Key: "help", Value: []any{}})
		}
	}
	if c.JSON {
		encoder := json.NewEncoder(c.Out)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(jsonValue(normalized))
	}
	encoded, err := Encode(normalized)
	if err != nil {
		return err
	}
	if encoded != "" {
		_, err = fmt.Fprintln(c.Out, encoded)
	}
	return err
}

// outputFields applies the CLI wire format after TOON normalization, so JSON and
// TOON see the same keys and timestamps without changing the store's numeric times.
func outputFields(value any) any {
	switch v := value.(type) {
	case Object:
		for i := range v {
			v[i].Key = snakeKey(v[i].Key)
			v[i].Value = outputTime(v[i].Key, outputFields(v[i].Value))
		}
	case Row:
		for i := range v {
			v[i].Key = snakeKey(v[i].Key)
			v[i].Value = outputTime(v[i].Key, outputFields(v[i].Value))
		}
	case []any:
		for i := range v {
			v[i] = outputFields(v[i])
		}
	}
	return value
}

func snakeKey(key string) string {
	var out []rune
	letters := []rune(strings.NewReplacer("HTTP", "Http", "JSON", "Json", "URL", "Url", "SHA", "Sha", "API", "Api", "PR", "Pr", "ID", "Id").Replace(key))
	for i, r := range letters {
		if unicode.IsUpper(r) {
			if i > 0 && letters[i-1] != '_' && (unicode.IsLower(letters[i-1]) || unicode.IsDigit(letters[i-1]) || (unicode.IsUpper(letters[i-1]) && i+1 < len(letters) && unicode.IsLower(letters[i+1]))) {
				out = append(out, '_')
			}
			out = append(out, unicode.ToLower(r))
		} else {
			out = append(out, r)
		}
	}
	return string(out)
}

func outputTime(key string, value any) any {
	if key != "at" && key != "updated" && !strings.HasSuffix(key, "_at") {
		return value
	}
	var ms int64
	switch v := value.(type) {
	case int:
		ms = int64(v)
	case int64:
		ms = v
	default:
		return value
	}
	if ms == 0 {
		return ""
	}
	// The date guard avoids treating ordinary counters as Unix milliseconds.
	if ms < 100000000000 {
		return value
	}
	return time.UnixMilli(ms).Local().Format(time.RFC3339Nano)
}

func jsonValue(value any) any {
	switch v := value.(type) {
	case Object:
		object := make(map[string]any, len(v))
		for _, field := range v {
			object[field.Key] = jsonValue(field.Value)
		}
		return object
	case Row:
		object := make(map[string]any, len(v))
		for _, field := range v {
			object[field.Key] = jsonValue(field.Value)
		}
		return object
	case []any:
		values := make([]any, len(v))
		for i, item := range v {
			values[i] = jsonValue(item)
		}
		return values
	default:
		return value
	}
}

type App struct {
	Name    string
	Version string
	Root    *Command
	Out     io.Writer
	ErrOut  io.Writer
	Context context.Context
}

func (a *App) Run(args []string) int {
	if a.Out == nil {
		a.Out = os.Stdout
	}
	if a.ErrOut == nil {
		a.ErrOut = os.Stderr
	}
	if a.Context == nil {
		a.Context = context.Background()
	}
	jsonOutput := false
	filtered := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--json" {
			jsonOutput = true
			continue
		}
		filtered = append(filtered, arg)
	}
	ctx := &Context{Context: a.Context, JSON: jsonOutput, Out: a.Out, ErrOut: a.ErrOut}
	if len(filtered) == 1 && filtered[0] == "--version" {
		if jsonOutput {
			fmt.Fprintf(a.Out, "{\"name\":%q,\"version\":%q}\n", a.Name, a.Version)
		} else {
			fmt.Fprintf(a.Out, "%s %s\n", a.Name, a.Version)
		}
		return 0
	}
	if command, path, ok := a.resolve(filtered); ok && hasFlag(filtered, "--help", "-h") {
		a.printHelp(command, path)
		return 0
	}
	if hasFlag(filtered, "--help", "-h") {
		a.printHelp(a.Root, nil)
		return 0
	}
	command, remaining, path, ok := a.findCommand(filtered)
	if !ok {
		return a.printError(ctx, Usage("unknown command", "Run `"+a.Name+" --help` to list commands"))
	}
	if len(path) == 0 && len(filtered) > 0 && !strings.HasPrefix(filtered[0], "-") {
		return a.printError(ctx, Usage(fmt.Sprintf("unknown command %q", filtered[0]), "Run `"+a.Name+" --help` to list commands"))
	}
	if command.Handler == nil {
		return a.printError(ctx, Usage("command requires a subcommand", "Run `"+a.Name+" "+strings.Join(path, " ")+" --help`"))
	}
	if err := command.Handler(ctx, remaining); err != nil {
		return a.printError(ctx, err)
	}
	return 0
}

func (a *App) resolve(args []string) (*Command, []string, bool) {
	command, _, path, ok := a.findCommand(args)
	return command, path, ok
}

func (a *App) findCommand(args []string) (*Command, []string, []string, bool) {
	command := a.Root
	path := []string{}
	index := 0
	for index < len(args) {
		arg := args[index]
		if arg == "--help" || arg == "-h" {
			index++
			continue
		}
		var next *Command
		for _, candidate := range command.Subcommands {
			if candidate.Name == arg {
				next = candidate
				break
			}
		}
		if next == nil {
			break
		}
		command = next
		path = append(path, arg)
		index++
	}
	if index < len(args) && (args[index] == "--help" || args[index] == "-h") {
		index++
	}
	return command, args[index:], path, true
}

func (a *App) printHelp(command *Command, path []string) {
	if command == nil {
		command = a.Root
	}
	name := a.Name
	if len(path) > 0 {
		name += " " + strings.Join(path, " ")
	}
	if command.Usage != "" {
		name = strings.ReplaceAll(command.Usage, "$", a.Name)
	}
	fmt.Fprintf(a.Out, "%s\n", name)
	if command.Summary != "" {
		fmt.Fprintf(a.Out, "\n%s\n", command.Summary)
	}
	if len(command.Subcommands) > 0 {
		fmt.Fprintln(a.Out, "\ncommands:")
		for _, child := range command.Subcommands {
			if child.Hidden {
				continue
			}
			fmt.Fprintf(a.Out, "  %-16s %s\n", child.Name, child.Summary)
		}
	}
	if command == a.Root {
		fmt.Fprintf(a.Out, "\nflags: --json, --help, --version\n")
		return
	}
	fmt.Fprintf(a.Out, "\nflags: --json, --help\n")
}

func (a *App) printError(ctx *Context, err error) int {
	var structured *Error
	if !errors.As(err, &structured) {
		structured = Failure("internal_error", err.Error(), false)
	}
	if structured.ExitCode == 0 {
		structured.ExitCode = 1
	}
	if structured.Reported {
		return structured.ExitCode
	}
	if ctx.JSON {
		encoder := json.NewEncoder(ctx.Out)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(structured)
	} else {
		help := "[]"
		if len(structured.Help) > 0 {
			parts := make([]string, len(structured.Help))
			for i, item := range structured.Help {
				parts[i] = quote(item)
			}
			help = "[" + strings.Join(parts, ",") + "]"
		}
		fmt.Fprintf(ctx.Out, "error{code,message,retryable,help}: %s,%s,%t,%s\n",
			quote(structured.Code), quote(structured.Message), structured.Retryable, help)
	}
	return structured.ExitCode
}

func hasFlag(args []string, flags ...string) bool {
	for _, arg := range args {
		for _, flag := range flags {
			if arg == flag {
				return true
			}
		}
	}
	return false
}
