package research

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// ClaudeReadOnlyTools are Claude Code tools a research child may call without
// further checks. Bash is admitted per command by ReadOnlyCommand.
var ClaudeReadOnlyTools = []string{"Glob", "Grep", "LS", "NotebookRead", "Read", "TodoWrite", "ToolSearch", "WebFetch", "WebSearch"}

// claudeBuiltinReadOnlyAgents are built-in agent types whose documented tool
// set is read-only. A same-named project or user definition overrides them and
// is checked like any custom agent.
var claudeBuiltinReadOnlyAgents = map[string][]string{
	"Explore": {"Bash", "Glob", "Grep", "LS", "NotebookRead", "Read", "WebFetch", "WebSearch"},
	"Plan":    {"Bash", "Glob", "Grep", "LS", "NotebookRead", "Read", "WebFetch", "WebSearch"},
}

// PiReadOnlyTools is the explicit Pi tool loadout of a research child.
var PiReadOnlyTools = []string{"read", "grep", "find", "ls"}

// ClaudeToolAllowed reports whether a registered research child may call tool.
// Bash is decided per command by ReadOnlyCommand.
func ClaudeToolAllowed(tool string) bool {
	return slices.Contains(ClaudeReadOnlyTools, tool)
}

// ClaudeLaunch is the Agent tool input a parent sent.
type ClaudeLaunch struct {
	SubagentType string `json:"subagent_type"`
	Description  string `json:"description"`
	Prompt       string `json:"prompt"`
	Model        string `json:"model"`
	Isolation    string `json:"isolation"`
	Mode         string `json:"mode"`
	Name         string `json:"name"`
	TeamName     string `json:"team_name"`
	Background   bool   `json:"run_in_background"`
}

// ClaudeAgent is the resolved, admitted definition of a research child.
type ClaudeAgent struct {
	Type   string
	Source string // builtin or the definition file
	Model  string
	Tools  []string
}

// AdmitClaudeAgent resolves the definition the harness will use for launch and
// refuses any child that could write. cwd is the parent's working directory;
// configDir is Claude's user configuration directory.
func AdmitClaudeAgent(launch ClaudeLaunch, cwd, configDir string) (ClaudeAgent, error) {
	agentType := strings.TrimSpace(launch.SubagentType)
	if agentType == "" {
		agentType = "general-purpose"
	}
	if launch.Isolation != "" {
		return ClaudeAgent{}, fmt.Errorf("isolation %q is refused: research children run in the parent's checkout and Posse never adopts harness-owned worktrees", launch.Isolation)
	}
	if launch.TeamName != "" || launch.Name != "" {
		return ClaudeAgent{}, fmt.Errorf("named teammates are refused: research children are bounded, unnamed subagents")
	}
	if launch.Mode != "" && launch.Mode != "default" && launch.Mode != "plan" {
		return ClaudeAgent{}, fmt.Errorf("permission mode %q is refused for a read-only research child", launch.Mode)
	}
	if strings.Contains(agentType, ":") {
		return ClaudeAgent{}, fmt.Errorf("plugin agent %q is refused: Posse cannot verify its definition is read-only", agentType)
	}
	definition, found, err := findClaudeAgentDefinition(agentType, cwd, configDir)
	if err != nil {
		return ClaudeAgent{}, err
	}
	model := launch.Model
	if found {
		if err := definition.readOnly(); err != nil {
			return ClaudeAgent{}, fmt.Errorf("agent %q from %s is refused: %w", agentType, definition.path, err)
		}
		if model == "" {
			model = definition.model
		}
		return ClaudeAgent{Type: agentType, Source: definition.path, Model: modelOrInherit(model), Tools: definition.tools}, nil
	}
	tools, ok := claudeBuiltinReadOnlyAgents[agentType]
	if !ok {
		return ClaudeAgent{}, fmt.Errorf("agent %q is refused: only Explore, Plan or a custom definition with an explicit read-only tool list may run as tracked research", agentType)
	}
	return ClaudeAgent{Type: agentType, Source: "builtin", Model: modelOrInherit(model), Tools: append([]string(nil), tools...)}, nil
}

func modelOrInherit(model string) string {
	if strings.TrimSpace(model) == "" {
		return "inherit"
	}
	return model
}

type claudeAgentDefinition struct {
	path           string
	model          string
	tools          []string
	toolsSet       bool
	permissionMode string
	isolation      string
	mcpServers     bool
}

func (d claudeAgentDefinition) readOnly() error {
	if !d.toolsSet {
		return fmt.Errorf("it inherits every parent tool; declare an explicit read-only `tools` list")
	}
	if d.permissionMode != "" && d.permissionMode != "default" && d.permissionMode != "plan" {
		return fmt.Errorf("permissionMode %q can bypass read-only enforcement", d.permissionMode)
	}
	if d.isolation != "" {
		return fmt.Errorf("isolation %q creates a harness-owned worktree", d.isolation)
	}
	if d.mcpServers {
		return fmt.Errorf("it declares MCP servers whose tools cannot be verified read-only")
	}
	for _, tool := range d.tools {
		if tool != "Bash" && !ClaudeToolAllowed(tool) {
			return fmt.Errorf("tool %q can change files or launch further agents", tool)
		}
	}
	return nil
}

// findClaudeAgentDefinition returns the highest-precedence definition named
// agentType: project agents, then user agents.
func findClaudeAgentDefinition(agentType, cwd, configDir string) (claudeAgentDefinition, bool, error) {
	roots := []string{}
	for dir := cwd; dir != "" && cwd != ""; dir = filepath.Dir(dir) {
		roots = append(roots, filepath.Join(dir, ".claude", "agents"))
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	if configDir != "" {
		roots = append(roots, filepath.Join(configDir, "agents"))
	}
	for _, root := range roots {
		var files []string
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err == nil && !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
				files = append(files, path)
			}
			return nil
		})
		sort.Strings(files)
		for _, path := range files {
			data, err := os.ReadFile(path)
			if err != nil {
				return claudeAgentDefinition{}, false, fmt.Errorf("read agent definition %s: %w", path, err)
			}
			definition, name := parseClaudeAgentDefinition(data)
			if name == agentType {
				definition.path = path
				return definition, true, nil
			}
		}
	}
	return claudeAgentDefinition{}, false, nil
}

// parseClaudeAgentDefinition reads the YAML frontmatter keys that decide a
// child's effects. Unknown keys are ignored; list values may be inline,
// comma-separated or YAML block lists.
func parseClaudeAgentDefinition(data []byte) (claudeAgentDefinition, string) {
	var definition claudeAgentDefinition
	name := ""
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return definition, ""
	}
	listKey := ""
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		trimmed := strings.TrimSpace(line)
		if listKey != "" && strings.HasPrefix(trimmed, "- ") {
			if listKey == "tools" {
				definition.tools = append(definition.tools, unquote(strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))))
			}
			continue
		}
		listKey = ""
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "name":
			name = unquote(value)
		case "model":
			definition.model = unquote(value)
		case "permissionMode":
			definition.permissionMode = unquote(value)
		case "isolation":
			definition.isolation = unquote(value)
		case "mcpServers":
			// An empty value starts a block of servers.
			definition.mcpServers = value != "[]" && value != "{}"
		case "tools":
			definition.toolsSet = true
			if value == "" {
				listKey = "tools"
				continue
			}
			value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
			for tool := range strings.SplitSeq(value, ",") {
				if tool = unquote(strings.TrimSpace(tool)); tool != "" {
					definition.tools = append(definition.tools, tool)
				}
			}
		}
	}
	return definition, name
}

func unquote(value string) string {
	if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
		return value[1 : len(value)-1]
	}
	return value
}

// readOnlyPrograms may run in a research child's shell. Each entry lists the
// arguments that would let the program write or execute something else.
var readOnlyPrograms = map[string][]string{
	"basename": nil, "cat": nil, "cmp": nil, "cut": nil, "diff": nil, "dirname": nil, "du": nil,
	"echo": nil, "file": nil, "head": nil, "jq": nil, "ls": nil, "nl": nil, "pwd": nil,
	"readlink": nil, "realpath": nil, "stat": nil, "tail": nil, "tr": nil, "true": nil, "wc": nil, "which": nil,
	"find": {"-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls"},
	"grep": nil, "egrep": nil, "fgrep": nil,
	"rg":   {"--pre", "--pre-glob"},
	"sort": {"-o", "--output"},
	"tree": {"-o"},
}

// readOnlyGitCommands are git subcommands that only read repository state.
var readOnlyGitCommands = map[string]bool{
	"blame": true, "cat-file": true, "describe": true, "diff": true, "grep": true, "log": true,
	"ls-files": true, "ls-tree": true, "merge-base": true, "rev-list": true, "rev-parse": true,
	"shortlog": true, "show": true, "show-ref": true, "status": true,
}

// readOnlyGHCommands maps a gh command group to the subcommands that only read
// tracker or CI state. Anything not listed is refused.
var readOnlyGHCommands = map[string]map[string]bool{
	"issue": {"list": true, "view": true},
	"pr":    {"list": true, "view": true, "diff": true, "checks": true},
	"run":   {"list": true, "view": true},
}

// readOnlyGHAPIFlags are the only flags `gh api` may carry; the value is true
// when the flag consumes an argument. Body, field and input flags are absent
// because they turn the request into a POST.
var readOnlyGHAPIFlags = map[string]bool{
	"--paginate": false, "--slurp": false, "-i": false, "--include": false,
	"--jq": true, "-q": true, "--cache": true, "--method": true, "-X": true,
	"--hostname": true, "-H": true, "--header": true,
}

func readOnlyGH(args []string) error {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if (args[0] == "-R" || args[0] == "--repo") && len(args) > 1 {
			args = args[2:]
			continue
		}
		return fmt.Errorf("gh option %s is refused", args[0])
	}
	if len(args) == 0 {
		return fmt.Errorf("gh needs a read-only subcommand")
	}
	if args[0] == "api" {
		return readOnlyGHAPI(args[1:])
	}
	if len(args) < 2 || !readOnlyGHCommands[args[0]][args[1]] {
		return fmt.Errorf("gh %s is not a read-only gh command; allowed: issue list|view, pr list|view|diff|checks, run list|view, api with GET", strings.Join(args[:min(len(args), 2)], " "))
	}
	for _, arg := range args[2:] {
		if arg == "--web" || arg == "-w" {
			return fmt.Errorf("gh %s %s %s opens a browser and is refused", args[0], args[1], arg)
		}
	}
	return nil
}

// readOnlyGHAPI admits a single GET request to a REST endpoint. graphql is
// refused because it is always a POST and can carry mutations.
func readOnlyGHAPI(args []string) error {
	endpoints := 0
	for i := 0; i < len(args); i++ {
		name, value, joined := strings.Cut(args[i], "=")
		if !strings.HasPrefix(args[i], "-") {
			endpoints++
			if strings.HasPrefix(strings.TrimPrefix(args[i], "/"), "graphql") {
				return fmt.Errorf("gh api graphql is refused: it can mutate")
			}
			continue
		}
		takesValue, ok := readOnlyGHAPIFlags[name]
		if !ok {
			return fmt.Errorf("gh api flag %s is refused: only GET reads are allowed", args[i])
		}
		if takesValue && !joined {
			if i+1 >= len(args) {
				return fmt.Errorf("gh api flag %s needs a value", args[i])
			}
			i++
			value = args[i]
		}
		if (name == "--method" || name == "-X") && !strings.EqualFold(value, "GET") {
			return fmt.Errorf("gh api method %s is refused: only GET reads are allowed", value)
		}
	}
	if endpoints != 1 {
		return fmt.Errorf("gh api needs exactly one REST endpoint")
	}
	return nil
}

// ReadOnlyCommand reports whether a shell command only reads. It accepts
// pipelines and lists of allowlisted programs with literal or parameter
// arguments, and refuses redirection to files, substitutions, subshells and
// every program that is not known to be read-only.
func ReadOnlyCommand(command string) error {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return fmt.Errorf("posse cannot parse the command")
	}
	if len(file.Stmts) == 0 {
		return fmt.Errorf("empty command")
	}
	for _, stmt := range file.Stmts {
		if err := readOnlyStmt(stmt); err != nil {
			return err
		}
	}
	return nil
}

func readOnlyStmt(stmt *syntax.Stmt) error {
	if stmt.Background || stmt.Coprocess {
		return fmt.Errorf("background commands are refused")
	}
	for _, redirect := range stmt.Redirs {
		if err := readOnlyRedirect(redirect); err != nil {
			return err
		}
	}
	switch command := stmt.Cmd.(type) {
	case *syntax.CallExpr:
		return readOnlyCall(command)
	case *syntax.BinaryCmd:
		if command.Op != syntax.Pipe && command.Op != syntax.AndStmt && command.Op != syntax.OrStmt {
			return fmt.Errorf("operator %s is refused", command.Op)
		}
		if err := readOnlyStmt(command.X); err != nil {
			return err
		}
		return readOnlyStmt(command.Y)
	default:
		return fmt.Errorf("compound shell commands are refused")
	}
}

func readOnlyRedirect(redirect *syntax.Redirect) error {
	switch redirect.Op {
	case syntax.RdrIn, syntax.DplIn, syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
		return nil
	case syntax.DplOut:
		// >&N duplicates a descriptor; >&file writes the file.
		if word := redirect.Word.Lit(); word == "-" || strings.Trim(word, "0123456789") == "" && word != "" {
			return nil
		}
	case syntax.RdrOut, syntax.AppOut, syntax.RdrAll, syntax.AppAll, syntax.RdrClob:
		if redirect.Word != nil && redirect.Word.Lit() == "/dev/null" {
			return nil
		}
	}
	return fmt.Errorf("redirection that writes a file is refused")
}

func readOnlyCall(call *syntax.CallExpr) error {
	if len(call.Assigns) != 0 {
		return fmt.Errorf("environment assignments are refused")
	}
	if len(call.Args) == 0 {
		return fmt.Errorf("empty command")
	}
	args := make([]string, 0, len(call.Args))
	for _, word := range call.Args {
		value, err := literalWord(word)
		if err != nil {
			return err
		}
		args = append(args, value)
	}
	program := filepath.Base(args[0])
	switch program {
	case "git":
		return readOnlyGit(args[1:])
	case "gh":
		return readOnlyGH(args[1:])
	}
	refused, ok := readOnlyPrograms[program]
	if !ok {
		return fmt.Errorf("%s is not a known read-only program", program)
	}
	for _, arg := range args[1:] {
		for _, flag := range refused {
			if arg == flag || strings.HasPrefix(arg, flag+"=") || (len(flag) == 2 && strings.HasPrefix(arg, flag)) {
				return fmt.Errorf("%s %s can write or execute", program, flag)
			}
		}
	}
	return nil
}

func readOnlyGit(args []string) error {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch {
		case args[0] == "--no-pager":
			args = args[1:]
		case args[0] == "-C" && len(args) > 1:
			args = args[2:]
		default:
			return fmt.Errorf("git option %s is refused", args[0])
		}
	}
	if len(args) == 0 || !readOnlyGitCommands[args[0]] {
		subcommand := ""
		if len(args) > 0 {
			subcommand = " " + args[0]
		}
		return fmt.Errorf("git%s is not a read-only git command", subcommand)
	}
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "--output") || strings.HasPrefix(arg, "--ext-diff") || strings.HasPrefix(arg, "--textconv") {
			return fmt.Errorf("git %s %s can write or execute", args[0], arg)
		}
	}
	return nil
}

// literalWord returns a word's text when it has no command or process
// substitution. Parameter expansions are kept as written; they cannot run code.
func literalWord(word *syntax.Word) (string, error) {
	var builder strings.Builder
	var walk func(parts []syntax.WordPart) error
	walk = func(parts []syntax.WordPart) error {
		for _, part := range parts {
			switch value := part.(type) {
			case *syntax.Lit:
				builder.WriteString(value.Value)
			case *syntax.SglQuoted:
				builder.WriteString(value.Value)
			case *syntax.DblQuoted:
				if err := walk(value.Parts); err != nil {
					return err
				}
			case *syntax.ParamExp:
				if value.Exp != nil || value.Repl != nil || value.Slice != nil {
					return fmt.Errorf("parameter operations are refused")
				}
				builder.WriteString("$")
				if value.Param != nil {
					builder.WriteString(value.Param.Value)
				}
			default:
				return fmt.Errorf("command, arithmetic and process substitution are refused")
			}
		}
		return nil
	}
	if err := walk(word.Parts); err != nil {
		return "", err
	}
	return builder.String(), nil
}
