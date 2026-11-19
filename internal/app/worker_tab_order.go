package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func workerWorkspaceOrderLockPath(home, sessionSocket string) string {
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(sessionSocket)))
	return filepath.Join(home, "state", "herdr-workspace-order-"+key+".lock")
}

func (s *Service) placeWorkerWorkspace(ctx context.Context, db *store.DB, home string, projectID, taskID int64, leadID, workerID string) {
	if leadID == "" || workerID == "" {
		log.Printf("posse: could not order Worker workspace %q: Lead or Worker workspace id is empty", workerID)
		return
	}
	lockPath := workerWorkspaceOrderLockPath(home, herdr.SocketPath(os.Environ()))
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		log.Printf("posse: could not order Worker workspace %s: create lock directory: %v", workerID, err)
		return
	}
	lock, err := acquireFileLock(ctx, lockPath)
	if err != nil {
		log.Printf("posse: could not order Worker workspace %s: acquire session lock: %v", workerID, err)
		return
	}
	defer lock.Close()

	snapshot, err := s.snapshot(ctx)
	if err != nil {
		log.Printf("posse: could not order Worker workspace %s: read session snapshot: %v", workerID, err)
		return
	}
	tasks, err := db.Tasks(ctx, projectID, true)
	if err != nil {
		log.Printf("posse: could not order Worker workspace %s: read Project Tasks: %v", workerID, err)
		return
	}
	ids := make([]string, len(snapshot.Workspaces))
	positions := make(map[string]int, len(snapshot.Workspaces))
	for i, workspace := range snapshot.Workspaces {
		ids[i] = workspace.WorkspaceID
		if workspace.WorkspaceID != "" {
			if _, duplicate := positions[workspace.WorkspaceID]; duplicate {
				log.Printf("posse: could not order Worker workspace %s: duplicate workspace id %s", workerID, workspace.WorkspaceID)
				return
			}
			positions[workspace.WorkspaceID] = i
		}
	}
	leadIndex, leadExists := positions[leadID]
	workerIndex, workerExists := positions[workerID]
	if !leadExists || !workerExists {
		log.Printf("posse: could not order Worker workspace %s: exact Lead workspace %s or Worker workspace is absent", workerID, leadID)
		return
	}
	owned := map[string]bool{workerID: true}
	workspacesByPath := make(map[string][]string)
	for _, workspace := range snapshot.Workspaces {
		if workspace.Worktree.IsLinkedWorktree && workspace.Worktree.CheckoutPath != "" {
			path := filepath.Clean(workspace.Worktree.CheckoutPath)
			workspacesByPath[path] = append(workspacesByPath[path], workspace.WorkspaceID)
		}
	}
	for _, task := range tasks {
		if task.ID == taskID {
			continue
		}
		if task.HerdrWorkspaceID != "" {
			owned[task.HerdrWorkspaceID] = true
		}
		if task.WorktreePath != "" {
			for _, id := range workspacesByPath[filepath.Clean(task.WorktreePath)] {
				owned[id] = true
			}
		}
	}
	blockEnd := leadIndex + 1
	for blockEnd < len(ids) && owned[ids[blockEnd]] {
		blockEnd++
	}
	if workerIndex >= leadIndex+1 && workerIndex < blockEnd {
		return
	}
	insertIndex := blockEnd
	if workerIndex < insertIndex {
		insertIndex--
	}
	withoutWorker := append([]string(nil), ids[:workerIndex]...)
	withoutWorker = append(withoutWorker, ids[workerIndex+1:]...)
	expected := append([]string(nil), withoutWorker[:insertIndex]...)
	expected = append(expected, workerID)
	expected = append(expected, withoutWorker[insertIndex:]...)
	raw, err := s.herdrCall(ctx, "workspace.move", map[string]any{"workspace_id": workerID, "insert_index": insertIndex})
	if err != nil {
		log.Printf("posse: could not order Worker workspace %s after Lead workspace %s: %v", workerID, leadID, err)
		return
	}
	var result struct {
		Type       string `json:"type"`
		Workspaces []struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Type != "workspace_list" {
		log.Printf("posse: could not verify order of Worker workspace %s: invalid workspace.move response", workerID)
		return
	}
	actual := make([]string, len(result.Workspaces))
	for i, workspace := range result.Workspaces {
		actual[i] = workspace.WorkspaceID
	}
	if !equalWorkspaceOrder(actual, expected) {
		log.Printf("posse: could not verify order of Worker workspace %s: returned workspace order differs from requested order", workerID)
	}
}

func equalWorkspaceOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
