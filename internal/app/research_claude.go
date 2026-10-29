package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/execgroup"
	"github.com/thanhbinh1905/posse/internal/research"
	"github.com/thanhbinh1905/posse/internal/store"
)

// claudeResearchHookInput is the subset of Claude Code hook input the research
// adapter reads. agent_id is present only inside a subagent.
type claudeResearchHookInput struct {
	SessionID            string          `json:"session_id"`
	TranscriptPath       string          `json:"transcript_path"`
	CWD                  string          `json:"cwd"`
	PermissionMode       string          `json:"permission_mode"`
	HookEventName        string          `json:"hook_event_name"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolUseID            string          `json:"tool_use_id"`
	AgentID              string          `json:"agent_id"`
	AgentType            string          `json:"agent_type"`
	AgentTranscriptPath  string          `json:"agent_transcript_path"`
	LastAssistantMessage string          `json:"last_assistant_message"`
}

// claudeAgentTools are the Claude Code tools that launch a subagent.
func claudeAgentTool(name string) bool { return name == "Agent" || name == "Task" }

// RunResearchHook is `posse _research-hook claude [--version <v>]`: Claude Code
// PreToolUse, SubagentStart and SubagentStop hooks of a Posse-launched Lead or
// Worker. Exit 2 refuses a tool call and shows stderr to the agent.
func (s *Service) RunResearchHook(stdin io.Reader, stdout, stderr io.Writer, args []string) int {
	if len(args) < 1 || args[0] != string(research.Claude) {
		fmt.Fprintln(stderr, "posse: _research-hook supports only claude")
		return 1
	}
	version := ""
	if len(args) == 3 && args[1] == "--version" {
		version = args[2]
	}
	data, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
	if err != nil {
		return 0
	}
	var input claudeResearchHookInput
	if json.Unmarshal(data, &input) != nil {
		return 0
	}
	// Ordinary parent tool calls are the hot path: decide without the store.
	if input.HookEventName == "PreToolUse" && input.AgentID == "" && !claudeAgentTool(input.ToolName) {
		return 0
	}
	ctx := context.Background()
	refusal, extra, err := s.claudeResearchHook(ctx, input, version)
	if err != nil {
		if input.HookEventName == "PreToolUse" {
			fmt.Fprintf(stderr, "posse: research hook failed, so this call is refused: %v\n", err)
			return guardExitBlocked
		}
		fmt.Fprintf(stderr, "posse: research hook failed: %v\n", err)
		return 1
	}
	if extra != "" {
		output, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": input.HookEventName, "additionalContext": extra}})
		fmt.Fprintln(stdout, string(output))
	}
	if refusal != "" {
		fmt.Fprintln(stderr, "posse: "+refusal)
		return guardExitBlocked
	}
	return 0
}

func (s *Service) claudeResearchHook(ctx context.Context, input claudeResearchHookInput, version string) (string, string, error) {
	db, home, err := s.openDB()
	if err != nil {
		return "", "", err
	}
	defer db.Close()
	switch input.HookEventName {
	case "PreToolUse":
		if input.AgentID != "" {
			return s.claudeChildToolCall(ctx, db, home, input)
		}
		return s.admitClaudeChild(ctx, db, home, input, version)
	case "SubagentStart":
		return s.bindClaudeChild(ctx, db, input)
	case "SubagentStop":
		return "", "", s.collectClaudeChild(ctx, db, home, input)
	}
	return "", "", nil
}

// admitClaudeChild registers an Agent tool call before Claude starts the
// child, or refuses it visibly with a recorded refused attempt.
func (s *Service) admitClaudeChild(ctx context.Context, db *store.DB, home string, input claudeResearchHookInput, version string) (string, string, error) {
	parent, err := s.resolveResearchParent(ctx, db)
	if errors.Is(err, errResearchParentUnknown) {
		return "research children of this session are refused: Posse cannot find its Lead or Worker parent", "", nil
	}
	if err != nil {
		return "", "", err
	}
	cfg, err := s.researchConfig(home, parent.project)
	if err != nil {
		return "", "", err
	}
	if cfg.Research.Native == config.ResearchOff {
		return "", "", nil
	}
	var launch research.ClaudeLaunch
	_ = json.Unmarshal(input.ToolInput, &launch)
	attempt := store.ResearchAttempt{
		ProjectID: parent.project.ID, TaskID: parent.task.ID, ParentRole: parent.role(), ParentSession: input.SessionID,
		ParentPID: parent.agentPID, ParentIdentity: parent.agentIdentity, Harness: string(research.Claude), HarnessVersion: version,
		IdempotencyKey: claudeToolKey(input), AgentType: launch.SubagentType, Model: launch.Model, Scope: input.CWD,
		PromptDigest: digestText(launch.Prompt), State: string(research.Registered),
	}
	refuse := func(reason string) (string, string, error) {
		attempt.State, attempt.Note = string(research.Refused), reason
		attempt.IdempotencyKey = ""
		refused, _, err := db.CreateResearchAttempt(ctx, attempt, 0)
		if err != nil {
			return "", "", err
		}
		return fmt.Sprintf("research child refused (%s): %s. Use a read-only Explore or Plan agent, or ask a Worker to do write work.", research.AttemptID(refused.ID), reason), "", nil
	}
	if input.SessionID == "" || input.ToolUseID == "" {
		return refuse("the hook input has no session or tool use identity")
	}
	// A repeated delivery of the same tool call keeps its first decision.
	if existing, err := db.ResearchAttemptByKey(ctx, parent.project.ID, string(research.Claude), attempt.IdempotencyKey); err == nil {
		if existing.State == string(research.Refused) {
			return fmt.Sprintf("research child %s was refused: %s", research.AttemptID(existing.ID), existing.Note), "", nil
		}
		return "", "", nil
	} else if !store.IsNotFound(err) {
		return "", "", err
	}
	if version == "" {
		version = claudeHookVersion(ctx, home, parent)
	}
	checked, err := research.CheckVersion(research.Claude, version)
	attempt.HarnessVersion = checked
	if err != nil {
		return refuse(err.Error())
	}
	if scope, err := researchScopeAllowed(ctx, db, parent, input.CWD); err != nil {
		return refuse(err.Error())
	} else {
		attempt.Scope = scope
	}
	agent, err := research.AdmitClaudeAgent(launch, input.CWD, claudeConfigDirectory())
	if err != nil {
		return refuse(err.Error())
	}
	attempt.AgentType, attempt.Model, attempt.Tools = agent.Type, agent.Model, agent.Tools
	// A second unbound child of the same type could not be told apart from
	// the first when Claude starts it.
	if pending, err := db.UnboundResearchAttempts(ctx, string(research.Claude), input.SessionID, agent.Type); err != nil {
		return "", "", err
	} else if len(pending) > 0 {
		return refuse(fmt.Sprintf("%s %s child %s has not started yet; launch one %s child at a time", parent.label(), agent.Type, research.AttemptID(pending[0].ID), agent.Type))
	}
	registered, created, err := db.CreateResearchAttempt(ctx, attempt, cfg.Research.MaxActive)
	if errors.Is(err, store.ErrResearchBudget) {
		return refuse(err.Error() + "; wait for a child to finish or cancel one with `posse research cancel`")
	}
	if err != nil {
		return "", "", err
	}
	if !created && registered.State == string(research.Refused) {
		return fmt.Sprintf("research child %s was refused: %s", research.AttemptID(registered.ID), registered.Note), "", nil
	}
	return "", "", nil
}

func claudeToolKey(input claudeResearchHookInput) string {
	return "claude-tool:" + input.SessionID + ":" + input.ToolUseID
}

// bindClaudeChild binds the native agent ID to the single registered attempt
// of this parent session and agent type. An unregistered child is told to stop.
func (s *Service) bindClaudeChild(ctx context.Context, db *store.DB, input claudeResearchHookInput) (string, string, error) {
	if input.AgentID == "" {
		return "", "", nil
	}
	if attempt, err := db.ResearchAttemptByChild(ctx, string(research.Claude), input.AgentID); err == nil {
		return "", researchChildContext(attempt), nil
	} else if !store.IsNotFound(err) {
		return "", "", err
	}
	attempt, err := s.bindPendingClaudeChild(ctx, db, input)
	if err != nil {
		return "", "", err
	}
	if attempt == nil {
		return "", "This subagent is not a registered Posse research child. Posse refuses its tool calls; stop now and report that it was not registered.", nil
	}
	return "", researchChildContext(*attempt), nil
}

func (s *Service) bindPendingClaudeChild(ctx context.Context, db *store.DB, input claudeResearchHookInput) (*store.ResearchAttempt, error) {
	pending, err := db.UnboundResearchAttempts(ctx, string(research.Claude), input.SessionID, input.AgentType)
	if err != nil || len(pending) != 1 {
		return nil, err
	}
	child := input.AgentID
	transcript := claudeChildTranscript(input)
	change := store.ResearchChange{State: researchState(research.Running), ChildID: &child}
	if transcript != "" {
		change.SessionRef = &transcript
	}
	bound, err := db.UpdateResearchAttempt(ctx, pending[0].ID, []string{string(research.Registered)}, change)
	if errors.Is(err, store.ErrStateRace) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &bound, nil
}

func researchChildContext(attempt store.ResearchAttempt) string {
	return fmt.Sprintf("You are Posse research child %s: read-only. Posse refuses file edits, writes, nested agents and shell commands that are not known to be read-only. Return your findings as your final message.", research.AttemptID(attempt.ID))
}

// reopenClaudeChild moves a completed child back to running when it fits the
// active budget. It returns the refusal reason when it does not.
func (s *Service) reopenClaudeChild(ctx context.Context, db *store.DB, home string, attempt store.ResearchAttempt) (string, error) {
	project, err := db.ProjectByID(ctx, attempt.ProjectID)
	if err != nil {
		return "", err
	}
	cfg, err := s.researchConfig(home, project)
	if err != nil {
		return "", err
	}
	active, err := db.CountActiveResearchAttempts(ctx)
	if err != nil {
		return "", err
	}
	if active >= cfg.Research.MaxActive {
		return fmt.Sprintf("research budget of %d active children is used; wait for a child to finish or cancel one with `posse research cancel`", cfg.Research.MaxActive), nil
	}
	note := "resumed after completion"
	if _, err := db.UpdateResearchAttempt(ctx, attempt.ID, []string{string(research.Completed)}, store.ResearchChange{State: researchState(research.Running), Note: &note}); err != nil && !errors.Is(err, store.ErrStateRace) {
		return "", err
	}
	return "", nil
}

// claudeChildTranscript is where Claude Code keeps a subagent transcript: a
// subagents directory beside the parent session transcript.
func claudeChildTranscript(input claudeResearchHookInput) string {
	if input.AgentTranscriptPath != "" {
		return input.AgentTranscriptPath
	}
	if input.TranscriptPath == "" || input.AgentID == "" {
		return ""
	}
	base := strings.TrimSuffix(input.TranscriptPath, filepath.Ext(input.TranscriptPath))
	return filepath.Join(base, "subagents", "agent-"+input.AgentID+".jsonl")
}

// claudeChildToolCall enforces the read-only contract inside the child,
// regardless of its declared permission mode or any bypass the parent holds.
func (s *Service) claudeChildToolCall(ctx context.Context, db *store.DB, home string, input claudeResearchHookInput) (string, string, error) {
	attempt, err := db.ResearchAttemptByChild(ctx, string(research.Claude), input.AgentID)
	if store.IsNotFound(err) {
		bound, bindErr := s.bindPendingClaudeChild(ctx, db, input)
		if bindErr != nil {
			return "", "", bindErr
		}
		if bound == nil {
			return "this subagent is not a registered Posse research child; stop and report that it was not registered", "", nil
		}
		attempt = *bound
	} else if err != nil {
		return "", "", err
	}
	id := research.AttemptID(attempt.ID)
	switch research.State(attempt.State) {
	case research.Running, research.Registered:
	case research.Interrupted:
		// A resumed parent session continues its own interrupted child.
		if !attempt.Resumable || attempt.ParentSession != input.SessionID {
			return fmt.Sprintf("research child %s was interrupted and cannot resume in this session; stop now", id), "", nil
		}
		parentPID, parentIdentity := 0, ""
		for _, process := range processAncestors(procRoot, os.Getpid()) {
			if process.agent {
				parentPID = process.pid
				parentIdentity, _ = researchProcessIdentity(process.pid)
				break
			}
		}
		note := "resumed with its parent session"
		if _, err := db.UpdateResearchAttempt(ctx, attempt.ID, []string{string(research.Interrupted)}, store.ResearchChange{State: researchState(research.Running), Note: &note}); err != nil && !errors.Is(err, store.ErrStateRace) {
			return "", "", err
		}
		if parentPID != 0 {
			if _, err := db.ExecContext(ctx, `UPDATE research_attempts SET parent_pid=?, parent_identity=? WHERE id=?`, parentPID, parentIdentity, attempt.ID); err != nil {
				return "", "", err
			}
		}
	case research.Completed:
		// SendMessage resumes a finished child inside its parent session; the
		// same attempt runs again and SubagentStop accepts its new result.
		if attempt.ParentSession != input.SessionID {
			return fmt.Sprintf("research child %s is completed and belongs to another session; stop now", id), "", nil
		}
		reason, err := s.reopenClaudeChild(ctx, db, home, attempt)
		if err != nil {
			return "", "", err
		}
		if reason != "" {
			return fmt.Sprintf("research child %s is completed and cannot resume: %s; stop now", id, reason), "", nil
		}
	case research.Cancelled:
		return fmt.Sprintf("research child %s was cancelled by Posse; stop now and return without further tool calls", id), "", nil
	default:
		return fmt.Sprintf("research child %s is %s; stop now", id, attempt.State), "", nil
	}
	if claudeAgentTool(input.ToolName) {
		return fmt.Sprintf("research child %s cannot launch further agents", id), "", nil
	}
	if input.ToolName == "Bash" {
		var tool struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(input.ToolInput, &tool)
		if err := research.ReadOnlyCommand(tool.Command); err != nil {
			return fmt.Sprintf("research child %s is read-only: %v", id, err), "", nil
		}
		return "", "", nil
	}
	if !research.ClaudeToolAllowed(input.ToolName) {
		return fmt.Sprintf("research child %s is read-only: tool %s is refused", id, input.ToolName), "", nil
	}
	return "", "", nil
}

// collectClaudeChild accepts a bound child's final answer and usage. A child
// cancelled or unknown to Posse contributes nothing.
func (s *Service) collectClaudeChild(ctx context.Context, db *store.DB, home string, input claudeResearchHookInput) error {
	attempt, err := db.ResearchAttemptByChild(ctx, string(research.Claude), input.AgentID)
	if store.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	transcript := claudeChildTranscript(input)
	change := store.ResearchChange{}
	if transcript != "" {
		change.SessionRef = &transcript
		if usage, model, err := research.ClaudeTranscriptUsage(transcript); err == nil {
			encoded := usage.JSON()
			change.Usage = &encoded
			if model != "" && (attempt.Model == "" || attempt.Model == "inherit") {
				change.Model = &model
			}
		}
	}
	if research.State(attempt.State) != research.Running {
		note := attempt.Note
		if !strings.Contains(note, "result discarded") {
			note = strings.TrimPrefix(note+"; result discarded", "; ")
		}
		change.Note = &note
		_, err := db.UpdateResearchAttempt(ctx, attempt.ID, nil, change)
		return err
	}
	project, err := db.ProjectByID(ctx, attempt.ProjectID)
	if err != nil {
		return err
	}
	cfg, err := s.researchConfig(home, project)
	if err != nil {
		return err
	}
	result, err := writeResearchResult(researchDirectory(home, project.Name, attempt.ID), input.LastAssistantMessage, cfg.Research.MaxResultBytes)
	if err != nil {
		return err
	}
	result.State = researchState(research.Completed)
	result.SessionRef, result.Usage, result.Model = change.SessionRef, change.Usage, change.Model
	_, err = db.UpdateResearchAttempt(ctx, attempt.ID, []string{string(research.Running)}, result)
	if errors.Is(err, store.ErrStateRace) {
		return nil
	}
	return err
}

func claudeConfigDirectory() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// researchScopeAllowed resolves dir and requires it inside the parent's own
// checkout: the Lead's Project root or Task Sandboxes, or the Worker's Sandbox.
func researchScopeAllowed(ctx context.Context, db *store.DB, parent researchParent, dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("an explicit scope directory is required")
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("scope %s: %w", dir, err)
	}
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return "", fmt.Errorf("scope %s is not a directory", dir)
	}
	roots := []string{}
	if parent.task.ID != 0 {
		roots = append(roots, parent.task.WorktreePath)
	} else {
		roots = append(roots, parent.project.Root)
		if tasks, err := db.LiveTasks(ctx, parent.project.ID); err == nil {
			for _, task := range tasks {
				roots = append(roots, task.WorktreePath)
			}
		}
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		if real, err := filepath.EvalSymlinks(root); err == nil && (real == resolved || pathWithin(real, resolved)) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("scope %s is outside the %s's checkout", dir, parent.label())
}

// writeClaudeResearchPlugin writes the Posse-owned Claude plugin that installs
// the research hooks for one Lead or Worker launch. Launching never runs the
// harness: the hook learns the Claude version only when an Agent call needs
// admission, and an unsupported or unknown version refuses it visibly.
func writeClaudeResearchPlugin(home, project string) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate posse executable for the Claude research hooks: %w", err)
	}
	command := shellQuote(executable) + " _research-hook claude"
	group := func(matcher string) []any {
		entry := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 10}}}
		if matcher != "" {
			entry["matcher"] = matcher
		}
		return []any{entry}
	}
	root := filepath.Join(home, "projects", project, "claude-research")
	hooks := map[string]any{
		"PreToolUse":    group(""),
		"SubagentStart": group(""),
		"SubagentStop":  group(""),
	}
	if err := writeClaudeHookPlugin(root, "posse-research", "Posse tracking for bounded read-only research subagents", hooks); err != nil {
		return "", err
	}
	return root, nil
}

// writeClaudeHookPlugin writes a Posse-owned Claude plugin under root that
// installs hooks.
func writeClaudeHookPlugin(root, name, description string, hooks map[string]any) error {
	hooksJSON, err := json.MarshalIndent(map[string]any{"hooks": hooks}, "", "  ")
	if err != nil {
		return err
	}
	manifest, err := json.MarshalIndent(map[string]string{"name": name, "version": "1.0.0", "description": description}, "", "  ")
	if err != nil {
		return err
	}
	for path, data := range map[string][]byte{filepath.Join(root, ".claude-plugin", "plugin.json"): manifest, filepath.Join(root, "hooks", "hooks.json"): hooksJSON} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := writeFile(path, append(data, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// researchHarnessVersion reports `<harness> --version`, or "" when the harness
// cannot report one. Tests replace it.
var researchHarnessVersion = func(ctx context.Context, harness research.Harness) string {
	return researchExecutableVersion(ctx, string(harness))
}

func researchExecutableVersion(ctx context.Context, executable string) string {
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := execgroup.CommandContext(probe, executable, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// claudeHookVersion reports the version of the parent's Claude executable,
// probed once per executable file and cached under POSSE_HOME. It falls back
// to `claude` on PATH when the parent executable is unknown.
func claudeHookVersion(ctx context.Context, home string, parent researchParent) string {
	executable := ""
	if parent.agentPID > 0 {
		if path, err := os.Readlink(filepath.Join(procRoot, strconv.Itoa(parent.agentPID), "exe")); err == nil {
			executable = strings.TrimSuffix(path, " (deleted)")
		}
	}
	if executable == "" || !knownAgentProcess(executable) {
		return researchHarnessVersion(ctx, research.Claude)
	}
	info, err := os.Stat(executable)
	if err != nil {
		return researchHarnessVersion(ctx, research.Claude)
	}
	key := digestText(fmt.Sprintf("%s\x00%d\x00%d", executable, info.Size(), info.ModTime().UnixNano()))
	cache := filepath.Join(home, "research-versions", "claude-"+key[:32])
	if data, err := os.ReadFile(cache); err == nil {
		return string(data)
	}
	version := researchExecutableVersion(ctx, executable)
	if _, ok := research.ParseVersion(version); ok {
		if err := os.MkdirAll(filepath.Dir(cache), 0o700); err == nil {
			_ = writeFile(cache, []byte(version))
		}
	}
	return version
}

// appendResearchLaunchArgs installs the research hooks for a Claude Lead or
// Worker when research.native is tracked. Other harnesses launch research
// children through `posse research start`, which needs no launch arguments.
func appendResearchLaunchArgs(ctx context.Context, home string, project store.Project, cfg config.Config, kind string, args []string) ([]string, error) {
	if kind != string(research.Claude) || cfg.Research.Native != config.ResearchTracked {
		return args, nil
	}
	plugin, err := writeClaudeResearchPlugin(home, project.Name)
	if err != nil {
		return nil, err
	}
	return append(args, "--plugin-dir", plugin), nil
}
