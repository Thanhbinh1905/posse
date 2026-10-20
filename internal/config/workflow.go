package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type WorkflowProfile struct {
	Stages []WorkflowStage `toml:"stages" json:"stages"`
}

type WorkflowStage struct {
	Name     string `toml:"name" json:"name"`
	Frame    string `toml:"frame" json:"frame"`
	Gate     string `toml:"gate" json:"gate"`
	TaskType string `toml:"task_type" json:"task_type"`
	Workers  any    `toml:"workers" json:"workers"`
	Skills   any    `toml:"skills,omitempty" json:"skills,omitempty"`
}

type WorkflowSelection struct {
	Name    string          `json:"name"`
	Profile WorkflowProfile `json:"profile"`
	Source  string          `json:"source"`
	SHA256  string          `json:"sha256"`
}

func (stage *WorkflowStage) UnmarshalJSON(data []byte) error {
	type workflowStageAlias WorkflowStage
	var decoded workflowStageAlias
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	*stage = WorkflowStage(decoded)
	return nil
}

type WorkflowValidationIssue struct {
	Key    string
	Reason string
}

var workflowRange = regexp.MustCompile(`^([0-9]+)\.\.([0-9]+)$`)

func ValidateWorkflowProfile(profile WorkflowProfile) []WorkflowValidationIssue {
	issues := []WorkflowValidationIssue{}
	seenNames := map[string]bool{}
	if len(profile.Stages) == 0 {
		return []WorkflowValidationIssue{{Key: "stages", Reason: "must contain at least one stage"}}
	}
	for index, stage := range profile.Stages {
		prefix := fmt.Sprintf("stages[%d]", index)
		if strings.TrimSpace(stage.Name) == "" {
			issues = append(issues, WorkflowValidationIssue{Key: prefix + ".name", Reason: "is required"})
		} else if seenNames[stage.Name] {
			issues = append(issues, WorkflowValidationIssue{Key: prefix + ".name", Reason: "must be unique within the workflow"})
		} else {
			seenNames[stage.Name] = true
		}
		if !oneOf(stage.Frame, "plan", "implement", "review", "ship") {
			issues = append(issues, WorkflowValidationIssue{Key: prefix + ".frame", Reason: "must be plan, implement, review or ship"})
		}
		if stage.Gate != "user" {
			issues = append(issues, WorkflowValidationIssue{Key: prefix + ".gate", Reason: "must be user"})
		}
		if !oneOf(stage.TaskType, "none", "research", "change", "review") {
			issues = append(issues, WorkflowValidationIssue{Key: prefix + ".task_type", Reason: "must be none, research, change or review"})
		}
		if reason := validateWorkflowWorkers(stage.Workers); reason != "" {
			issues = append(issues, WorkflowValidationIssue{Key: prefix + ".workers", Reason: reason})
		}
	}
	return issues
}

func validateWorkflowWorkers(value any) string {
	switch workers := value.(type) {
	case int:
		if workers >= 0 {
			return ""
		}
	case int32:
		if workers >= 0 {
			return ""
		}
	case int64:
		if workers >= 0 {
			return ""
		}
	case float64:
		if workers >= 0 && workers == float64(int64(workers)) {
			return ""
		}
	case json.Number:
		if count, err := strconv.ParseInt(workers.String(), 10, 64); err == nil && count >= 0 {
			return ""
		}
	case string:
		if workers == "one-per-ticket-category-or-pattern" || workers == "selected-review-mode" {
			return ""
		}
		if bounds := workflowRange.FindStringSubmatch(workers); len(bounds) == 3 {
			var minimum, maximum int
			if _, err := fmt.Sscanf(bounds[1], "%d", &minimum); err == nil {
				if _, err := fmt.Sscanf(bounds[2], "%d", &maximum); err == nil && minimum <= maximum {
					return ""
				}
			}
		}
	}
	return "must be a non-negative count, an increasing min..max range, one-per-ticket-category-or-pattern or selected-review-mode"
}

func WorkflowDigest(profile WorkflowProfile) (string, error) {
	data, err := json.Marshal(profile)
	if err != nil {
		return "", fmt.Errorf("encode workflow profile: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func workflowSchemaValidationErrors(file string, values map[string]any) []error {
	workflows, ok := values["workflows"].(map[string]any)
	if !ok {
		return nil
	}
	var issues []error
	for _, name := range sortedMapKeys(workflows) {
		prefix := "workflows." + name
		profile, ok := workflows[name].(map[string]any)
		if ok && len(profile) == 0 {
			continue
		}
		if !ok {
			issues = append(issues, &InvalidError{File: file, Key: prefix, Reason: "must be a workflow profile table"})
			continue
		}
		for _, key := range sortedMapKeys(profile) {
			if key != "stages" {
				issues = append(issues, &InvalidError{File: file, Key: prefix + "." + key, Reason: "unknown workflow profile key"})
			}
		}
		stages, ok := workflowTableEntries(profile["stages"])
		if !ok {
			issues = append(issues, &InvalidError{File: file, Key: prefix + ".stages", Reason: "must be an array of tables"})
			continue
		}
		if len(stages) == 0 {
			issues = append(issues, &InvalidError{File: file, Key: prefix + ".stages", Reason: "must contain at least one stage"})
			continue
		}
		seenNames := map[string]bool{}
		for index, stage := range stages {
			stagePrefix := fmt.Sprintf("%s.stages[%d]", prefix, index)
			for _, key := range sortedMapKeys(stage) {
				if !oneOf(key, "name", "frame", "gate", "task_type", "workers", "skills") {
					issues = append(issues, &InvalidError{File: file, Key: stagePrefix + "." + key, Reason: "unknown workflow stage key"})
				}
			}
			name, _ := stage["name"].(string)
			if strings.TrimSpace(name) == "" {
				issues = append(issues, &InvalidError{File: file, Key: stagePrefix + ".name", Reason: "is required and must be a string"})
			} else if seenNames[name] {
				issues = append(issues, &InvalidError{File: file, Key: stagePrefix + ".name", Reason: "must be unique within the workflow"})
			} else {
				seenNames[name] = true
			}
			frame, _ := stage["frame"].(string)
			if !oneOf(frame, "plan", "implement", "review", "ship") {
				issues = append(issues, &InvalidError{File: file, Key: stagePrefix + ".frame", Reason: "must be plan, implement, review or ship"})
			}
			gate, _ := stage["gate"].(string)
			if gate != "user" {
				issues = append(issues, &InvalidError{File: file, Key: stagePrefix + ".gate", Reason: "must be user"})
			}
			taskType, _ := stage["task_type"].(string)
			if !oneOf(taskType, "none", "research", "change", "review") {
				issues = append(issues, &InvalidError{File: file, Key: stagePrefix + ".task_type", Reason: "must be none, research, change or review"})
			}
			if reason := validateWorkflowWorkers(stage["workers"]); reason != "" {
				issues = append(issues, &InvalidError{File: file, Key: stagePrefix + ".workers", Reason: reason})
			}
		}
	}
	return issues
}

func workflowTableEntries(value any) ([]map[string]any, bool) {
	switch entries := value.(type) {
	case []map[string]any:
		return entries, true
	case []any:
		result := make([]map[string]any, 0, len(entries))
		for _, item := range entries {
			entry, ok := item.(map[string]any)
			if !ok {
				return nil, false
			}
			result = append(result, entry)
		}
		return result, true
	default:
		return nil, false
	}
}

func isWorkflowStagesKey(key string) bool {
	parts := strings.Split(key, ".")
	return len(parts) == 3 && parts[0] == "workflows" && namePart.MatchString(parts[1]) && parts[2] == "stages"
}

func workflowSources(profiles map[string]WorkflowProfile, globalPath string, global map[string]any, projectPath string, project map[string]any) map[string]string {
	sources := make(map[string]string, len(profiles))
	for name := range profiles {
		sources[name] = globalPath
		if workflowHasStages(project, name) {
			sources[name] = projectPath
		}
	}
	return sources
}

func workflowHasStages(values map[string]any, name string) bool {
	workflows, _ := values["workflows"].(map[string]any)
	profile, _ := workflows[name].(map[string]any)
	_, found := profile["stages"]
	return found
}

func (c Config) ResolveWorkflow(name string) (WorkflowSelection, bool, error) {
	profile, found := c.Workflows[name]
	if !found {
		return WorkflowSelection{}, false, nil
	}
	digest, err := WorkflowDigest(profile)
	if err != nil {
		return WorkflowSelection{}, false, err
	}
	return WorkflowSelection{Name: name, Profile: profile, Source: c.WorkflowSources[name], SHA256: digest}, true, nil
}
