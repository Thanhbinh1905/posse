package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

// These are identity hints, not credentials. Same-user agents can erase them;
// consent records do not establish an authentication boundary.
var agentEnvironment = []string{"CLAUDECODE", "CLAUDE_AGENT_ID", "CODEX_CI", "CODEX_THREAD_ID", "PI_CODING_AGENT", "PI_SESSION_ID", "OPENCODE_SESSION_ID"}

func agentEnvironmentKey(key string) bool {
	for _, name := range agentEnvironment {
		if key == name {
			return true
		}
	}
	return false
}

type caller struct {
	role      string
	projectID int64
	paneID    string
}

func (s *Service) configCaller(ctx context.Context, db *store.DB) caller {
	panes := []string{os.Getenv("HERDR_PANE_ID")}
	agent := false
	for _, key := range agentEnvironment {
		agent = agent || os.Getenv(key) != ""
	}
	for _, process := range processAncestors(procRoot, os.Getpid()) {
		// Do not import actor hints from a parent of an isolated home.
		if home := process.env["HOME"]; home != "" && home != os.Getenv("HOME") {
			break
		}
		panes = append(panes, process.env["HERDR_PANE_ID"])
		for _, key := range agentEnvironment {
			agent = agent || process.env[key] != ""
		}
		agent = agent || process.agent
	}
	var lead caller
	var unboundPane string
	for _, pane := range panes {
		if pane == "" {
			continue
		}
		if unboundPane == "" {
			unboundPane = pane
		}
		if task, err := db.TaskByPane(ctx, pane); err == nil {
			return caller{role: "worker", projectID: task.ProjectID}
		}
		if project, err := db.ProjectByLeadPane(ctx, pane); err == nil && lead.role == "" {
			lead = caller{role: "lead", projectID: project.ID, paneID: pane}
		}
	}
	if dir, err := currentDir(); err == nil {
		if task, err := taskForPath(ctx, db, dir); err == nil {
			return caller{role: "worker", projectID: task.ProjectID}
		}
	}
	if lead.role != "" {
		return lead
	}
	if agent {
		return caller{role: "agent"}
	}
	if unboundPane != "" {
		// A pane without a Posse binding is not proof of a User. A fresh
		// Herdr observation can distinguish its plain shell from an agent;
		// missing or unavailable evidence stays unbound.
		if s.Herdr != nil {
			probe, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			if snapshot, err := s.Herdr.Snapshot(probe); err == nil {
				for _, pane := range snapshot.Panes {
					if pane.PaneID == unboundPane && pane.Agent == "" && (pane.AgentStatus == "" || pane.AgentStatus == "unknown") {
						return caller{role: "user"}
					}
				}
			}
		}
		return caller{role: "agent"}
	}
	return caller{role: "user"}
}

func knownAgentProcess(path string) bool {
	switch strings.TrimSuffix(filepath.Base(path), " (deleted)") {
	case "claude", "codex", "pi", "opencode":
		return true
	}
	return false
}
