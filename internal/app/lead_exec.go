package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

type leadExecPlan struct {
	ProjectID   int64             `json:"project_id"`
	PaneID      string            `json:"pane_id"`
	WorkspaceID string            `json:"workspace_id"`
	AgentName   string            `json:"agent_name"`
	Kind        string            `json:"kind"`
	Binary      string            `json:"binary"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env,omitempty"`
	NeedsPrompt bool              `json:"needs_prompt"`
}

func (s *Service) ExecPendingLead() error {
	if s.pendingLead == nil {
		return nil
	}
	plan := *s.pendingLead
	payload, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	monitor := exec.Command(executable, "_finalize-lead", base64.RawURLEncoding.EncodeToString(payload))
	monitor.Stdin = nil
	monitor.Stdout = io.Discard
	monitor.Stderr = io.Discard
	monitor.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := monitor.Start(); err != nil {
		return fmt.Errorf("start Lead tracker: %w", err)
	}
	args := append([]string{plan.Binary}, plan.Args...)
	env := os.Environ()
	for key, value := range plan.Env {
		for i := len(env) - 1; i >= 0; i-- {
			if strings.HasPrefix(env[i], key+"=") {
				env = append(env[:i], env[i+1:]...)
			}
		}
		env = append(env, key+"="+value)
	}
	return syscall.Exec(plan.Binary, args, env)
}

func (s *Service) RunLeadFinalizer(args []string) int {
	if err := s.finalizeLead(args); err != nil {
		if len(args) == 1 {
			if payload, decodeErr := base64.RawURLEncoding.DecodeString(args[0]); decodeErr == nil {
				var plan leadExecPlan
				if json.Unmarshal(payload, &plan) == nil && plan.PaneID != "" {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					_, _ = s.herdrCall(ctx, "notification.show", map[string]any{"title": "Posse Lead startup failed", "body": err.Error()})
				}
			}
		}
		fmt.Fprintln(os.Stderr, "posse: Lead startup tracking failed:", err)
		return 1
	}
	return 0
}

func (s *Service) finalizeLead(args []string) error {
	if len(args) != 1 {
		return errors.New("invalid Lead startup payload")
	}
	payload, err := base64.RawURLEncoding.DecodeString(args[0])
	if err != nil {
		return errors.New("invalid Lead startup payload")
	}
	var plan leadExecPlan
	if err := json.Unmarshal(payload, &plan); err != nil || plan.ProjectID == 0 || plan.PaneID == "" || plan.WorkspaceID == "" || plan.AgentName == "" {
		return errors.New("invalid Lead startup payload")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if os.Getenv("HERDR_PANE_ID") != plan.PaneID || os.Getenv("HERDR_WORKSPACE_ID") != plan.WorkspaceID {
		return errors.New("lead startup pane context changed")
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := db.ProjectByID(ctx, plan.ProjectID)
	if err != nil {
		return err
	}
	if project.LeadPaneID != plan.PaneID || project.HerdrWorkspaceID != plan.WorkspaceID {
		return errors.New("lead startup no longer owns its recorded pane")
	}
	var agent herdr.Pane
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot, snapshotErr := s.Herdr.Snapshot(ctx)
		if snapshotErr == nil {
			paneFound := false
			for _, pane := range snapshot.Panes {
				if pane.PaneID != plan.PaneID {
					continue
				}
				if pane.WorkspaceID != plan.WorkspaceID {
					return nil
				}
				paneFound = true
				if pane.Agent != "" && pane.AgentStatus != "unknown" {
					agent = pane
				}
				break
			}
			if !paneFound {
				// A detached finalizer is cancelled when its pane or workspace
				// closes before startup completes.
				return nil
			}
		}
		if agent.Agent != "" {
			break
		}
		select {
		case <-ctx.Done():
			return errors.New("herdr did not detect the lead in the calling pane")
		case <-ticker.C:
		}
	}
	if _, err := s.herdrCall(ctx, "agent.rename", map[string]any{"target": plan.PaneID, "name": plan.AgentName}); err != nil {
		return err
	}
	if plan.NeedsPrompt {
		if err := s.deliverLeadPrompt(ctx, plan.PaneID, "Run `posse lead` and follow it."); err != nil {
			return err
		}
	}
	if err := s.deliverNotices(ctx, db, project); err != nil {
		return err
	}
	return s.regenerateProjects(ctx, db)
}

func directAgentCommand(kind string) (string, error) {
	if strings.TrimSpace(kind) == "" {
		return "", errors.New("lead agent kind is empty")
	}
	binary, err := exec.LookPath(kind)
	if err != nil {
		return "", fmt.Errorf("find Lead agent %s: %w", kind, err)
	}
	return binary, nil
}
