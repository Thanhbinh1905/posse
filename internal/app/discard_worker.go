package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// Discard retains only the current launch and its proved descendants. Persist
// their start identities in the intent so crash recovery cannot retarget a
// replacement. Shells are retained for verification, never signalled.
type discardProcess struct {
	PID      int    `json:"pid"`
	Identity string `json:"identity"`
}

type discardPane struct {
	Pane      herdr.Pane       `json:"pane"`
	Shell     discardProcess   `json:"shell"`
	AgentPID  int              `json:"agent_pid"`
	Processes []discardProcess `json:"processes"`
	shell     *sandboxProcessHandle
	agents    *sandboxProcessAuthorization
}

type discardWorkerPlan struct {
	Generation string        `json:"generation"`
	Launch     int           `json:"launch"`
	AgentName  string        `json:"agent_name"`
	Panes      []discardPane `json:"panes"`
}

type discardIntentPayload struct {
	Discard                  bool               `json:"discard"`
	UserQuote                string             `json:"user_quote"`
	KeepPullRequests         bool               `json:"keep_pull_requests,omitempty"`
	Worker                   *discardWorkerPlan `json:"worker,omitempty"`
	DiscardApprovalRecorded  bool               `json:"discard_approval_recorded,omitempty"`
	DiscardApprovalBranchSHA string             `json:"discard_approval_branch_sha,omitempty"`
}

// UnmarshalJSON retains the exact-instance authority recorded before the
// vocabulary cut. This is a stored-intent reader, not a new-input alias.
func (payload *discardIntentPayload) UnmarshalJSON(data []byte) error {
	type currentPayload discardIntentPayload
	var current currentPayload
	if err := json.Unmarshal(data, &current); err != nil {
		return err
	}
	var legacy struct {
		Worker *discardWorkerPlan `json:"rider"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	if current.Worker != nil && legacy.Worker != nil {
		return fmt.Errorf("discard intent has conflicting historical and current Worker bindings")
	}
	if current.Worker == nil {
		current.Worker = legacy.Worker
	}
	*payload = discardIntentPayload(current)
	return nil
}

func discardIdentityRefused(reason string) error {
	return axi.Failure("discard_agent_identity_unavailable", reason, true, "Inspect `posse peek <task>` and preserve any replacement or unrelated work", "Retry `posse teardown <task> --discard --user-approved \"<User's original words>\"` only after verifying Worker ownership")
}

func (plan *discardWorkerPlan) Close() {
	for i := range plan.Panes {
		pane := &plan.Panes[i]
		if pane.shell != nil {
			_ = pane.shell.Close()
		}
		pane.agents.Close()
	}
}

func (s *Service) retainDiscardWorker(ctx context.Context, db *store.DB, project store.Project, task store.Task, intent store.Intent, panes taskPaneTeardownPlan) (*discardWorkerPlan, error) {
	if capability := processIdentityCapabilityError(); capability != "" {
		return nil, discardIdentityRefused(capability)
	}
	var payload discardIntentPayload
	if err := json.Unmarshal([]byte(intent.PayloadJSON), &payload); err != nil {
		return nil, err
	}
	plan := payload.Worker
	if plan == nil {
		snapshot, err := s.snapshot(ctx)
		if err != nil {
			return nil, discardIdentityRefused(err.Error())
		}
		plan = &discardWorkerPlan{Generation: snapshot.ServerStartedAt, Launch: task.Launches, AgentName: task.AgentName}
		for _, pane := range snapshot.Panes {
			if pane.Label == task.PaneLabel && !ownsTaskPane(snapshot, project, task, taskTabs(snapshot, project, task), pane) {
				return nil, axi.Failure("discard_live_agent", "Task pane belongs to a foreign Worker; approved discard refused", false, "Preserve the foreign agent and its work")
			}
			if !panes.paneIDs[pane.PaneID] {
				continue
			}
			info, err := s.sandboxPaneProcessInfo(ctx, pane.PaneID)
			if err != nil {
				return nil, discardIdentityRefused(err.Error())
			}
			bootID, startTime, err := store.ProcessIdentityForPID(info.ShellPID)
			if err != nil {
				return nil, discardIdentityRefused(err.Error())
			}
			bound := discardPane{Pane: pane, Shell: discardProcess{PID: info.ShellPID, Identity: bootID + "/" + startTime}}
			if info.ForegroundProcessGroup > 1 && info.ForegroundProcessGroup != info.ShellPID {
				if pane.Agent == "" || task.AgentName == "" || snapshotAgentName(snapshot, pane.PaneID) != task.AgentName || task.AgentName != agentName(project.Name, task.Seq, task.Launches) {
					return nil, discardIdentityRefused("Task pane does not contain its current Worker launch")
				}
				name, cwd, found := foregroundProcess(info, info.ForegroundProcessGroup)
				if !found || name != pane.Agent || !pathInside(cwd, task.WorktreePath) {
					return nil, discardIdentityRefused("Herdr foreground process does not match the Task Worker")
				}
				bound.AgentPID = info.ForegroundProcessGroup
				bootID, startTime, err := store.ProcessIdentityForPID(bound.AgentPID)
				if err != nil {
					return nil, discardIdentityRefused(err.Error())
				}
				bound.Processes = []discardProcess{{PID: bound.AgentPID, Identity: bootID + "/" + startTime}}
			}
			plan.Panes = append(plan.Panes, bound)
		}
	}
	if plan.Launch != task.Launches || plan.AgentName != task.AgentName {
		return nil, discardIdentityRefused("Task was relaunched after discard retained its Worker instance")
	}
	success := false
	defer func() {
		if !success {
			plan.Close()
		}
	}()
	for i := range plan.Panes {
		pane := &plan.Panes[i]
		// A pane already removed by an earlier attempt needs no new authority.
		snapshot, err := s.snapshot(ctx)
		if err != nil || snapshot.ServerStartedAt != plan.Generation {
			return nil, discardIdentityRefused("Herdr generation changed during approved discard")
		}
		if _, found := snapshotPane(snapshot, pane.Pane.PaneID); !found {
			continue
		}
		pane.shell, err = openSandboxProcessHandle(pane.Shell.PID)
		if err != nil {
			return nil, discardIdentityRefused(err.Error())
		}
		if pane.Shell.Identity != "" && pane.shell.Identity() != pane.Shell.Identity {
			return nil, discardIdentityRefused("Task pane shell changed process instance")
		}
		pane.Shell.Identity = pane.shell.Identity()
		pane.agents = newSandboxProcessAuthorization()
		if payload.Worker == nil && pane.AgentPID != 0 {
			handle, err := pane.agents.bind(pane.AgentPID)
			if err != nil {
				return nil, discardIdentityRefused(err.Error())
			}
			if handle.Identity() != pane.Processes[0].Identity {
				return nil, discardIdentityRefused("Worker PID changed while retaining its process instance")
			}
			if err := pane.agents.captureDescendants(); err != nil {
				return nil, discardIdentityRefused(err.Error())
			}
			pane.Processes = nil
			for pid, handle := range pane.agents.handles {
				pane.Processes = append(pane.Processes, discardProcess{PID: pid, Identity: handle.Identity()})
			}
			sort.Slice(pane.Processes, func(i, j int) bool { return pane.Processes[i].PID < pane.Processes[j].PID })
		} else {
			for _, process := range pane.Processes {
				handle, err := pane.agents.bind(process.PID)
				if processGone(err) {
					continue
				}
				if err != nil {
					return nil, discardIdentityRefused(err.Error())
				}
				if handle.Identity() != process.Identity {
					return nil, discardIdentityRefused("Worker PID changed process instance")
				}
			}
		}
		if _, err := s.verifyDiscardPane(ctx, project, task, plan, pane); err != nil {
			return nil, err
		}
	}
	if payload.Worker == nil {
		payload.Worker = plan
		if err := db.UpdateIntentPayload(ctx, intent.ID, intent.ProcessID, marshalJSON(payload)); err != nil {
			return nil, err
		}
	}
	success = true
	return plan, nil
}

// verifyDiscardPane checks topology, launch name, server generation and retained
// shell/foreground process identities. A shell observation never authorizes a
// newly observed foreground process, even when it has the same harness/name.
func (s *Service) verifyDiscardPane(ctx context.Context, project store.Project, task store.Task, plan *discardWorkerPlan, pane *discardPane) (bool, error) {
	check := func(snapshot herdr.Snapshot) (bool, error) {
		if snapshot.ServerStartedAt != plan.Generation {
			return false, discardIdentityRefused("Herdr generation changed during approved discard")
		}
		current, found := snapshotPane(snapshot, pane.Pane.PaneID)
		if !found {
			return false, nil
		}
		name := snapshotAgentName(snapshot, current.PaneID)
		if current.Label != pane.Pane.Label || current.TabID != pane.Pane.TabID || current.WorkspaceID != pane.Pane.WorkspaceID || !pathInside(current.CWD, task.WorktreePath) || name != "" && name != task.AgentName || current.Agent != "" && current.Agent != pane.Pane.Agent {
			return false, discardIdentityRefused("Task pane identity changed; preserving its replacement")
		}
		return true, nil
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return false, discardIdentityRefused(err.Error())
	}
	found, err := check(snapshot)
	if err != nil || !found {
		return false, err
	}
	checkProcess := func(info paneProcessInfo) (bool, error) {
		if info.ShellPID != pane.Shell.PID || pane.shell == nil {
			return false, discardIdentityRefused("Task pane shell identity changed")
		}
		if alive, err := verifySandboxProcessHandleIfAlive(info.ShellPID, pane.shell); err != nil || !alive {
			return false, discardIdentityRefused("Task pane shell process instance is no longer verifiable")
		}
		foreground := info.ForegroundProcessGroup
		busy := foreground != info.ShellPID
		if busy && foreground > 1 {
			if foreground != pane.AgentPID {
				return false, discardIdentityRefused("Replacement foreground process appeared in the Task pane")
			}
			if _, err := pane.agents.verifyIfAlive(foreground); err != nil {
				return false, discardIdentityRefused("Worker process instance is no longer verifiable: " + err.Error())
			}
			// Herdr may briefly retain the exited Worker's foreground group.
			// Wait for the shell rather than treating proved exit as replacement.
		}
		return busy, nil
	}
	info, err := s.sandboxPaneProcessInfo(ctx, pane.Pane.PaneID)
	if err != nil {
		return false, discardIdentityRefused(err.Error())
	}
	busy, err := checkProcess(info)
	if err != nil {
		return false, err
	}
	// process_info itself may race a replacement; recheck ownership after it.
	snapshot, err = s.snapshot(ctx)
	if err != nil {
		return false, discardIdentityRefused(err.Error())
	}
	if found, err = check(snapshot); err != nil || !found {
		return false, err
	}
	if busy && pane.AgentPID != 0 && snapshotAgentName(snapshot, pane.Pane.PaneID) == "" {
		alive, err := pane.agents.verifyIfAlive(pane.AgentPID)
		if err != nil || alive {
			return false, discardIdentityRefused("Worker launch changed during process verification")
		}
	}
	// The snapshot cannot distinguish a same-named restart. Re-read the
	// foreground after it, including when the first observation was a shell.
	info, err = s.sandboxPaneProcessInfo(ctx, pane.Pane.PaneID)
	if err != nil {
		return false, discardIdentityRefused(err.Error())
	}
	return checkProcess(info)
}

func (s *Service) stopDiscardWorker(ctx context.Context, project store.Project, task store.Task, plan *discardWorkerPlan) ([]string, error) {
	var stopped []string
	for i := range plan.Panes {
		pane := &plan.Panes[i]
		if pane.shell == nil {
			continue
		}
		for _, step := range []struct {
			signal syscall.Signal
			grace  time.Duration
		}{{syscall.SIGINT, 300 * time.Millisecond}, {syscall.SIGTERM, agentStopGrace}, {syscall.SIGKILL, agentKillGrace}} {
			if _, err := s.verifyDiscardPane(ctx, project, task, plan, pane); err != nil {
				return stopped, err
			}
			for _, process := range pane.Processes {
				handle := pane.agents.handle(process.PID)
				if handle == nil {
					continue // A previously retained instance exited before recovery.
				}
				alive, err := pane.agents.verifyIfAlive(process.PID)
				if err != nil {
					return stopped, discardIdentityRefused(err.Error())
				}
				if !alive {
					continue
				}
				if _, err := s.verifyDiscardPane(ctx, project, task, plan, pane); err != nil {
					return stopped, err
				}
				if err := handle.Signal(step.signal); err != nil {
					return stopped, discardIdentityRefused(err.Error())
				}
				stopped = append(stopped, fmt.Sprint(process.PID))
			}
			deadline := time.Now().Add(step.grace)
			for {
				busy, err := s.verifyDiscardPane(ctx, project, task, plan, pane)
				if err != nil {
					return stopped, err
				}
				alive := busy
				for _, handle := range pane.agents.handles {
					running, err := handle.Alive()
					if err != nil {
						return stopped, err
					}
					alive = alive || running
				}
				if !alive {
					break
				}
				if time.Now().After(deadline) {
					break
				}
				select {
				case <-ctx.Done():
					return stopped, ctx.Err()
				case <-time.After(50 * time.Millisecond):
				}
			}
		}
		if busy, err := s.verifyDiscardPane(ctx, project, task, plan, pane); err != nil || busy {
			if err != nil {
				return stopped, err
			}
			return stopped, discardIdentityRefused("Worker did not return its pane to the retained shell")
		}
	}
	return uniqueSorted(stopped), nil
}

func (s *Service) closeDiscardWorkerPanes(ctx context.Context, project store.Project, task store.Task, plan *discardWorkerPlan) error {
	for i := range plan.Panes {
		pane := &plan.Panes[i]
		snapshot, err := s.snapshot(ctx)
		if err != nil {
			return err
		}
		if _, found := snapshotPane(snapshot, pane.Pane.PaneID); !found {
			continue
		}
		// Compare-and-clear stale lifecycle authority, with another process check
		// after observing its source/sequence. Never clear a live replacement.
		source, seq, session := s.paneAgentAuthority(ctx, pane.Pane.PaneID, pane.Pane.Agent)
		if busy, err := s.verifyDiscardPane(ctx, project, task, plan, pane); err != nil || busy {
			if err != nil {
				return err
			}
			return discardIdentityRefused("Task pane still has a foreground Worker")
		}
		if session && seq != 0 {
			if _, err := s.herdrCall(ctx, "pane.clear_agent_authority", map[string]any{"pane_id": pane.Pane.PaneID, "source": source, "seq": seq}); err != nil {
				return err
			}
		}
		// Herdr 0.9.1 has no conditional close. This is deliberately the last
		// observation before closing just this pane, never a whole tab/group.
		if busy, err := s.verifyDiscardPane(ctx, project, task, plan, pane); err != nil || busy {
			if err != nil {
				return err
			}
			return discardIdentityRefused("Replacement appeared before Task pane closure")
		}
		if _, err := s.herdrCall(ctx, "pane.close", map[string]any{"pane_id": pane.Pane.PaneID}); err != nil && !missingPaneError(err) {
			return err
		}
	}
	return nil
}
