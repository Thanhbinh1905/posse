package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

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
