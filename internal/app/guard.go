package app

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"mvdan.cc/sh/v3/syntax"
)

// guardExitBlocked is the exit code Claude Code and Codex PreToolUse hooks use
// to refuse a tool call; stderr is shown to the agent.
const guardExitBlocked = 2

// RunGuard is the `posse _guard` PreToolUse hook. Outside a Worker it allows
// everything. In a Worker it refuses a shell command that could change the
// Herdr session the Worker runs in, or run a posse build other than the
// installed one against the Worker's posse home. ADR 0007 states what it
// covers and what it cannot.
func (s *Service) RunGuard(stdin io.Reader, stderr io.Writer) int {
	data, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
	if err != nil {
		return 0
	}
	var input struct {
		CWD       string `json:"cwd"`
		ToolInput struct {
			Command json.RawMessage `json:"command"`
		} `json:"tool_input"`
	}
	if json.Unmarshal(data, &input) != nil || len(input.ToolInput.Command) == 0 {
		return 0
	}
	command := ""
	if json.Unmarshal(input.ToolInput.Command, &command) != nil {
		var argv []string
		if json.Unmarshal(input.ToolInput.Command, &argv) != nil {
			return 0
		}
		command = strings.Join(quoteArgv(argv), " ")
	}
	if strings.TrimSpace(command) == "" {
		return 0
	}
	home, err := s.homePath()
	if err != nil || !s.workerCaller(context.Background(), home) {
		return 0
	}
	cwd := input.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	self, _ := os.Executable()
	refused, reason := guardCommand(command, guardScope{env: os.Environ(), cwd: cwd, workerHome: home, posse: self})
	if !refused {
		return 0
	}
	fmt.Fprintf(stderr, "posse: refused %s from a posse Rider: %s. Riders never change the User's Herdr session or posse home. Run experiments against an isolated Herdr server and a temporary POSSE_HOME instead. Use a clean environment, for example `env -i HOME=/tmp/posse-e2e-lab/home XDG_CONFIG_HOME=/tmp/posse-e2e-lab/xdg POSSE_HOME=/tmp/posse-e2e-lab/posse PATH=/usr/local/bin:/usr/bin:/bin herdr server`, with your isolated Herdr binary on PATH.\n", reason.command, reason.why)
	return guardExitBlocked
}

type guardReason struct {
	command string
	why     string
}

// guardScope is what a guarded command runs against.
type guardScope struct {
	env        []string
	cwd        string
	workerHome string // the posse home this process is a Worker of
	posse      string // the posse executable running the guard
}

// guardCommand reports whether a shell command may change the Herdr session
// reached from scope.env, or run another posse build against the Worker home.
func guardCommand(script string, scope guardScope) (bool, guardReason) {
	state := newShellEnv(scope.env)
	g := &guard{realSocket: herdr.SocketPath(scope.env), userHome: state.values["HOME"], workerHome: scope.workerHome, posse: scope.posse, visited: map[string]bool{}}
	state.cwd = scope.cwd
	return g.check(state, script, 0)
}

// guardHerdrCommand is guardCommand for a Worker of no posse home.
func guardHerdrCommand(script string, env []string) (bool, guardReason) {
	cwd, _ := os.Getwd()
	return guardCommand(script, guardScope{env: env, cwd: cwd})
}

const (
	guardMaxDepth    = 6
	guardMaxFileSize = 1 << 20
)

type guard struct {
	realSocket string
	userHome   string
	workerHome string
	posse      string
	visited    map[string]bool
}

func refuse(command, why string) (bool, guardReason) {
	return true, guardReason{command: "`" + firstLine(command) + "`", why: why}
}

func (g *guard) check(e *shellEnv, script string, depth int) (bool, guardReason) {
	if depth > guardMaxDepth {
		if g.mentionsHerdr(script) {
			return refuse(script, "it nests commands too deeply for posse to check")
		}
		return false, guardReason{}
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(script), "")
	if err != nil {
		if g.mentionsHerdr(script) {
			return refuse(script, "posse could not parse this command, and it mentions Herdr")
		}
		return false, guardReason{}
	}
	return g.statements(e, script, file.Stmts, depth)
}

func (g *guard) statements(e *shellEnv, script string, stmts []*syntax.Stmt, depth int) (bool, guardReason) {
	for _, stmt := range stmts {
		if refused, reason := g.statement(e, script, stmt, depth); refused {
			return true, reason
		}
	}
	return false, guardReason{}
}

func (g *guard) statement(e *shellEnv, script string, stmt *syntax.Stmt, depth int) (bool, guardReason) {
	switch cmd := stmt.Cmd.(type) {
	case *syntax.IfClause:
		if len(cmd.Cond) == 0 {
			return g.statements(e, script, cmd.Then, depth)
		}
		if refused, reason := g.statements(e, script, cmd.Cond, depth); refused {
			return true, reason
		}
		before := e.clone()
		thenEnv := e.clone()
		if refused, reason := g.statements(thenEnv, script, cmd.Then, depth); refused {
			return true, reason
		}
		elseEnv := before.clone()
		if cmd.Else != nil {
			if refused, reason := g.statement(elseEnv, script, &syntax.Stmt{Cmd: cmd.Else}, depth); refused {
				return true, reason
			}
		}
		e.merge(thenEnv, elseEnv)
		return false, guardReason{}
	case *syntax.FuncDecl:
		body := e.clone()
		if refused, reason := g.statement(body, script, cmd.Body, depth); refused {
			return true, reason
		}
		e.merge(e, body)
		return false, guardReason{}
	case *syntax.Block:
		return g.statements(e, script, cmd.Stmts, depth)
	case *syntax.Subshell:
		return g.statements(e.clone(), script, cmd.Stmts, depth)
	case *syntax.BinaryCmd:
		left := e.clone()
		if refused, reason := g.statement(left, script, cmd.X, depth); refused {
			return true, reason
		}
		right := left.clone()
		if refused, reason := g.statement(right, script, cmd.Y, depth); refused {
			return true, reason
		}
		e.merge(left, right, e)
		return false, guardReason{}
	case *syntax.WhileClause:
		body := e.clone()
		if refused, reason := g.statements(body, script, cmd.Cond, depth); refused {
			return true, reason
		}
		if refused, reason := g.statements(body, script, cmd.Do, depth); refused {
			return true, reason
		}
		e.merge(e, body)
		return false, guardReason{}
	case *syntax.ForClause:
		body := e.clone()
		if refused, reason := g.statements(body, script, cmd.Do, depth); refused {
			return true, reason
		}
		e.merge(e, body)
		return false, guardReason{}
	case *syntax.CaseClause:
		paths := []*shellEnv{e.clone()} // no pattern need match
		for _, item := range cmd.Items {
			branch := e.clone()
			if refused, reason := g.statements(branch, script, item.Stmts, depth); refused {
				return true, reason
			}
			paths = append(paths, branch)
		}
		e.merge(paths...)
		return false, guardReason{}
	}
	// Command substitutions are evaluated by the shell before the outer call.
	// They get a copy of the environment, as do pipelines and function bodies.
	refused, reason := false, guardReason{}
	syntax.Walk(stmt, func(node syntax.Node) bool {
		if refused {
			return false
		}
		switch node := node.(type) {
		case *syntax.DeclClause:
			if node.Variant != nil && node.Variant.Value == "export" {
				for _, assign := range node.Args {
					e.assign(script, assign, true)
				}
			}
		case *syntax.CallExpr:
			refused, reason = g.call(e, script, node, stmt, depth)
		}
		return !refused
	})
	return refused, reason
}

// merge retains only environment facts established on every possible path.
func (e *shellEnv) merge(paths ...*shellEnv) {
	first := paths[0]
	for _, name := range []string{"HERDR_SOCKET_PATH", "HERDR_CONFIG_PATH", "HERDR_SESSION", "XDG_CONFIG_HOME", "HOME", "POSSE_HOME", "PATH"} {
		common := true
		for _, path := range paths[1:] {
			if first.values[name] != path.values[name] || first.exported[name] != path.exported[name] || first.unknown[name] != path.unknown[name] {
				common = false
			}
		}
		if !common {
			e.unknown[name] = true
			continue
		}
		if value, ok := first.values[name]; ok {
			e.values[name] = value
		} else {
			delete(e.values, name)
		}
		if first.exported[name] {
			e.exported[name] = true
		} else {
			delete(e.exported, name)
		}
		if first.unknown[name] {
			e.unknown[name] = true
		} else {
			delete(e.unknown, name)
		}
	}
	e.cwd = first.cwd
	for _, path := range paths[1:] {
		if e.cwd != path.cwd {
			e.cwd = ""
		}
	}
}

// call evaluates one simple command. Bare assignments, `unset` and `cd`
// change the tracked state; everything else is checked by command.
func (g *guard) call(e *shellEnv, script string, call *syntax.CallExpr, stmt *syntax.Stmt, depth int) (bool, guardReason) {
	if len(call.Args) == 0 {
		for _, assign := range call.Assigns {
			e.assign(script, assign, false)
		}
		return false, guardReason{}
	}
	child := e.clone()
	for _, assign := range call.Assigns {
		child.assign(script, assign, true)
	}
	args := make([]string, len(call.Args))
	literal := make([]bool, len(call.Args))
	for index, word := range call.Args {
		args[index], literal[index] = e.word(script, word)
		if !literal[index] {
			// Keep the source of an unresolved env assignment so envPrefix can
			// track which variable is unknown without losing the next program.
			args[index] = sourceText(script, word)
		}
	}
	raw := sourceText(script, call)
	if literal[0] {
		switch args[0] {
		case "unset":
			for index := 1; index < len(args); index++ {
				if literal[index] && !strings.HasPrefix(args[index], "-") {
					e.unset(args[index])
				}
			}
			return false, guardReason{}
		case "cd", "pushd":
			if len(args) > 1 && literal[1] {
				e.cwd = e.path(args[1])
			} else {
				e.cwd = ""
			}
			return false, guardReason{}
		}
	}
	stdin := ""
	if stmt != nil {
		for _, redirect := range stmt.Redirs {
			if redirect.Hdoc != nil {
				stdin += sourceText(script, redirect.Hdoc) + "\n"
			}
			if redirect.Op == syntax.WordHdoc && redirect.Word != nil {
				value, _ := e.word(script, redirect.Word)
				stdin += value + "\n"
			}
		}
	}
	if stdin == "" {
		// A program reading a pipe may receive anything the script produces.
		stdin = script
	}
	return g.command(child, args, literal, raw, stdin, depth)
}

func (g *guard) command(e *shellEnv, args []string, literal []bool, raw, stdin string, depth int) (bool, guardReason) {
	for len(args) > 0 {
		if !literal[0] || args[0] == "" {
			if g.mentionsHerdr(raw) {
				return refuse(raw, "posse cannot tell which program this runs, and it mentions Herdr")
			}
			return false, guardReason{}
		}
		program := args[0]
		name := filepath.Base(program)
		switch {
		case name == "command" && len(args) == 3 && literal[1] && literal[2] && (args[1] == "-v" || args[1] == "-V"):
			// These shell-builtin forms inspect a single program name; they do
			// not execute it. All other command forms still unwrap and check it.
			return false, guardReason{}
		case wrapperOptions[name] != nil:
			args, literal = skipWrapper(name, args[1:], literal[1:])
			continue
		case name == "env":
			var split string
			args, literal, split = e.envPrefix(args[1:], literal[1:])
			if split != "" {
				return g.check(e, split+" "+strings.Join(quoteArgv(args), " "), depth+1)
			}
			continue
		case name == "find":
			for index, arg := range args {
				if arg == "-exec" || arg == "-execdir" || arg == "-ok" || arg == "-okdir" {
					end := index + 1
					for end < len(args) && args[end] != ";" && args[end] != "+" {
						end++
					}
					return g.command(e, args[index+1:end], literal[index+1:end], raw, stdin, depth)
				}
			}
			return false, guardReason{}
		case name == "herdr":
			return g.herdr(e, args, literal)
		case name == "curl":
			return g.curl(e, args, literal, raw)
		case name == "git" && g.workerHome != "":
			if gitRemoteWrite(e, args[1:], literal[1:]) {
				return refuse(raw, "Riders must push their Task branch through `posse publish`, not `git push`")
			}
			return false, guardReason{}
		case name == "ln" && len(args) >= 3 && sameSocket(args[len(args)-2], g.realSocket):
			return refuse(raw, "it aliases the Rider's Herdr socket")
		case name == "make" || name == "gmake":
			code := ""
			if strings.Contains(stdin, ">") {
				code = stdin
			}
			explicit := false
			for index := 1; index+1 < len(args); index++ {
				if args[index] == "-f" && literal[index+1] {
					explicit = true
					if contents, ok := g.readFile(e, args[index+1]); ok {
						code += "\n" + contents
					}
				}
			}
			if !explicit {
				for _, file := range []string{"GNUmakefile", "makefile", "Makefile"} {
					if contents, ok := g.readFile(e, file); ok {
						code += "\n" + contents
						break
					}
				}
			}
			return g.opaque(e, raw, code)
		case strings.HasPrefix(name, "posse") && !strings.HasSuffix(name, ".sh"):
			return g.posseBuild(e, program, raw)
		case name == "go" && len(args) > 2 && args[1] == "run":
			for _, arg := range args[2:] {
				if strings.HasSuffix(strings.TrimSuffix(arg, "/"), "cmd/posse") {
					return g.posseBuild(e, "", raw)
				}
			}
			return false, guardReason{}
		case shells[name]:
			return g.shell(e, args, literal, raw, stdin, depth)
		case name == "source" || name == ".":
			if len(args) < 2 || !literal[1] {
				return g.opaque(e, raw, raw)
			}
			return g.script(e, args[1], raw, stdin, depth)
		case name == "eval":
			return g.check(e.clone(), strings.Join(args[1:], " "), depth+1)
		case interpreters.MatchString(name):
			return g.interpreter(e, args, literal, raw, stdin)
		case socketTools[name]:
			return g.opaque(e, raw, strings.Join(args, " "))
		case strings.Contains(program, "/"):
			if g.isPosseBinary(e, program) {
				return g.posseBuild(e, program, raw)
			}
			return g.executable(e, program, args, literal, raw, stdin, depth)
		}
		if resolved := e.lookPath(program); resolved != "" && g.isPosseBinary(e, resolved) {
			return g.posseBuild(e, resolved, raw)
		}
		return false, guardReason{}
	}
	return false, guardReason{}
}

// wrapperOptions lists programs that run the rest of their arguments as a
// command, with the options of each that take a value.
var wrapperOptions = map[string]map[string]bool{
	"command": {}, "exec": {"-a": true}, "nohup": {}, "builtin": {}, "time": {"-f": true, "-o": true},
	"nice": {"-n": true}, "ionice": {"-c": true, "-n": true}, "setsid": {}, "stdbuf": {"-i": true, "-o": true, "-e": true},
	"sudo": {"-u": true, "-g": true, "-C": true, "-D": true}, "doas": {"-u": true}, "chrt": {}, "taskset": {},
	"timeout": {"-s": true, "-k": true, "--signal": true, "--kill-after": true}, "watch": {"-n": true, "-d": false},
	"xargs": {"-I": true, "-i": false, "-n": true, "-P": true, "-L": true, "-d": true, "-E": true, "-s": true, "-a": true},
	"flock": {"-w": true, "-E": true}, "unbuffer": {}, "script": {},
}

// skipWrapper drops a wrapper's options and positional operands, returning
// the command it runs.
func skipWrapper(name string, args []string, literal []bool) ([]string, []bool) {
	options := wrapperOptions[name]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		if args[0] == "--" {
			return args[1:], literal[1:]
		}
		takesValue := options[args[0]] && !strings.Contains(args[0], "=")
		args, literal = args[1:], literal[1:]
		if takesValue && len(args) > 0 {
			args, literal = args[1:], literal[1:]
		}
	}
	switch name {
	case "timeout", "chrt", "flock":
		if len(args) > 0 {
			args, literal = args[1:], literal[1:]
		}
	case "taskset":
		if len(args) > 0 {
			args, literal = args[1:], literal[1:]
		}
	case "script":
		for index := 0; index+1 < len(args); index++ {
			if args[index] == "-c" {
				return []string{"sh", "-c", args[index+1]}, []bool{true, true, literal[index+1]}
			}
		}
		return nil, nil
	}
	return args, literal
}

var (
	shells       = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true, "ash": true, "busybox": true}
	interpreters = regexp.MustCompile(`^(python[0-9.]*|pypy[0-9.]*|node|nodejs|deno|bun|perl[0-9.]*|ruby[0-9.]*|php[0-9.]*|lua[0-9.]*|luajit|tclsh[0-9.]*|osascript|Rscript|julia|expect|awk|gawk|mawk|nawk)$`)
	socketTools  = map[string]bool{"socat": true, "nc": true, "ncat": true, "netcat": true, "curl": true, "websocat": true, "wscat": true}
)

func (g *guard) shell(e *shellEnv, args []string, literal []bool, raw, stdin string, depth int) (bool, guardReason) {
	start := 1
	if filepath.Base(args[0]) == "busybox" {
		if len(args) < 2 || !shells[args[1]] {
			return false, guardReason{}
		}
		start = 2
	}
	for index := start; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-c" || strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg, "c"):
			if index+1 >= len(args) || !literal[index+1] {
				return g.opaque(e, raw, raw)
			}
			return g.check(e.clone(), args[index+1], depth+1)
		case arg == "-o" || arg == "+o" || arg == "--rcfile" || arg == "--init-file":
			index++
		case strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "+"):
		default:
			if !literal[index] {
				return g.opaque(e, raw, raw)
			}
			return g.script(e, arg, raw, stdin, depth)
		}
	}
	return g.check(e.clone(), stdin, depth+1)
}

// script checks a shell script file the command runs.
func (g *guard) script(e *shellEnv, name, raw, source string, depth int) (bool, guardReason) {
	contents, ok := g.readFile(e, name)
	if !ok {
		// A file written earlier in this tool call does not exist yet.
		if strings.Contains(source, ">") {
			return g.opaque(e, raw, source)
		}
		return false, guardReason{}
	}
	return g.check(e.clone(), contents, depth+1)
}

func (g *guard) interpreter(e *shellEnv, args []string, literal []bool, raw, stdin string) (bool, guardReason) {
	code := ""
	for index := 1; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-c" || arg == "-e" || arg == "-E" || arg == "-r" || arg == "--eval" || arg == "-p" || arg == "--print" || arg == "-m":
			if index+1 < len(args) {
				code += args[index+1] + "\n"
				index++
			}
		case strings.HasPrefix(arg, "-"):
		default:
			if contents, ok := g.readFile(e, arg); ok {
				code += contents + "\n"
			} else {
				code += arg + "\n"
			}
			index = len(args)
		}
	}
	if strings.TrimSpace(code) == "" {
		code = stdin
	}
	if len(args) > 0 && pythonInterpreter(filepath.Base(args[0])) && provenPythonInspection(code) {
		return false, guardReason{}
	}
	return g.opaque(e, raw, raw+"\n"+code)
}

// executable checks a program named by path: a text script is read and
// checked by its interpreter; a binary is opaque.
func (g *guard) executable(e *shellEnv, program string, args []string, literal []bool, raw, stdin string, depth int) (bool, guardReason) {
	contents, ok := g.readFile(e, program)
	if !ok {
		if strings.Contains(stdin, ">") {
			return g.opaque(e, raw, stdin)
		}
		return false, guardReason{}
	}
	firstLine, _, _ := strings.Cut(contents, "\n")
	if !strings.HasPrefix(firstLine, "#!") {
		if bytes.IndexByte([]byte(contents), 0) >= 0 {
			return false, guardReason{}
		}
		return g.check(e.clone(), contents, depth+1)
	}
	fields := strings.Fields(strings.TrimPrefix(firstLine, "#!"))
	if len(fields) == 0 {
		return false, guardReason{}
	}
	interpreter := filepath.Base(fields[0])
	if interpreter == "env" && len(fields) > 1 {
		interpreter = filepath.Base(fields[len(fields)-1])
	}
	if shells[interpreter] {
		return g.check(e.clone(), contents, depth+1)
	}
	return g.opaque(e, raw, raw+"\n"+contents)
}

func (g *guard) readFile(e *shellEnv, name string) (string, bool) {
	path := e.path(name)
	if path == "" || g.visited[path] {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > guardMaxFileSize {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	g.visited[path] = true
	return string(data), true
}

// opaque checks code posse cannot follow (an interpreter, a socket tool, an
// unknown program): it is refused when it refers to Herdr or its socket,
// unless its environment points Herdr at another server and the code does
// not name the session's socket itself.
func (g *guard) opaque(e *shellEnv, raw, code string) (bool, guardReason) {
	code = strings.NewReplacer(`\t`, "\t", `\n`, "\n").Replace(code)
	if !g.mentionsHerdr(code) {
		return false, guardReason{}
	}
	if g.namesRealSocket(code) {
		return refuse(raw, "it names the socket of the Herdr session this Rider runs in")
	}
	if socket, known := e.socket(); known && g.realSocket != "" && !sameSocket(socket, g.realSocket) {
		return false, guardReason{}
	}
	return refuse(raw, "it reaches Herdr in a way posse cannot check, and its environment still points at the Rider's Herdr session")
}

var herdrReference = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])herdr($|[^A-Za-z0-9_-])|herdr\.sock|HERDR_SOCKET_PATH|HERDR_BIN_PATH`)

func (g *guard) mentionsHerdr(text string) bool {
	return herdrReference.MatchString(text)
}

func (g *guard) namesRealSocket(text string) bool {
	if g.realSocket == "" {
		return false
	}
	if strings.Contains(text, g.realSocket) {
		return true
	}
	if home := g.userHome; home != "" && strings.HasPrefix(g.realSocket, home+"/") {
		relative := strings.TrimPrefix(g.realSocket, home)
		if strings.Contains(text, "~"+relative) || strings.Contains(text, "$HOME"+relative) || strings.Contains(text, "${HOME}"+relative) {
			return true
		}
	}
	return strings.HasSuffix(g.realSocket, "/.config/herdr/herdr.sock") && strings.Contains(text, ".config/herdr/herdr.sock")
}

// sameSocket catches hard links as well as symlinks and identical paths.
func sameSocket(a, b string) bool {
	if samePath(a, b) {
		return true
	}
	left, err := os.Stat(a)
	if err != nil {
		return false
	}
	right, err := os.Stat(b)
	return err == nil && os.SameFile(left, right)
}

// isPosseBinary recognizes renamed Go builds of posse by embedded build info.
// Other opaque native programs remain outside a shell hook's authority.
func (g *guard) isPosseBinary(e *shellEnv, program string) bool {
	path := e.path(program)
	if path == "" {
		return false
	}
	info, err := buildinfo.ReadFile(path)
	return err == nil && info.Path == "github.com/thanhbinh1905/posse/cmd/posse"
}

// posseBuild refuses a posse executable other than the one running the guard
// against the Worker home: an older or foreign build would not enforce Worker
// mode, and could migrate the database.
func (g *guard) posseBuild(e *shellEnv, program, raw string) (bool, guardReason) {
	if g.workerHome == "" {
		return false, guardReason{}
	}
	if program != "" && g.posse != "" {
		resolved := program
		if !strings.Contains(program, "/") {
			resolved = e.lookPath(program)
		} else {
			resolved = e.path(program)
		}
		if resolved != "" && samePath(resolved, g.posse) {
			return false, guardReason{}
		}
	}
	home, known := e.posseHome()
	if known && !samePath(home, g.workerHome) {
		return false, guardReason{}
	}
	return refuse(raw, "it runs a posse build other than the installed one against the Rider's posse home")
}

func (g *guard) curl(e *shellEnv, args []string, literal []bool, raw string) (bool, guardReason) {
	for index := 1; index < len(args); index++ {
		arg := args[index]
		if !literal[index] {
			if g.mentionsHerdr(raw) {
				return g.opaque(e, raw, strings.Join(args, " "))
			}
			continue
		}
		if arg == "--" {
			break
		}
		socket := ""
		switch {
		case arg == "--unix-socket":
			index++
			if index >= len(args) || !literal[index] || args[index] == "" {
				return refuse(raw, "posse cannot tell which Herdr server curl reaches")
			}
			socket = args[index]
		case strings.HasPrefix(arg, "--unix-socket="):
			socket = strings.TrimPrefix(arg, "--unix-socket=")
		}
		if socket == "" {
			continue
		}
		socket = e.path(socket)
		if socket == "" || g.realSocket == "" {
			return refuse(raw, "posse cannot tell which Herdr server curl reaches")
		}
		if sameSocket(socket, g.realSocket) {
			return refuse(raw, "it targets the Herdr session this Rider runs in")
		}
	}
	return false, guardReason{}
}

func (g *guard) herdr(e *shellEnv, args []string, literal []bool) (bool, guardReason) {
	display := strings.Join(args, " ")
	for index := range args {
		if !literal[index] {
			return refuse(display, "posse cannot tell what this herdr command does")
		}
	}
	positionals := []string{}
	targetKnown := true
	// Help is read-only only before the `--` delimiter. After `--`,
	// even --help may be a label or pane command argument.
	for index := 1; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--":
			positionals = append(positionals, args[index+1:]...)
			index = len(args)
		case readOnlyHerdrFlags[arg]:
			return false, guardReason{}
		case arg == "--session" || arg == "--remote" || strings.HasPrefix(arg, "--session=") || strings.HasPrefix(arg, "--remote="):
			targetKnown = false
			if !strings.Contains(arg, "=") {
				index++
			}
		case strings.HasPrefix(arg, "-"):
		default:
			positionals = append(positionals, arg)
		}
	}
	if readOnlyHerdr(positionals) {
		return false, guardReason{}
	}
	socket, known := e.socket()
	if !known || !targetKnown {
		return refuse(display, "posse cannot tell which Herdr server it reaches")
	}
	if g.realSocket == "" || sameSocket(socket, g.realSocket) {
		if g.realSocket != "" && e.values["HERDR_SOCKET_PATH"] == "" {
			return refuse(display, "the default socket reaches the User's Herdr server")
		}
		return refuse(display, "it changes the Herdr session this Rider runs in")
	}
	return false, guardReason{}
}

var readOnlyHerdrFlags = map[string]bool{"--help": true, "-h": true, "--version": true, "-V": true, "--skill": true, "--default-config": true}

// readOnlyHerdr reports whether herdr positionals name a command that only
// reads state.
func readOnlyHerdr(positionals []string) bool {
	if len(positionals) == 0 {
		return false
	}
	switch positionals[0] {
	case "status", "help", "completion":
		return true
	case "api":
		return len(positionals) > 1 && (positionals[1] == "schema" || positionals[1] == "snapshot")
	}
	if len(positionals) == 1 {
		return false
	}
	switch positionals[1] {
	case "list", "get", "read", "status", "explain", "wait-output":
		return true
	}
	return false
}

var (
	validName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	simpleVar = regexp.MustCompile(`^\$(\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))$`)
)

var provenPythonInspections = func() []*regexp.Regexp {
	literal := `(?:'[^'\\\r\n]*(?:\\.[^'\\\r\n]*)*'|"[^"\\\r\n]*(?:\\.[^"\\\r\n]*)*")`
	return []*regexp.Regexp{
		regexp.MustCompile(`(?s)^\s*print\s*\(\s*` + literal + `\s*\)\s*;?\s*$`),
		regexp.MustCompile(`(?s)^\s*from\s+pathlib\s+import\s+Path\s*[;\n]\s*print\s*\(\s*Path\s*\(\s*` + literal + `\s*\)\s*\.\s*read_text\s*\(\s*\)\s*\)\s*;?\s*$`),
		regexp.MustCompile(`(?s)^\s*import\s+importlib\.metadata\s*;\s*print\s*\(\s*importlib\.metadata\.version\s*\(\s*` + literal + `\s*\)\s*\)\s*;?\s*$`),
		regexp.MustCompile(`(?s)^\s*from\s+importlib\.metadata\s+import\s+version\s*;\s*print\s*\(\s*version\s*\(\s*` + literal + `\s*\)\s*\)\s*;?\s*$`),
		regexp.MustCompile(`(?s)^\s*import\s+sys\s*;\s*print\s*\(\s*sys\.version(?:_info)?\s*\)\s*;?\s*$`),
		regexp.MustCompile(`(?s)^\s*import\s+sys\s*;\s*print\s*\(\s*` + literal + `\s*,\s*sys\.version(?:_info)?\s*\)\s*;?\s*$`),
	}
}()

func pythonInterpreter(name string) bool {
	return strings.HasPrefix(name, "python") || strings.HasPrefix(name, "pypy")
}

// provenPythonInspection recognizes a few single-purpose read-only probes.
// Other interpreter programs remain opaque and are checked conservatively.
func provenPythonInspection(code string) bool {
	code = strings.TrimSpace(code)
	for _, pattern := range provenPythonInspections {
		if pattern.MatchString(code) {
			return true
		}
	}
	return false
}

// shellEnv tracks the environment and working directory a command will see,
// as far as the script shows them. A variable set from an expansion posse
// cannot resolve is unknown; an unknown working directory is empty.
type shellEnv struct {
	values   map[string]string
	exported map[string]bool
	unknown  map[string]bool
	cwd      string
}

func newShellEnv(env []string) *shellEnv {
	state := &shellEnv{values: map[string]string{}, exported: map[string]bool{}, unknown: map[string]bool{}}
	for _, entry := range env {
		if name, value, ok := strings.Cut(entry, "="); ok {
			state.values[name], state.exported[name] = value, true
		}
	}
	return state
}

func (e *shellEnv) clone() *shellEnv {
	copied := &shellEnv{values: map[string]string{}, exported: map[string]bool{}, unknown: map[string]bool{}, cwd: e.cwd}
	for key, value := range e.values {
		copied.values[key] = value
	}
	for key := range e.exported {
		copied.exported[key] = true
	}
	for key := range e.unknown {
		copied.unknown[key] = true
	}
	return copied
}

// envPrefix applies `env [-i] [-u NAME] [-C DIR] [-S STRING] [NAME=VALUE]...`
// and returns the command that follows, and the -S string when there is one.
func (e *shellEnv) envPrefix(args []string, literal []bool) ([]string, []bool, string) {
	split := ""
	for len(args) > 0 {
		arg := args[0]
		if !literal[0] {
			name, _, assignment := strings.Cut(arg, "=")
			if !assignment || !validName.MatchString(name) {
				break
			}
			// The value is dynamic, but an unrelated assignment does not
			// make a known Herdr socket or the following program unknown.
			delete(e.values, name)
			e.unknown[name], e.exported[name] = true, true
			args, literal = args[1:], literal[1:]
			continue
		}
		switch {
		case arg == "-i" || arg == "--ignore-environment" || arg == "-":
			e.values, e.exported, e.unknown = map[string]string{}, map[string]bool{}, map[string]bool{}
		case arg == "-u" || arg == "--unset":
			if len(args) > 1 {
				e.unset(args[1])
				args, literal = args[1:], literal[1:]
			}
		case strings.HasPrefix(arg, "--unset="):
			e.unset(strings.TrimPrefix(arg, "--unset="))
		case strings.HasPrefix(arg, "-u") && len(arg) > 2:
			e.unset(arg[2:])
		case arg == "-C" || arg == "--chdir":
			if len(args) > 1 {
				e.cwd = e.path(args[1])
				args, literal = args[1:], literal[1:]
			}
		case arg == "-S" || arg == "--split-string":
			if len(args) > 1 {
				split = args[1]
				args, literal = args[1:], literal[1:]
			}
		case strings.HasPrefix(arg, "-S") && len(arg) > 2:
			split = arg[2:]
		case strings.HasPrefix(arg, "-"):
		case strings.Contains(arg, "=") && validName.MatchString(arg[:strings.Index(arg, "=")]):
			name, value, _ := strings.Cut(arg, "=")
			e.values[name], e.exported[name] = value, true
			delete(e.unknown, name)
		default:
			return args, literal, split
		}
		args, literal = args[1:], literal[1:]
	}
	return args, literal, split
}

func (e *shellEnv) assign(script string, assign *syntax.Assign, export bool) {
	if assign.Name == nil {
		return
	}
	name := assign.Name.Value
	if export {
		e.exported[name] = true
	}
	if assign.Naked {
		return
	}
	if assign.Value == nil {
		e.values[name] = ""
		delete(e.unknown, name)
		return
	}
	value, ok := e.word(script, assign.Value)
	if !ok || assign.Append || assign.Array != nil {
		e.unknown[name] = true
		return
	}
	e.values[name] = value
	delete(e.unknown, name)
}

func (e *shellEnv) unset(name string) {
	delete(e.values, name)
	delete(e.exported, name)
	delete(e.unknown, name)
}

// path resolves a file name against the tracked working directory and HOME.
func (e *shellEnv) path(name string) string {
	if strings.HasPrefix(name, "~/") && !e.unknown["HOME"] && e.values["HOME"] != "" {
		name = filepath.Join(e.values["HOME"], name[2:])
	}
	if filepath.IsAbs(name) {
		return filepath.Clean(name)
	}
	if e.cwd == "" {
		return ""
	}
	return filepath.Join(e.cwd, name)
}

// lookPath resolves a program name through the tracked PATH.
func (e *shellEnv) lookPath(name string) string {
	if e.unknown["PATH"] {
		return ""
	}
	for _, directory := range filepath.SplitList(e.values["PATH"]) {
		if directory == "" {
			continue
		}
		candidate := e.path(filepath.Join(directory, name))
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	if path, err := exec.LookPath(name); err == nil && e.values["PATH"] == os.Getenv("PATH") {
		return path
	}
	return ""
}

// posseHome resolves the posse home a posse command uses with this
// environment.
func (e *shellEnv) posseHome() (string, bool) {
	if e.unknown["POSSE_HOME"] || e.unknown["HOME"] {
		return "", false
	}
	if home := e.values["POSSE_HOME"]; home != "" && e.exported["POSSE_HOME"] {
		return filepath.Clean(home), true
	}
	if e.values["HOME"] == "" {
		return "", false
	}
	return filepath.Join(e.values["HOME"], ".posse"), true
}

// word returns a word's value when it is literal, quoted text or a plain
// $NAME / ${NAME} of a known variable.
func (e *shellEnv) word(script string, word *syntax.Word) (string, bool) {
	var builder strings.Builder
	for _, part := range word.Parts {
		value, ok := e.part(script, part)
		if !ok {
			return "", false
		}
		builder.WriteString(value)
	}
	return builder.String(), true
}

func (e *shellEnv) part(script string, part syntax.WordPart) (string, bool) {
	switch part := part.(type) {
	case *syntax.Lit:
		return part.Value, true
	case *syntax.SglQuoted:
		return part.Value, !part.Dollar
	case *syntax.DblQuoted:
		var builder strings.Builder
		for _, inner := range part.Parts {
			value, ok := e.part(script, inner)
			if !ok {
				return "", false
			}
			builder.WriteString(value)
		}
		return builder.String(), true
	case *syntax.CmdSubst:
		if sourceText(script, part) == "$(pwd)" && e.cwd != "" {
			return e.cwd, true
		}
		return "", false
	case *syntax.ParamExp:
		match := simpleVar.FindStringSubmatch(sourceText(script, part))
		if match == nil {
			return "", false
		}
		name := match[2] + match[3]
		if e.unknown[name] {
			return "", false
		}
		return e.values[name], true
	}
	return "", false
}

// socket resolves the Herdr socket a herdr command reaches with this
// environment, the way the herdr CLI does.
func (e *shellEnv) socket() (string, bool) {
	if e.unknown["HERDR_SOCKET_PATH"] {
		return "", false
	}
	if e.values["HERDR_SOCKET_PATH"] == "" || !e.exported["HERDR_SOCKET_PATH"] {
		if e.unknown["HERDR_CONFIG_PATH"] || e.unknown["HERDR_SESSION"] {
			return "", false
		}
		if e.values["HERDR_CONFIG_PATH"] == "" || !e.exported["HERDR_CONFIG_PATH"] {
			if e.unknown["XDG_CONFIG_HOME"] {
				return "", false
			}
			if (e.values["XDG_CONFIG_HOME"] == "" || !e.exported["XDG_CONFIG_HOME"]) && e.unknown["HOME"] {
				return "", false
			}
		}
	}
	env := []string{}
	for name, value := range e.values {
		if e.exported[name] {
			env = append(env, name+"="+value)
		}
	}
	return herdr.SocketPath(env), true
}

func sourceText(script string, node syntax.Node) string {
	start, end := int(node.Pos().Offset()), int(node.End().Offset())
	if start < 0 || end > len(script) || start >= end {
		return ""
	}
	return script[start:end]
}

func quoteArgv(argv []string) []string {
	quoted := make([]string, len(argv))
	for index, arg := range argv {
		quoted[index] = shellQuote(arg)
	}
	return quoted
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return line
}
