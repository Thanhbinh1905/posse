package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/research"
	"github.com/thanhbinh1905/posse/internal/store"
)

// researchParent is the Lead or Worker that owns a research child.
type researchParent struct {
	project store.Project
	task    store.Task // zero for the Lead
	// agentPID and agentIdentity name the parent harness process instance.
	agentPID      int
	agentIdentity string
}

func (p researchParent) role() string {
	if p.task.ID != 0 {
		return research.ParentWorker
	}
	return research.ParentLead
}

func (p researchParent) label() string {
	if p.task.ID != 0 {
		return taskIDString(p.task.Seq)
	}
	return "Lead"
}

var errResearchParentUnknown = errors.New("research parent unknown")

// researchProcessIdentity names one process instance: boot ID and start time.
var researchProcessIdentity = func(pid int) (string, error) {
	boot, start, err := store.ProcessIdentityForPID(pid)
	if err != nil {
		return "", err
	}
	return boot + "/" + start, nil
}

// researchProcessAlive reports whether pid is still the recorded instance.
func researchProcessAlive(pid int, identity string) bool {
	if pid < 1 || identity == "" {
		return false
	}
	current, err := researchProcessIdentity(pid)
	return err == nil && current == identity
}

// resolveResearchParent finds the Lead or Worker this process acts for: the
// pane bindings of it and its ancestors, then a Worker Sandbox it runs in.
func (s *Service) resolveResearchParent(ctx context.Context, db *store.DB) (researchParent, error) {
	ancestors := processAncestors(procRoot, os.Getpid())
	panes := []string{os.Getenv("HERDR_PANE_ID")}
	for _, process := range ancestors {
		panes = append(panes, process.env["HERDR_PANE_ID"])
	}
	var parent researchParent
	found := false
	for _, pane := range panes {
		if pane == "" {
			continue
		}
		if task, err := db.TaskByPane(ctx, pane); err == nil {
			project, err := db.ProjectByID(ctx, task.ProjectID)
			if err != nil {
				return researchParent{}, err
			}
			parent, found = researchParent{project: project, task: task}, true
			break
		} else if !store.IsNotFound(err) {
			return researchParent{}, err
		}
		if project, err := db.ProjectByLeadPane(ctx, pane); err == nil {
			parent, found = researchParent{project: project}, true
			break
		} else if !store.IsNotFound(err) {
			return researchParent{}, err
		}
	}
	if !found {
		dir, err := currentDir()
		if err != nil {
			return researchParent{}, err
		}
		task, err := taskForPath(ctx, db, dir)
		if err != nil {
			return researchParent{}, errResearchParentUnknown
		}
		project, err := db.ProjectByID(ctx, task.ProjectID)
		if err != nil {
			return researchParent{}, err
		}
		parent = researchParent{project: project, task: task}
	}
	for _, process := range ancestors {
		if process.agent {
			parent.agentPID = process.pid
			parent.agentIdentity, _ = researchProcessIdentity(process.pid)
			break
		}
	}
	return parent, nil
}

func researchParentFailure() error {
	return axi.Failure("research_parent_unknown", "research children need a Posse Lead or Worker parent", false, "Run from the Lead pane or a Worker pane of a registered Project")
}

// researchDirectory holds an attempt's prompt, bounded event log, result and
// harness session files. It lives under POSSE_HOME, never in the checkout.
func researchDirectory(home, project string, id int64) string {
	return filepath.Join(home, "projects", project, "research", research.AttemptID(id))
}

func digestText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// writeResearchResult stores at most limit bytes of a child's answer and
// records whether it was truncated.
func writeResearchResult(dir, text string, limit int) (store.ResearchChange, error) {
	truncated := false
	if limit > 0 && len(text) > limit {
		text = strings.ToValidUTF8(text[:limit], "")
		truncated = true
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return store.ResearchChange{}, err
	}
	path := filepath.Join(dir, "result.md")
	if err := writeFile(path, []byte(text)); err != nil {
		return store.ResearchChange{}, err
	}
	size := int64(len(text))
	digest := digestText(text)
	return store.ResearchChange{ResultPath: &path, ResultBytes: &size, ResultDigest: &digest, ResultTruncated: &truncated}, nil
}

func stringPtr(value string) *string { return &value }

func researchState(state research.State) *string { return stringPtr(string(state)) }

var activeResearchStates = []string{string(research.Registered), string(research.Running)}

// reconcileResearch records children whose parent or runner instance is gone.
// A child with a resumable native session is interrupted and resumable;
// otherwise it is interrupted without resume. Nothing is relaunched.
func (s *Service) reconcileResearch(ctx context.Context, db *store.DB) error {
	attempts, err := db.ActiveResearchAttempts(ctx)
	if err != nil {
		return err
	}
	for _, attempt := range attempts {
		note, resumable, lost := researchLoss(attempt)
		if !lost {
			continue
		}
		_, err := db.UpdateResearchAttempt(ctx, attempt.ID, activeResearchStates, store.ResearchChange{State: researchState(research.Interrupted), Resumable: &resumable, Note: &note})
		if err != nil && !errors.Is(err, store.ErrStateRace) {
			return err
		}
	}
	return nil
}

// researchRunnerStartGrace is how long a registered process-launched child
// may wait for its runner to record itself.
const researchRunnerStartGrace = time.Minute

func researchLoss(attempt store.ResearchAttempt) (string, bool, bool) {
	switch research.Harness(attempt.Harness) {
	case research.Claude:
		if attempt.ParentPID == 0 || researchProcessAlive(attempt.ParentPID, attempt.ParentIdentity) {
			return "", false, false
		}
		if attempt.ChildID == "" {
			return "parent Claude session exited before the child started", false, true
		}
		resumable := attempt.SessionRef != "" && regularFileExists(attempt.SessionRef)
		if resumable {
			return "parent Claude session exited; resume the parent session to continue the child", true, true
		}
		return "parent Claude session exited and the child transcript is unavailable", false, true
	default:
		if attempt.RunnerPID == 0 {
			if time.Since(time.UnixMilli(attempt.CreatedAt)) < researchRunnerStartGrace {
				return "", false, false
			}
			return "the research runner never started", false, true
		}
		if researchProcessAlive(attempt.RunnerPID, attempt.RunnerIdentity) {
			return "", false, false
		}
		if researchProcessAlive(attempt.ChildPID, attempt.ChildIdentity) {
			// The runner died but the child still runs; cancel stops it.
			return "", false, false
		}
		resumable, why := researchResumable(attempt)
		if resumable {
			return "the research runner exited before the child finished; `posse research follow-up` resumes its session", true, true
		}
		return "the research runner exited before the child finished; " + why, false, true
	}
}

// researchResumable reports whether a process-launched child's native session
// can continue. Codex threads resume by ID; Pi resumes only a persisted
// Posse-owned session file.
func researchResumable(attempt store.ResearchAttempt) (bool, string) {
	switch research.Harness(attempt.Harness) {
	case research.Codex:
		if attempt.ChildID == "" {
			return false, "Codex never announced a thread"
		}
		return true, ""
	case research.Pi:
		if attempt.SessionRef == "" || !regularFileExists(attempt.SessionRef) {
			return false, "no persisted Pi session was recorded"
		}
		return true, ""
	default:
		return false, "resume is unsupported"
	}
}

func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func (s *Service) researchConfig(home string, project store.Project) (config.Config, error) {
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return config.Config{}, configFailure(err)
	}
	return cfg, nil
}

func configFailure(err error) error {
	var invalid *config.InvalidError
	if errors.As(err, &invalid) {
		return axi.Failure("config_invalid", err.Error(), false, "Run `posse config show --effective` to inspect configuration")
	}
	return err
}

// researchView renders one attempt for CLI output.
func researchView(attempt store.ResearchAttempt, tasks map[int64]int) map[string]any {
	parent := "Lead"
	if attempt.TaskID != 0 {
		parent = taskIDString(tasks[attempt.TaskID])
	}
	view := map[string]any{
		"id":        research.AttemptID(attempt.ID),
		"parent":    parent,
		"harness":   attempt.Harness,
		"state":     attempt.State,
		"access":    attempt.Access,
		"model":     attempt.Model,
		"scope":     attempt.Scope,
		"usage":     research.ParseUsage(attempt.Usage).Summary(),
		"resumable": attempt.Resumable,
	}
	if attempt.AgentType != "" {
		view["agent"] = attempt.AgentType
	}
	if attempt.ChildID != "" {
		view["child"] = attempt.ChildID
	}
	if attempt.PreviousID != 0 {
		view["follows"] = research.AttemptID(attempt.PreviousID)
	}
	if attempt.ResultPath != "" {
		view["result"] = attempt.ResultPath
		view["result_bytes"] = attempt.ResultBytes
		if attempt.ResultTruncated {
			view["result_truncated"] = true
		}
	}
	if attempt.Note != "" {
		view["note"] = attempt.Note
	}
	return view
}

func researchTaskSeqs(ctx context.Context, db *store.DB, projectID int64) map[int64]int {
	seqs := map[int64]int{}
	if tasks, err := db.Tasks(ctx, projectID, true); err == nil {
		for _, task := range tasks {
			seqs[task.ID] = task.Seq
		}
	}
	return seqs
}

// researchScope resolves the caller for inspection commands: a Lead or Worker
// parent, or the User in a registered Project folder.
func (s *Service) researchScope(ctx context.Context, db *store.DB) (store.Project, *store.Task, error) {
	parent, err := s.resolveResearchParent(ctx, db)
	if err == nil {
		if parent.task.ID != 0 {
			return parent.project, &parent.task, nil
		}
		return parent.project, nil, nil
	}
	if !errors.Is(err, errResearchParentUnknown) {
		return store.Project{}, nil, err
	}
	project, err := s.projectForCWD(ctx, db)
	return project, nil, err
}

func (s *Service) researchAttemptFor(ctx context.Context, db *store.DB, project store.Project, task *store.Task, ref string) (store.ResearchAttempt, error) {
	id, err := research.ParseAttemptID(ref)
	if err != nil {
		return store.ResearchAttempt{}, axi.Usage(err.Error(), "Run `posse research list` to list attempts")
	}
	attempt, err := db.ResearchAttempt(ctx, id)
	if store.IsNotFound(err) || err == nil && (attempt.ProjectID != project.ID || task != nil && attempt.TaskID != task.ID) {
		return store.ResearchAttempt{}, axi.Failure("research_unknown", fmt.Sprintf("unknown research attempt %s", ref), false, "Run `posse research list` to list attempts this caller may see")
	}
	return attempt, err
}

func (s *Service) researchList(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("research list", args, map[string]flagSpec{"all": {boolean: true}, "task": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("research list takes no positional arguments")
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := s.reconcileResearch(ctx.Context, db); err != nil {
		return err
	}
	project, task, err := s.researchScope(ctx.Context, db)
	if err != nil {
		return err
	}
	filter := store.ResearchAttemptFilter{ProjectID: project.ID, Active: parsed.Flags["all"] == "", Limit: 200}
	if task != nil {
		filter.TaskID = task.ID
	} else if ref := parsed.Flags["task"]; ref != "" {
		selected, err := s.currentTask(ctx.Context, db, project, ref)
		if err != nil {
			return err
		}
		filter.TaskID = selected.ID
	}
	attempts, err := db.ResearchAttempts(ctx.Context, filter)
	if err != nil {
		return err
	}
	seqs := researchTaskSeqs(ctx.Context, db, project.ID)
	rows := make([]map[string]any, 0, len(attempts))
	for _, attempt := range attempts {
		rows = append(rows, researchView(attempt, seqs))
	}
	help := []any{"Run `posse research show <attempt>` for details and the result path"}
	if filter.Active {
		help = append(help, "Add --all to include finished attempts")
	}
	return ctx.Print(axi.Object{{Key: "research", Value: rows}, {Key: "help", Value: help}})
}

func (s *Service) researchShow(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("research show", args, map[string]flagSpec{"result": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("research show requires one attempt id", "Run `posse research list` to list attempts")
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := s.reconcileResearch(ctx.Context, db); err != nil {
		return err
	}
	project, task, err := s.researchScope(ctx.Context, db)
	if err != nil {
		return err
	}
	attempt, err := s.researchAttemptFor(ctx.Context, db, project, task, parsed.Positionals[0])
	if err != nil {
		return err
	}
	view := researchView(attempt, researchTaskSeqs(ctx.Context, db, project.ID))
	view["tools"] = attempt.Tools
	view["harness_version"] = attempt.HarnessVersion
	if attempt.ParentSession != "" {
		view["parent_session"] = attempt.ParentSession
	}
	if attempt.SessionRef != "" {
		view["session"] = attempt.SessionRef
	}
	view["created_at"] = time.UnixMilli(attempt.CreatedAt).UTC().Format(time.RFC3339)
	if attempt.EndedAt != 0 {
		view["ended_at"] = time.UnixMilli(attempt.EndedAt).UTC().Format(time.RFC3339)
	}
	if parsed.Flags["result"] != "" {
		if attempt.ResultPath == "" {
			return axi.Failure("research_no_result", fmt.Sprintf("%s has no accepted result", research.AttemptID(attempt.ID)), false)
		}
		data, err := os.ReadFile(attempt.ResultPath)
		if err != nil {
			return err
		}
		if digestText(string(data)) != attempt.ResultDigest {
			return axi.Failure("research_result_changed", fmt.Sprintf("%s result no longer matches its recorded digest", attempt.ResultPath), false)
		}
		view["result_text"] = string(data)
	}
	return ctx.Print(view)
}

func (s *Service) researchCancel(ctx *axi.Context, args []string) error {
	if len(args) != 1 {
		return axi.Usage("research cancel requires one attempt id")
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := s.reconcileResearch(ctx.Context, db); err != nil {
		return err
	}
	project, task, err := s.researchScope(ctx.Context, db)
	if err != nil {
		return err
	}
	attempt, err := s.researchAttemptFor(ctx.Context, db, project, task, args[0])
	if err != nil {
		return err
	}
	cancelled, err := s.cancelResearchAttempt(ctx.Context, db, attempt, "cancelled by `posse research cancel`")
	if err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "research", Value: researchView(cancelled, researchTaskSeqs(ctx.Context, db, project.ID))}})
}

// cancelResearchAttempt records the cancellation first, so output arriving
// after it is never accepted, then stops the exact owned process instance.
// A Claude child runs inside its parent; its next tool call is refused.
func (s *Service) cancelResearchAttempt(ctx context.Context, db *store.DB, attempt store.ResearchAttempt, note string) (store.ResearchAttempt, error) {
	if !research.State(attempt.State).Active() {
		return attempt, axi.Failure("research_not_active", fmt.Sprintf("%s is %s, not registered or running", research.AttemptID(attempt.ID), attempt.State), false)
	}
	if research.Harness(attempt.Harness) == research.Claude {
		note += "; Posse refuses the child's further tool calls and discards its result"
	}
	cancelled, err := db.UpdateResearchAttempt(ctx, attempt.ID, activeResearchStates, store.ResearchChange{State: researchState(research.Cancelled), Note: &note})
	if err != nil {
		if errors.Is(err, store.ErrStateRace) {
			return attempt, axi.Failure("research_state_changed", fmt.Sprintf("%s changed state concurrently", research.AttemptID(attempt.ID)), true, "Run `posse research show` and retry")
		}
		return attempt, err
	}
	if research.Harness(attempt.Harness) != research.Claude {
		if err := stopResearchProcesses(cancelled); err != nil {
			return cancelled, axi.Failure("research_stop_failed", err.Error(), true, "Retry `posse research cancel`; Posse signals only the recorded process instances")
		}
	}
	return cancelled, nil
}

// stopResearchProcesses signals the recorded runner and child instances. A
// PID reused by another process is never signalled. The child leads the
// process group its runner created, so the group is stopped with it.
func stopResearchProcesses(attempt store.ResearchAttempt) error {
	var errs []error
	for _, process := range []struct {
		pid      int
		identity string
		group    bool
	}{{attempt.RunnerPID, attempt.RunnerIdentity, false}, {attempt.ChildPID, attempt.ChildIdentity, true}} {
		if process.pid < 1 || process.identity == "" || process.pid == os.Getpid() {
			continue
		}
		handle, err := openSandboxProcessHandle(process.pid)
		if err != nil {
			if !researchProcessAlive(process.pid, process.identity) {
				continue
			}
			errs = append(errs, err)
			continue
		}
		if handle.Identity() == process.identity {
			errs = append(errs, handle.Signal(syscall.SIGTERM))
			if group, err := sandboxProcessGroupID(process.pid); process.group && err == nil && group == process.pid {
				if err := syscall.Kill(-group, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
					errs = append(errs, err)
				}
			}
		}
		_ = handle.Close()
	}
	return errors.Join(errs...)
}

// taskResearchViews lists a Task's recent research children for status views.
func taskResearchViews(ctx context.Context, db *store.DB, task store.Task) ([]map[string]any, error) {
	attempts, err := db.ResearchAttempts(ctx, store.ResearchAttemptFilter{ProjectID: task.ProjectID, TaskID: task.ID, Limit: 20})
	if err != nil {
		return nil, err
	}
	views := make([]map[string]any, 0, len(attempts))
	for _, attempt := range attempts {
		views = append(views, researchView(attempt, map[int64]int{task.ID: task.Seq}))
	}
	return views, nil
}
