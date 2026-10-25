package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

type reviewResolutionsFile struct {
	Schema   string                    `json:"schema"`
	Patterns []reviewPatternResolution `json:"patterns"`
}

type reviewPatternResolution struct {
	Pattern string `json:"pattern"`
	Status  string `json:"status"`
	Note    string `json:"note,omitempty"`
}

func decodeReviewResolutions(contents []byte) (reviewResolutionsFile, error) {
	var input reviewResolutionsFile
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, fmt.Errorf("invalid JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return input, fmt.Errorf("unexpected data after the resolutions object")
		}
		return input, fmt.Errorf("invalid trailing JSON: %w", err)
	}
	if input.Schema != "posse.review-resolutions.v1" || input.Patterns == nil {
		return input, fmt.Errorf("schema must be posse.review-resolutions.v1 and patterns must be an array")
	}
	seen := map[string]bool{}
	for i, answer := range input.Patterns {
		if !validPatternReference(answer.Pattern) || (answer.Status != "fixed" && answer.Status != "disputed") {
			return input, fmt.Errorf("pattern answer %d requires a P-n pattern and fixed or disputed status", i+1)
		}
		if seen[answer.Pattern] {
			return input, fmt.Errorf("duplicate pattern answer %s", answer.Pattern)
		}
		seen[answer.Pattern] = true
	}
	return input, nil
}

func validPatternReference(value string) bool {
	if !strings.HasPrefix(value, "P-") || len(value) < 3 || value[2] < '1' || value[2] > '9' {
		return false
	}
	for _, character := range value[3:] {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func convertReviewResolutions(input reviewResolutionsFile, expected, actual string, candidateID int64) *store.ReviewFixResolutionsInput {
	result := &store.ReviewFixResolutionsInput{ExpectedBaseHead: expected, ActualHead: actual, CandidateID: candidateID}
	for _, answer := range input.Patterns {
		result.Patterns = append(result.Patterns, store.ReviewFixResolution{Pattern: answer.Pattern, Status: answer.Status, Note: answer.Note})
	}
	return result
}

// reviewFixMemberAnswers reads the head of every Member a workspace Review fix
// named. A full-pipeline Member answers from its clean private candidate, which
// must still be the candidate the fix was issued for.
func (s *Service) reviewFixMemberAnswers(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task, round store.ValidationFixRound) ([]store.ReviewFixMemberHead, error) {
	answers := make([]store.ReviewFixMemberHead, 0, len(round.MemberHeads))
	for _, member := range round.MemberHeads {
		lane, err := resolveCandidateLane(ctx, db, project, task, member.Repository)
		if err != nil {
			return nil, err
		}
		root := lane.task.WorktreePath
		if member.CandidateID > 0 {
			candidate, err := db.LatestValidationCandidate(ctx, task.ID, member.Repository)
			if err != nil || candidate.ID != member.CandidateID || candidate.State != store.ValidationCandidateActive {
				return nil, axi.Failure("candidate_missing", lane.memberLabel("the private candidate this Review fix was issued for is no longer active"), false, "The Lead must decide whether to queue a new Review fix for the current candidate")
			}
			if err := s.ensureCandidateCheckout(ctx, db, home, lane.task, candidate); err != nil {
				return nil, err
			}
			status, err := gitStatusOutput(ctx, candidate.WorktreePath, "--porcelain", "--untracked-files=all")
			if err != nil {
				return nil, err
			}
			if status != "" {
				return nil, axi.Failure("candidate_dirty", lane.memberLabel("commit candidate changes before signalling done"), false, candidate.WorktreePath)
			}
			root = candidate.WorktreePath
		}
		head, err := gitOutput(ctx, root, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		if !gitAncestor(ctx, root, member.ExpectedBaseHead, head) {
			return nil, recordHeadDiscontinuity(ctx, db, project, task, member.ExpectedBaseHead, head)
		}
		answers = append(answers, store.ReviewFixMemberHead{Repository: member.Repository, ActualHead: head, CandidateID: member.CandidateID})
	}
	return answers, nil
}
