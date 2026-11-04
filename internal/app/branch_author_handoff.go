package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/thanhbinh1905/posse/internal/store"
)

type branchAuthorHandoffBundle struct {
	Version     int                           `json:"version"`
	Task        int                           `json:"task"`
	Branch      string                        `json:"branch"`
	Attempt     int                           `json:"attempt"`
	Generation  int64                         `json:"generation"`
	Head        string                        `json:"head"`
	Scope       string                        `json:"scope"`
	ScopeSource string                        `json:"scope_source"`
	ScopeSHA    string                        `json:"scope_sha256"`
	Findings    []store.ReviewFindingDetail   `json:"findings"`
	Candidates  []store.ValidationCandidate   `json:"candidates"`
	Evidence    []store.ValidationStageResult `json:"evidence"`
}

func buildBranchAuthorHandoffBundle(ctx context.Context, db *store.DB, home, projectName string, task store.Task, attempt int, generation int64) ([]byte, error) {
	taskDir := filepath.Join(home, "projects", projectName, "tasks", fmt.Sprintf("t%d", task.Seq))
	scope, err := os.ReadFile(filepath.Join(taskDir, "launch.md"))
	scopeSource := "launch.md"
	if os.IsNotExist(err) {
		scopeSource = "task_record"
		scope = []byte(fmt.Sprintf("Task: %s\nBranch: %s\n", task.Title, task.Branch))
	} else if err != nil {
		return nil, fmt.Errorf("read Task scope for branch author handoff: %w", err)
	}
	repository, err := gitCommonDir(ctx, task.WorktreePath)
	if err != nil {
		return nil, err
	}
	claim, err := db.BranchAuthor(ctx, repository, task.Branch)
	if err != nil {
		return nil, err
	}
	if claim.TaskID != task.ID || claim.Attempt != attempt || claim.Generation != generation {
		return nil, store.ErrBranchAuthorStale
	}
	findings, err := db.ReviewFindingsForTask(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	candidates, err := db.ValidationCandidates(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	evidence, err := db.ValidationStages(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	if findings == nil {
		findings = []store.ReviewFindingDetail{}
	}
	if candidates == nil {
		candidates = []store.ValidationCandidate{}
	}
	if evidence == nil {
		evidence = []store.ValidationStageResult{}
	}
	scopeHash := sha256.Sum256(scope)
	bundle := branchAuthorHandoffBundle{
		Version: 1, Task: task.Seq, Branch: task.Branch, Attempt: attempt,
		Generation: generation, Head: claim.HeadSHA, Scope: string(scope), ScopeSource: scopeSource,
		ScopeSHA: fmt.Sprintf("%x", scopeHash[:]), Findings: findings,
		Candidates: candidates, Evidence: evidence,
	}
	encoded, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}
