package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/execgroup"
	"github.com/thanhbinh1905/posse/internal/research"
	"github.com/thanhbinh1905/posse/internal/store"
)

// researchPromptLimit bounds a research prompt passed to a child process.
const researchPromptLimit = 64 << 10

// startResearchRunner launches the detached `posse _research-run` supervisor
// of one attempt. It survives its caller so a lost parent never loses the
// child's result. Tests replace it.
var startResearchRunner = func(home string, id int64) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	dir := filepath.Dir(researchRunnerLog(home, id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(researchRunnerLog(home, id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	command := exec.Command(executable, "_research-run", research.AttemptID(id))
	command.Env = append(os.Environ(), "POSSE_HOME="+home)
	command.Stdout, command.Stderr = logFile, logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func researchRunnerLog(home string, id int64) string {
	return filepath.Join(home, "research-runs", research.AttemptID(id)+".log")
}

func (s *Service) researchStart(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("research start", args, map[string]flagSpec{"harness": {}, "scope": {}, "prompt": {}, "prompt-file": {}, "model": {}, "key": {}, "wait": {boolean: true}, "timeout": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("research start takes no positional arguments; pass the prompt with --prompt or --prompt-file")
	}
	harness, err := research.ParseHarness(parsed.Flags["harness"])
	if err != nil {
		return axi.Usage(err.Error())
	}
	if harness == research.Claude {
		return axi.Failure("research_harness_hooks", "Claude research children are launched with the Agent tool; Posse registers them through its hooks", false, "Use an Explore or Plan agent from a Posse-launched Claude session")
	}
	prompt, err := researchPrompt(parsed)
	if err != nil {
		return err
	}
	scope := parsed.Flags["scope"]
	if scope == "" {
		return axi.Usage("research start requires --scope <directory>; children never default to the parent's directory")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	parent, err := s.resolveResearchParent(ctx.Context, db)
	if errors.Is(err, errResearchParentUnknown) {
		return researchParentFailure()
	}
	if err != nil {
		return err
	}
	cfg, err := s.researchConfig(home, parent.project)
	if err != nil {
		return err
	}
	if cfg.Research.Native != config.ResearchTracked {
		return axi.Failure("research_disabled", "research.native is off; Posse launches no research children", false, "Ask the User to set `research.native = \"tracked\"`")
	}
	if err := s.reconcileResearch(ctx.Context, db); err != nil {
		return err
	}
	model := parsed.Flags["model"]
	attempt := store.ResearchAttempt{
		ProjectID: parent.project.ID, TaskID: parent.task.ID, ParentRole: parent.role(), ParentPID: parent.agentPID, ParentIdentity: parent.agentIdentity,
		Harness: string(harness), IdempotencyKey: parsed.Flags["key"], Model: modelOrDefault(model), Tools: researchTools(harness), PromptDigest: digestText(prompt), State: string(research.Registered),
	}
	if parent.agentPID != 0 {
		attempt.ParentSession = parentSessionHint(harness)
	}
	if attempt.IdempotencyKey != "" {
		if existing, err := db.ResearchAttemptByKey(ctx.Context, parent.project.ID, string(harness), attempt.IdempotencyKey); err == nil {
			if existing.TaskID != parent.task.ID {
				return axi.Failure("research_key_conflict", "the research key belongs to another parent", false)
			}
			return s.printResearchOutcome(ctx, db, home, existing, parsed)
		} else if !store.IsNotFound(err) {
			return err
		}
	}
	resolved, scopeErr := researchScopeAllowed(ctx.Context, db, parent, scope)
	attempt.Scope = resolved
	if scopeErr != nil {
		attempt.Scope = scope
		return s.refuseResearch(ctx.Context, db, attempt, "research_scope_refused", scopeErr.Error())
	}
	version, versionErr := research.CheckVersion(harness, researchHarnessVersion(ctx.Context, harness))
	attempt.HarnessVersion = version
	if versionErr != nil {
		return s.refuseResearch(ctx.Context, db, attempt, "research_unsupported", versionErr.Error())
	}
	created, fresh, err := db.CreateResearchAttempt(ctx.Context, attempt, cfg.Research.MaxActive)
	if errors.Is(err, store.ErrResearchBudget) {
		return s.refuseResearch(ctx.Context, db, attempt, "research_budget", err.Error())
	}
	if err != nil {
		return err
	}
	if fresh {
		if err := s.launchResearchRunner(ctx.Context, db, home, parent.project, created, prompt); err != nil {
			return err
		}
	}
	return s.printResearchOutcome(ctx, db, home, created, parsed)
}

func (s *Service) researchFollowUp(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("research follow-up", args, map[string]flagSpec{"prompt": {}, "prompt-file": {}, "key": {}, "wait": {boolean: true}, "timeout": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("research follow-up requires one attempt id")
	}
	prompt, err := researchPrompt(parsed)
	if err != nil {
		return err
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := s.reconcileResearch(ctx.Context, db); err != nil {
		return err
	}
	parent, err := s.resolveResearchParent(ctx.Context, db)
	if errors.Is(err, errResearchParentUnknown) {
		return researchParentFailure()
	}
	if err != nil {
		return err
	}
	var task *store.Task
	if parent.task.ID != 0 {
		task = &parent.task
	}
	previous, err := s.researchAttemptFor(ctx.Context, db, parent.project, task, parsed.Positionals[0])
	if err != nil {
		return err
	}
	if previous.TaskID != parent.task.ID {
		return axi.Failure("research_parent_mismatch", fmt.Sprintf("%s belongs to another parent; only its own parent may continue it", research.AttemptID(previous.ID)), false)
	}
	harness := research.Harness(previous.Harness)
	if harness == research.Claude {
		return axi.Failure("research_harness_hooks", "Claude children continue inside their parent session", false, "Use the Agent tool's native continuation from the parent session")
	}
	state := research.State(previous.State)
	if state.Active() {
		return axi.Failure("research_active", fmt.Sprintf("%s is still %s", research.AttemptID(previous.ID), previous.State), false, "Wait with `posse research wait` or cancel it first")
	}
	if state != research.Completed && state != research.Interrupted {
		return axi.Failure("research_not_resumable", fmt.Sprintf("%s is %s and cannot be continued", research.AttemptID(previous.ID), previous.State), false, "Start a new attempt with `posse research start`")
	}
	if ok, why := researchResumable(previous); !ok {
		return axi.Failure("research_not_resumable", fmt.Sprintf("%s cannot resume: %s; it remains interrupted", research.AttemptID(previous.ID), why), false, "Start a new attempt with `posse research start`")
	}
	cfg, err := s.researchConfig(home, parent.project)
	if err != nil {
		return err
	}
	if cfg.Research.Native != config.ResearchTracked {
		return axi.Failure("research_disabled", "research.native is off; Posse launches no research children", false)
	}
	attempt := store.ResearchAttempt{
		ProjectID: parent.project.ID, TaskID: parent.task.ID, ParentRole: parent.role(), ParentPID: parent.agentPID, ParentIdentity: parent.agentIdentity,
		Harness: previous.Harness, IdempotencyKey: parsed.Flags["key"], PreviousID: previous.ID, ChildID: previous.ChildID, SessionRef: previous.SessionRef,
		Model: previous.Model, Tools: researchTools(harness), Scope: previous.Scope, PromptDigest: digestText(prompt), State: string(research.Registered),
	}
	if attempt.IdempotencyKey != "" {
		if existing, err := db.ResearchAttemptByKey(ctx.Context, parent.project.ID, previous.Harness, attempt.IdempotencyKey); err == nil {
			return s.printResearchOutcome(ctx, db, home, existing, parsed)
		}
	}
	version, versionErr := research.CheckVersion(harness, researchHarnessVersion(ctx.Context, harness))
	attempt.HarnessVersion = version
	if versionErr != nil {
		return s.refuseResearch(ctx.Context, db, attempt, "research_unsupported", versionErr.Error())
	}
	// One live turn per native thread or session.
	if followers, err := db.ResearchAttempts(ctx.Context, store.ResearchAttemptFilter{ProjectID: parent.project.ID, Active: true, Harness: previous.Harness}); err != nil {
		return err
	} else {
		for _, follower := range followers {
			if follower.ChildID != "" && follower.ChildID == previous.ChildID {
				return axi.Failure("research_active", fmt.Sprintf("%s already continues this child", research.AttemptID(follower.ID)), false)
			}
		}
	}
	created, fresh, err := db.CreateResearchAttempt(ctx.Context, attempt, cfg.Research.MaxActive)
	if errors.Is(err, store.ErrResearchBudget) {
		return s.refuseResearch(ctx.Context, db, attempt, "research_budget", err.Error())
	}
	if err != nil {
		return err
	}
	if fresh {
		if err := s.launchResearchRunner(ctx.Context, db, home, parent.project, created, prompt); err != nil {
			return err
		}
	}
	return s.printResearchOutcome(ctx, db, home, created, parsed)
}

func (s *Service) researchWait(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("research wait", args, map[string]flagSpec{"timeout": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("research wait requires one attempt id")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, task, err := s.researchScope(ctx.Context, db)
	if err != nil {
		return err
	}
	attempt, err := s.researchAttemptFor(ctx.Context, db, project, task, parsed.Positionals[0])
	if err != nil {
		return err
	}
	parsed.Flags["wait"] = "true"
	return s.printResearchOutcome(ctx, db, home, attempt, parsed)
}

func researchPrompt(parsed parsedArgs) (string, error) {
	prompt, file := parsed.Flags["prompt"], parsed.Flags["prompt-file"]
	if (prompt == "") == (file == "") {
		return "", axi.Usage("pass exactly one of --prompt or --prompt-file")
	}
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", axi.Usage(fmt.Sprintf("read --prompt-file: %v", err))
		}
		prompt = string(data)
	}
	if strings.TrimSpace(prompt) == "" {
		return "", axi.Usage("the research prompt is empty")
	}
	if len(prompt) > researchPromptLimit {
		return "", axi.Usage(fmt.Sprintf("the research prompt exceeds %d bytes", researchPromptLimit))
	}
	return prompt, nil
}

func modelOrDefault(model string) string {
	if strings.TrimSpace(model) == "" {
		return "default"
	}
	return model
}

func researchTools(harness research.Harness) []string {
	switch harness {
	case research.Codex:
		return []string{"sandbox:read-only", "approval:never"}
	case research.Pi:
		return append([]string(nil), research.PiReadOnlyTools...)
	}
	return nil
}

// parentSessionHint records the parent harness session when its environment
// names one.
func parentSessionHint(harness research.Harness) string {
	for _, key := range []string{"CODEX_THREAD_ID", "PI_SESSION_ID", "CLAUDE_CODE_SESSION_ID"} {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}

func (s *Service) refuseResearch(ctx context.Context, db *store.DB, attempt store.ResearchAttempt, code, reason string) error {
	attempt.State, attempt.Note, attempt.IdempotencyKey = string(research.Refused), reason, ""
	refused, _, err := db.CreateResearchAttempt(ctx, attempt, 0)
	if err != nil {
		return err
	}
	return axi.Failure(code, fmt.Sprintf("research child refused (%s): %s", research.AttemptID(refused.ID), reason), false, "Run `posse research show "+research.AttemptID(refused.ID)+"` for the recorded refusal")
}

func (s *Service) launchResearchRunner(ctx context.Context, db *store.DB, home string, project store.Project, attempt store.ResearchAttempt, prompt string) error {
	dir := researchDirectory(home, project.Name, attempt.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, "prompt.md"), []byte(prompt)); err != nil {
		return err
	}
	if err := startResearchRunner(home, attempt.ID); err != nil {
		note := "the research runner could not start: " + err.Error()
		_, _ = db.UpdateResearchAttempt(ctx, attempt.ID, activeResearchStates, store.ResearchChange{State: researchState(research.Failed), Note: &note})
		return axi.Failure("research_start_failed", note, true)
	}
	return nil
}

// printResearchOutcome prints an attempt, after waiting for it to settle when
// --wait was given. A completed attempt includes its digest-checked result.
func (s *Service) printResearchOutcome(ctx *axi.Context, db *store.DB, home string, attempt store.ResearchAttempt, parsed parsedArgs) error {
	if parsed.Flags["wait"] != "" {
		timeout := 30 * time.Minute
		if value := parsed.Flags["timeout"]; value != "" {
			ms, err := strconv.ParseInt(value, 10, 64)
			if err != nil || ms < 1 {
				return axi.Usage("--timeout must be a positive number of milliseconds")
			}
			timeout = time.Duration(ms) * time.Millisecond
		}
		waited, err := waitResearchAttempt(ctx.Context, db, attempt.ID, timeout, func() error { return s.reconcileResearch(ctx.Context, db) })
		if err != nil {
			return err
		}
		attempt = waited
	}
	view := researchView(attempt, researchTaskSeqs(ctx.Context, db, attempt.ProjectID))
	if attempt.State == string(research.Completed) && attempt.ResultPath != "" {
		if data, err := os.ReadFile(attempt.ResultPath); err == nil && digestText(string(data)) == attempt.ResultDigest {
			view["result_text"] = string(data)
		}
	}
	help := []any{"Run `posse research show " + research.AttemptID(attempt.ID) + "` for details"}
	if research.State(attempt.State).Active() {
		help = append(help, "Run `posse research wait "+research.AttemptID(attempt.ID)+"` to wait for its result", "Run `posse research cancel "+research.AttemptID(attempt.ID)+"` to stop it")
	}
	return ctx.Print(axi.Object{{Key: "research", Value: view}, {Key: "help", Value: help}})
}

var researchWaitInterval = 250 * time.Millisecond

func waitResearchAttempt(ctx context.Context, db *store.DB, id int64, timeout time.Duration, reconcile func() error) (store.ResearchAttempt, error) {
	deadline := time.Now().Add(timeout)
	for {
		attempt, err := db.ResearchAttempt(ctx, id)
		if err != nil {
			return store.ResearchAttempt{}, err
		}
		if !research.State(attempt.State).Active() {
			return attempt, nil
		}
		if time.Now().After(deadline) {
			return attempt, nil
		}
		select {
		case <-ctx.Done():
			return attempt, nil
		case <-time.After(researchWaitInterval):
		}
		if err := reconcile(); err != nil {
			return store.ResearchAttempt{}, err
		}
	}
}

// RunResearchRunner is `posse _research-run <attempt>`: it supervises one
// process-launched child to its end.
func (s *Service) RunResearchRunner(args []string, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "posse: _research-run requires one attempt id")
		return 1
	}
	id, err := research.ParseAttemptID(args[0])
	if err != nil {
		fmt.Fprintln(stderr, "posse:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := s.runResearchAttempt(ctx, id); err != nil {
		fmt.Fprintln(stderr, "posse: research runner:", err)
		return 1
	}
	return 0
}

// researchCommand is the exact child invocation for an attempt.
type researchCommand struct {
	name    string
	args    []string
	stdin   string
	env     []string
	session string // Pi: the Posse-owned session file to continue
}

func newResearchSessionID() string {
	var data [16]byte
	_, _ = rand.Read(data[:])
	data[6] = data[6]&0x0f | 0x40
	data[8] = data[8]&0x3f | 0x80
	encoded := hex.EncodeToString(data[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

// buildResearchCommand pins the read-only settings on the command line. The
// parent's own launch arguments, including any danger-full-access sandbox or
// bypass flag, are never inherited.
func buildResearchCommand(attempt store.ResearchAttempt, dir, prompt string) (researchCommand, error) {
	env := researchChildEnvironment(os.Environ())
	switch research.Harness(attempt.Harness) {
	case research.Codex:
		readOnly := []string{"-c", `sandbox_mode="read-only"`, "-c", `approval_policy="never"`}
		var args []string
		if attempt.PreviousID != 0 {
			if attempt.ChildID == "" {
				return researchCommand{}, fmt.Errorf("no Codex thread to resume")
			}
			args = append([]string{"exec", "resume", "--json", "--skip-git-repo-check"}, readOnly...)
			args = append(args, attempt.ChildID, "-")
		} else {
			args = append([]string{"exec", "--json", "--skip-git-repo-check", "--sandbox", "read-only", "--cd", attempt.Scope}, readOnly...)
			if attempt.Model != "" && attempt.Model != "default" {
				args = append(args, "--model", attempt.Model)
			}
			args = append(args, "-")
		}
		return researchCommand{name: "codex", args: args, stdin: prompt, env: env}, nil
	case research.Pi:
		args := []string{"--mode", "json", "-p", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-mcp", "--no-approve", "--offline",
			"--tools", strings.Join(research.PiReadOnlyTools, ",")}
		command := researchCommand{name: "pi", env: env}
		if attempt.PreviousID != 0 {
			if attempt.SessionRef == "" || !regularFileExists(attempt.SessionRef) {
				return researchCommand{}, fmt.Errorf("no persisted Pi session to resume")
			}
			args = append(args, "--session-dir", filepath.Dir(attempt.SessionRef), "--session", attempt.SessionRef)
			command.session = attempt.SessionRef
		} else {
			args = append(args, "--session-dir", filepath.Join(dir, "sessions"), "--session-id", attempt.ChildID)
		}
		if attempt.Model != "" && attempt.Model != "default" {
			args = append(args, "--model", attempt.Model)
		}
		command.args = append(args, "--", prompt)
		return command, nil
	}
	return researchCommand{}, fmt.Errorf("no process adapter for %s", attempt.Harness)
}

// researchChildEnvironment drops every Herdr binding and parent session
// identity, and marks the child as a Worker of the Posse home so it cannot
// act as the Lead.
func researchChildEnvironment(environ []string) []string {
	env := []string{}
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "HERDR_") || agentEnvironmentKey(key) || key == "CLAUDE_CODE_SESSION_ID" || key == workerHomeEnv {
			continue
		}
		env = append(env, entry)
	}
	if home := os.Getenv("POSSE_HOME"); home != "" {
		env = append(env, workerHomeEnv+"="+home)
	}
	return env
}

// boundedLog writes at most limit bytes and reports overflow.
type boundedLog struct {
	file    *os.File
	limit   int64
	written int64
	over    bool
}

func (l *boundedLog) write(line []byte) bool {
	if l.written+int64(len(line))+1 > l.limit {
		l.over = true
		return false
	}
	n, _ := l.file.Write(append(line, '\n'))
	l.written += int64(n)
	return true
}

// tailBuffer keeps the last bytes of a stream for failure notes.
type tailBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > 4096 {
		b.data = b.data[len(b.data)-4096:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.data))
}

func (s *Service) runResearchAttempt(ctx context.Context, id int64) error {
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	attempt, err := db.ResearchAttempt(ctx, id)
	if err != nil {
		return err
	}
	if attempt.State != string(research.Registered) || attempt.RunnerPID != 0 {
		return fmt.Errorf("%s is %s and already has a runner", research.AttemptID(id), attempt.State)
	}
	project, err := db.ProjectByID(ctx, attempt.ProjectID)
	if err != nil {
		return err
	}
	cfg, err := s.researchConfig(home, project)
	if err != nil {
		return err
	}
	dir := researchDirectory(home, project.Name, id)
	prompt, err := os.ReadFile(filepath.Join(dir, "prompt.md"))
	if err != nil {
		return s.failResearch(ctx, db, id, "read the research prompt: "+err.Error())
	}
	pid := os.Getpid()
	identity, err := researchProcessIdentity(pid)
	if err != nil {
		return s.failResearch(ctx, db, id, "identify the research runner: "+err.Error())
	}
	start := store.ResearchChange{State: researchState(research.Running), RunnerPID: &pid, RunnerIdentity: &identity}
	if research.Harness(attempt.Harness) == research.Pi && attempt.PreviousID == 0 {
		child := newResearchSessionID()
		start.ChildID = &child
		attempt.ChildID = child
	}
	if attempt, err = db.UpdateResearchAttempt(ctx, id, []string{string(research.Registered)}, start); err != nil {
		return err
	}
	command, err := buildResearchCommand(attempt, dir, string(prompt))
	if err != nil {
		return s.finishResearch(ctx, db, id, research.Interrupted, err.Error(), nil)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		return s.failResearch(ctx, db, id, err.Error())
	}
	timeout, _ := time.ParseDuration(cfg.Research.Timeout)
	childCtx, cancelChild := context.WithTimeout(ctx, timeout)
	defer cancelChild()
	verifier := newResearchVerifier(attempt)
	child := execgroup.CommandContext(childCtx, command.name, command.args...)
	child.Dir, child.Env, child.Stdin = attempt.Scope, command.env, strings.NewReader(command.stdin)
	stderr := &tailBuffer{}
	child.Stderr = stderr
	stdout, err := child.StdoutPipe()
	if err != nil {
		return s.failResearch(ctx, db, id, err.Error())
	}
	sessionSize := int64(-1)
	if command.session != "" {
		if info, err := os.Stat(command.session); err == nil {
			sessionSize = info.Size()
		}
	}
	if err := child.Start(); err != nil {
		return s.failResearch(ctx, db, id, fmt.Sprintf("start %s: %v", command.name, err))
	}
	if attempt.ChildID != "" {
		// A follow-up continues a known thread; verify its new turn.
		verifier.start(childCtx, attempt.ChildID)
	}
	childPID := child.Process.Pid
	if childIdentity, err := researchProcessIdentity(childPID); err == nil {
		_, _ = db.UpdateResearchAttempt(ctx, id, nil, store.ResearchChange{ChildPID: &childPID, ChildIdentity: &childIdentity})
	}
	events, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		cancelChild()
		_ = child.Wait()
		return s.failResearch(ctx, db, id, err.Error())
	}
	defer events.Close()
	log := &boundedLog{file: events, limit: int64(cfg.Research.MaxOutputBytes)}
	parser := research.NewEventParser(research.Harness(attempt.Harness))
	recorded := attempt.ChildID
	var refusal string
	reader := bufio.NewReaderSize(stdout, 64<<10)
	for {
		line, readErr := readBoundedLine(reader, cfg.Research.MaxOutputBytes)
		if len(line) > 0 {
			if !log.write(line) {
				cancelChild()
				break
			}
			parser.Consume(line)
			progress := parser.Progress()
			if progress.ChildID != "" && progress.ChildID != recorded {
				change := store.ResearchChange{}
				if recorded == "" {
					change.ChildID = &progress.ChildID
				} else if research.Harness(attempt.Harness) == research.Pi {
					// Pi announces the session it opened; it must be ours.
					refusal = fmt.Sprintf("Pi opened session %s instead of the Posse-owned session %s", progress.ChildID, recorded)
					cancelChild()
					break
				}
				recorded = progress.ChildID
				if _, err := db.UpdateResearchAttempt(ctx, id, activeResearchStates, change); err != nil {
					cancelChild()
					break
				}
				verifier.start(childCtx, recorded)
			}
			if reason := verifier.refusal(); reason != "" {
				refusal = reason
				cancelChild()
				break
			}
		}
		if readErr != nil {
			if errors.Is(readErr, errResearchLineTooLong) {
				log.over = true
				cancelChild()
			}
			break
		}
	}
	// Stop what is left of the child's own process group before draining.
	// The unreaped child keeps its PID, so the group cannot be another's.
	if errors.Is(childCtx.Err(), context.Canceled) || errors.Is(childCtx.Err(), context.DeadlineExceeded) || log.over {
		_ = syscall.Kill(-childPID, syscall.SIGKILL)
	}
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := child.Wait()
	// A child that failed before announcing a thread never ran a turn.
	if refusal == "" && (waitErr == nil || recorded != "") {
		refusal = verifier.finish(recorded)
	}
	progress := parser.Progress()
	usage := progress.Usage.JSON()
	change := store.ResearchChange{Usage: &usage}
	if progress.Model != "" && (attempt.Model == "" || attempt.Model == "default") {
		change.Model = &progress.Model
	}
	if research.Harness(attempt.Harness) == research.Pi {
		if session := piSessionFile(attempt, dir, command.session); session != "" {
			change.SessionRef = &session
		}
	}
	switch {
	case refusal != "":
		return s.finishResearch(ctx, db, id, research.Refused, refusal+"; the child was stopped and its result was not accepted", &change)
	case ctx.Err() != nil:
		// The runner itself was stopped: cancellation is already recorded.
		return s.finishResearch(ctx, db, id, research.Interrupted, "the research runner was stopped before the child finished", &change)
	case log.over:
		return s.finishResearch(ctx, db, id, research.Failed, fmt.Sprintf("the child exceeded research.max_output_bytes (%d) and was stopped", cfg.Research.MaxOutputBytes), &change)
	case errors.Is(childCtx.Err(), context.DeadlineExceeded):
		return s.finishResearch(ctx, db, id, research.Failed, "the child exceeded research.timeout ("+cfg.Research.Timeout+") and was stopped", &change)
	case waitErr != nil || progress.Error != "":
		reason := progress.Error
		if reason == "" {
			reason = firstLine(stderr.String())
		}
		if reason == "" && waitErr != nil {
			reason = waitErr.Error()
		}
		return s.finishResearch(ctx, db, id, research.Failed, "the child failed: "+reason, &change)
	case strings.TrimSpace(progress.Result) == "":
		return s.finishResearch(ctx, db, id, research.Failed, "the child produced no answer", &change)
	}
	if command.session != "" {
		// Resume is verified only when Pi appended to the same session file.
		if info, err := os.Stat(command.session); err != nil || info.Size() <= sessionSize {
			return s.finishResearch(ctx, db, id, research.Interrupted, "Pi did not continue the recorded session, so resume is unverified", &change)
		}
	}
	result, err := writeResearchResult(dir, progress.Result, cfg.Research.MaxResultBytes)
	if err != nil {
		return s.failResearch(ctx, db, id, err.Error())
	}
	result.Usage, result.Model, result.SessionRef = change.Usage, change.Model, change.SessionRef
	completed := store.ResearchAttempt{Harness: attempt.Harness, ChildID: recorded}
	if result.SessionRef != nil {
		completed.SessionRef = *result.SessionRef
	}
	resumable, _ := researchResumable(completed)
	result.Resumable = &resumable
	return s.finishResearch(ctx, db, id, research.Completed, "", &result)
}

var errResearchLineTooLong = errors.New("research event line exceeds the output limit")

func readBoundedLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := reader.ReadLine()
		line = append(line, chunk...)
		if len(line) > limit {
			return nil, errResearchLineTooLong
		}
		if err != nil || !isPrefix {
			return line, err
		}
	}
}

// piSessionFile finds the Posse-owned session file the child wrote.
func piSessionFile(attempt store.ResearchAttempt, dir, existing string) string {
	if existing != "" {
		return existing
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "sessions", "*_"+attempt.ChildID+".jsonl"))
	sort.Strings(matches)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}

func (s *Service) failResearch(ctx context.Context, db *store.DB, id int64, note string) error {
	return s.finishResearch(ctx, db, id, research.Failed, note, nil)
}

// finishResearch records the terminal state unless the attempt was already
// cancelled or refused; a cancelled attempt keeps its state and gains usage.
func (s *Service) finishResearch(ctx context.Context, db *store.DB, id int64, state research.State, note string, change *store.ResearchChange) error {
	update := store.ResearchChange{}
	if change != nil {
		update = *change
	}
	update.State = researchState(state)
	if note != "" {
		update.Note = &note
	}
	if state == research.Interrupted {
		attempt, err := db.ResearchAttempt(ctx, id)
		if err == nil {
			if update.SessionRef != nil {
				attempt.SessionRef = *update.SessionRef
			}
			resumable, _ := researchResumable(attempt)
			update.Resumable = &resumable
		}
	}
	// Detach from the runner's context: a stopped runner still records.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, err := db.UpdateResearchAttempt(writeCtx, id, activeResearchStates, update)
	if errors.Is(err, store.ErrStateRace) {
		late := store.ResearchChange{Usage: update.Usage, SessionRef: update.SessionRef}
		_, err = db.UpdateResearchAttempt(writeCtx, id, nil, late)
	}
	return err
}

// researchVerifier confirms a Codex child really runs read-only from the
// sandbox policy Codex records for every turn of its thread.
type researchVerifier struct {
	harness  research.Harness
	baseline int
	mu       sync.Mutex
	verdict  string
	verified bool
	started  bool
	done     chan struct{}
}

// codexVerifyTimeout bounds how long Posse waits for Codex to record the
// sandbox of the child's turn.
var codexVerifyTimeout = 20 * time.Second

func newResearchVerifier(attempt store.ResearchAttempt) *researchVerifier {
	verifier := &researchVerifier{harness: research.Harness(attempt.Harness), done: make(chan struct{})}
	if verifier.harness == research.Codex && attempt.ChildID != "" {
		policies, _ := codexTurnPolicies(attempt.ChildID)
		verifier.baseline = len(policies)
	}
	return verifier
}

func (v *researchVerifier) start(ctx context.Context, thread string) {
	if v.harness != research.Codex {
		return
	}
	v.mu.Lock()
	if v.started {
		v.mu.Unlock()
		return
	}
	v.started = true
	v.mu.Unlock()
	go func() {
		defer close(v.done)
		deadline := time.Now().Add(codexVerifyTimeout)
		for {
			if verdict, ok := v.check(thread); ok {
				v.mu.Lock()
				v.verdict, v.verified = verdict, true
				v.mu.Unlock()
				return
			}
			if time.Now().After(deadline) {
				v.mu.Lock()
				v.verdict, v.verified = "Codex recorded no sandbox policy for the child's turn, so read-only could not be verified", true
				v.mu.Unlock()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
}

func (v *researchVerifier) check(thread string) (string, bool) {
	policies, err := codexTurnPolicies(thread)
	if err != nil || len(policies) <= v.baseline {
		return "", false
	}
	for _, policy := range policies[v.baseline:] {
		if policy != "read-only" {
			return fmt.Sprintf("Codex ran the child with sandbox %q instead of read-only", policy), true
		}
	}
	return "", true
}

func (v *researchVerifier) refusal() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.verdict
}

// finish settles verification after the child exited. A Codex child that never
// announced a thread, or whose turn sandbox is unrecorded, is not accepted.
func (v *researchVerifier) finish(thread string) string {
	if v.harness != research.Codex {
		return ""
	}
	if thread == "" {
		return "Codex announced no thread, so its sandbox could not be verified"
	}
	v.mu.Lock()
	started := v.started
	v.mu.Unlock()
	if started {
		<-v.done
	}
	if verdict, ok := v.check(thread); ok {
		return verdict
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.verified {
		return "Codex recorded no sandbox policy for the child's turn, so read-only could not be verified"
	}
	return v.verdict
}

func codexHome() string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// codexTurnPolicies returns the sandbox policy type of each turn Codex
// recorded in the thread's rollout file, in order.
func codexTurnPolicies(thread string) ([]string, error) {
	if !plainResearchID(thread) {
		return nil, fmt.Errorf("invalid Codex thread id %q", thread)
	}
	matches, err := filepath.Glob(filepath.Join(codexHome(), "sessions", "*", "*", "*", "rollout-*-"+thread+".jsonl"))
	if err != nil || len(matches) == 0 {
		return nil, fmt.Errorf("no rollout for thread %s", thread)
	}
	sort.Strings(matches)
	file, err := os.Open(matches[len(matches)-1])
	if err != nil {
		return nil, err
	}
	defer file.Close()
	policies := []string{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), 32<<20)
	for scanner.Scan() {
		if !strings.Contains(scanner.Text(), `"turn_context"`) {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Payload struct {
				SandboxPolicy struct {
					Type string `json:"type"`
				} `json:"sandbox_policy"`
			} `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.Type == "turn_context" {
			policies = append(policies, entry.Payload.SandboxPolicy.Type)
		}
	}
	return policies, scanner.Err()
}

func plainResearchID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
