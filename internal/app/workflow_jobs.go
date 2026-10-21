package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/execgroup"
	"gopkg.in/yaml.v3"
)

const localWorkflowCoverageSchema = "posse.local-workflow-coverage.v1"
const workflowOutputLimit = 16 * 1024

type localWorkflowCoverage struct {
	Schema    string                `json:"schema"`
	Enabled   bool                  `json:"enabled"`
	Workflows []localWorkflowResult `json:"workflows"`
	Notes     []string              `json:"notes,omitempty"`
}

type localWorkflowResult struct {
	File   string             `json:"file"`
	Name   string             `json:"name,omitempty"`
	Status string             `json:"status"`
	Reason string             `json:"reason,omitempty"`
	Jobs   []localWorkflowJob `json:"jobs"`
}

type localWorkflowJob struct {
	Name   string              `json:"name"`
	Status string              `json:"status"`
	Reason string              `json:"reason,omitempty"`
	Steps  []localWorkflowStep `json:"steps,omitempty"`
}

type localWorkflowStep struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Command string `json:"command,omitempty"`
	Output  string `json:"output,omitempty"`
}

type parsedWorkflowJob struct {
	name     string
	runsOn   string
	steps    []parsedWorkflowStep
	workflow map[string]string
	jobEnv   map[string]string
	reason   string
}

type parsedWorkflowStep struct {
	name       string
	run        string
	uses       string
	shell      string
	workingDir string
	env        map[string]string
	with       map[string]string
	reason     string
}

type workflowOutput struct {
	bytes.Buffer
	limit int
}

func (w *workflowOutput) Write(data []byte) (int, error) {
	original := len(data)
	if len(data) >= w.limit {
		w.Buffer.Reset()
		_, _ = w.Buffer.Write(data[len(data)-w.limit:])
		return original, nil
	}
	if w.Len()+len(data) > w.limit {
		w.Buffer.Next(w.Len() + len(data) - w.limit)
	}
	_, _ = w.Buffer.Write(data)
	return original, nil
}

func (w *workflowOutput) String() string {
	value := w.Buffer.String()
	if len(value) == w.limit {
		return "[output truncated to last 16 KiB]\n" + value
	}
	return value
}

func runLocalWorkflowJobs(ctx context.Context, root, scratch string, enabled bool, userCoverage string) (localWorkflowCoverage, error) {
	coverage := localWorkflowCoverage{Schema: localWorkflowCoverageSchema, Enabled: enabled, Workflows: []localWorkflowResult{}}
	files, err := workflowFiles(root)
	if err != nil {
		return coverage, err
	}
	if strings.TrimSpace(userCoverage) != "" {
		coverage.Notes = append(coverage.Notes, "Worker coverage note: "+strings.TrimSpace(userCoverage))
	}
	coverage.Notes = append(coverage.Notes, "Commands ran on the local Linux host with a pull_request event label; GitHub permissions, hosted runner images, services and event payload are not reproduced.")
	if len(files) == 0 {
		coverage.Notes = append(coverage.Notes, "No GitHub Actions workflow files were found in .github/workflows.")
		return coverage, nil
	}

	blocked := false
	for _, file := range files {
		workflow, jobs, parseErr := parseLocalWorkflow(file)
		if parseErr != nil {
			coverage.Workflows = append(coverage.Workflows, localWorkflowResult{
				File:   filepath.ToSlash(filepath.Join(".github", "workflows", filepath.Base(file))),
				Status: "failed", Reason: parseErr.Error(), Jobs: []localWorkflowJob{},
			})
			blocked = true
			continue
		}
		workflowResult := localWorkflowResult{
			File: filepath.ToSlash(filepath.Join(".github", "workflows", filepath.Base(file))),
			Name: workflow.name, Status: "passed", Jobs: []localWorkflowJob{},
		}
		if !workflow.pullRequest {
			workflowResult.Status = "not_applicable"
			workflowResult.Reason = "workflow does not declare a pull_request trigger"
			for _, job := range jobs {
				workflowResult.Jobs = append(workflowResult.Jobs, localWorkflowJob{Name: job.name, Status: "not_applicable", Reason: workflowResult.Reason})
			}
			coverage.Workflows = append(coverage.Workflows, workflowResult)
			continue
		}
		if workflow.reason != "" {
			workflowResult.Status = "not_applicable"
			workflowResult.Reason = workflow.reason
			for _, job := range jobs {
				workflowResult.Jobs = append(workflowResult.Jobs, localWorkflowJob{Name: job.name, Status: "not_applicable", Reason: workflow.reason})
			}
			coverage.Workflows = append(coverage.Workflows, workflowResult)
			continue
		}
		for _, job := range jobs {
			reason := workflowJobNotApplicableReason(root, job)
			if reason != "" {
				workflowResult.Jobs = append(workflowResult.Jobs, localWorkflowJob{Name: job.name, Status: "not_applicable", Reason: reason})
				continue
			}
			if !enabled {
				workflowResult.Status = "failed"
				workflowResult.Jobs = append(workflowResult.Jobs, localWorkflowJob{Name: job.name, Status: "not_applicable", Reason: "local workflow execution is disabled by the frozen User config; enable full_pipeline.run_workflows for this Project before admitting the Task"})
				blocked = true
				continue
			}
			result, runErr := runLocalWorkflowJob(ctx, root, scratch, workflow, job)
			workflowResult.Jobs = append(workflowResult.Jobs, result)
			if runErr != nil {
				workflowResult.Status = "failed"
				blocked = true
			}
		}
		coverage.Workflows = append(coverage.Workflows, workflowResult)
	}
	if blocked {
		return coverage, fmt.Errorf("one or more local workflow jobs failed, could not be parsed, or were not authorized to run")
	}
	return coverage, nil
}

type parsedWorkflow struct {
	name        string
	pullRequest bool
	reason      string
	workflowEnv map[string]string
}

func workflowFiles(root string) ([]string, error) {
	workflowDir := filepath.Join(root, ".github", "workflows")
	info, err := os.Lstat(workflowDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("workflow directory must be a real directory inside the candidate")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	resolvedWorkflowDir, err := filepath.EvalSymlinks(workflowDir)
	if err != nil {
		return nil, err
	}
	if !pathWithin(resolvedRoot, resolvedWorkflowDir) {
		return nil, fmt.Errorf("workflow directory resolves outside the candidate")
	}
	entries, err := os.ReadDir(workflowDir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 {
			if strings.HasSuffix(strings.ToLower(entry.Name()), ".yml") || strings.HasSuffix(strings.ToLower(entry.Name()), ".yaml") {
				return nil, fmt.Errorf("workflow file %s is a symlink or directory", entry.Name())
			}
			continue
		}
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if extension == ".yml" || extension == ".yaml" {
			files = append(files, filepath.Join(workflowDir, entry.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

func parseLocalWorkflow(file string) (parsedWorkflow, []parsedWorkflowJob, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return parsedWorkflow{}, nil, err
	}
	if len(data) > 1024*1024 {
		return parsedWorkflow{}, nil, fmt.Errorf("workflow exceeds the 1 MiB local parsing limit")
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return parsedWorkflow{}, nil, fmt.Errorf("invalid workflow YAML: %w", err)
	}
	root := documentRoot(&document)
	if root == nil || root.Kind != yaml.MappingNode {
		return parsedWorkflow{}, nil, fmt.Errorf("workflow document must be a YAML mapping")
	}
	workflow := parsedWorkflow{
		name: nodeString(mappingValue(root, "name")), workflowEnv: map[string]string{},
	}
	for index := 0; index+1 < len(root.Content); index += 2 {
		key := root.Content[index].Value
		if key != "name" && key != "run-name" && key != "on" && key != "true" && key != "env" && key != "jobs" && key != "permissions" && key != "concurrency" {
			workflow.reason = "unsupported workflow property " + key
		}
	}
	trigger := mappingValue(root, "on")
	if trigger == nil {
		trigger = mappingValue(root, "true") // yaml.v3 resolves YAML 1.1's unquoted `on` as a boolean key
	}
	pullRequest, workflowReason := pullRequestTrigger(trigger)
	workflow.pullRequest = pullRequest
	if workflow.reason == "" {
		workflow.reason = workflowReason
	}
	if env := mappingValue(root, "env"); env != nil {
		workflowEnv, reason := parseWorkflowEnv(env)
		workflow.workflowEnv = workflowEnv
		if workflow.reason == "" {
			workflow.reason = reason
		}
	}
	jobsNode := mappingValue(root, "jobs")
	if jobsNode == nil || jobsNode.Kind != yaml.MappingNode {
		return workflow, nil, fmt.Errorf("workflow jobs must be a YAML mapping")
	}
	jobs := make([]parsedWorkflowJob, 0, len(jobsNode.Content)/2)
	for index := 0; index+1 < len(jobsNode.Content); index += 2 {
		nameNode, jobNode := jobsNode.Content[index], jobsNode.Content[index+1]
		if nameNode.Kind != yaml.ScalarNode || jobNode.Kind != yaml.MappingNode {
			return workflow, nil, fmt.Errorf("workflow job names and definitions must be scalar/mapping pairs")
		}
		job, parseErr := parseWorkflowJob(nameNode.Value, jobNode, workflow.workflowEnv)
		if parseErr != nil {
			return workflow, nil, fmt.Errorf("job %s: %w", nameNode.Value, parseErr)
		}
		jobs = append(jobs, job)
	}
	return workflow, jobs, nil
}

func documentRoot(document *yaml.Node) *yaml.Node {
	if document == nil || document.Kind != yaml.DocumentNode || len(document.Content) == 0 {
		return nil
	}
	return document.Content[0]
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

func nodeString(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}

func pullRequestTrigger(node *yaml.Node) (bool, string) {
	if node == nil {
		return false, ""
	}
	if node.Kind == yaml.ScalarNode {
		return node.Value == "pull_request", ""
	}
	if node.Kind == yaml.SequenceNode {
		for _, item := range node.Content {
			if item.Value == "pull_request" {
				return true, ""
			}
		}
		return false, ""
	}
	if node.Kind != yaml.MappingNode {
		return false, "workflow trigger must be a scalar, sequence or mapping"
	}
	pr := mappingValue(node, "pull_request")
	if pr == nil {
		return false, ""
	}
	if pr.Kind == yaml.MappingNode {
		for index := 0; index+1 < len(pr.Content); index += 2 {
			key := pr.Content[index].Value
			if key != "branches" && key != "branches-ignore" && key != "types" {
				return true, "pull_request trigger uses unsupported filter " + key
			}
			if key == "types" {
				for _, value := range sequenceOrScalar(pr.Content[index+1]) {
					if value != "opened" && value != "synchronize" && value != "reopened" && value != "ready_for_review" {
						return true, "pull_request trigger type " + value + " is not simulated locally"
					}
				}
			}
			if key == "branches" || key == "branches-ignore" {
				return true, "pull_request branch filters are not simulated locally"
			}
		}
	}
	return true, ""
}

func sequenceOrScalar(node *yaml.Node) []string {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.SequenceNode {
		values := make([]string, 0, len(node.Content))
		for _, item := range node.Content {
			values = append(values, item.Value)
		}
		return values
	}
	return []string{node.Value}
}

func parseWorkflowInputs(node *yaml.Node) (map[string]string, string) {
	inputs := map[string]string{}
	if node.Kind != yaml.MappingNode {
		return inputs, "action inputs must be a mapping"
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		key, value := node.Content[index].Value, node.Content[index+1]
		if value.Kind != yaml.ScalarNode || strings.Contains(value.Value, "${{") {
			return inputs, "action inputs contain an expression or non-scalar value"
		}
		inputs[key] = value.Value
	}
	return inputs, ""
}

func parseWorkflowEnv(node *yaml.Node) (map[string]string, string) {
	env := map[string]string{}
	if node.Kind != yaml.MappingNode {
		return env, "workflow env must be a mapping"
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		key, value := node.Content[index].Value, node.Content[index+1]
		if value.Kind != yaml.ScalarNode || strings.Contains(value.Value, "${{") {
			return env, "workflow env contains an expression or non-scalar value"
		}
		if secretEnvironmentKey(key) {
			return env, "workflow env declares a secret-like key " + key
		}
		env[key] = value.Value
	}
	return env, ""
}

func parseWorkflowJob(name string, node *yaml.Node, workflowEnv map[string]string) (parsedWorkflowJob, error) {
	job := parsedWorkflowJob{name: name, workflow: cloneStringMap(workflowEnv), jobEnv: map[string]string{}}
	for index := 0; index+1 < len(node.Content); index += 2 {
		key, value := node.Content[index].Value, node.Content[index+1]
		switch key {
		case "name":
		case "runs-on":
			job.runsOn = nodeString(value)
		case "steps":
			if value.Kind != yaml.SequenceNode {
				return job, fmt.Errorf("steps must be a sequence")
			}
			for stepIndex, stepNode := range value.Content {
				step, err := parseWorkflowStep(stepIndex, stepNode)
				if err != nil {
					return job, err
				}
				job.steps = append(job.steps, step)
			}
		case "env":
			var reason string
			job.jobEnv, reason = parseWorkflowEnv(value)
			if reason != "" {
				job.reason = reason
			}
		case "needs":
			job.reason = "job dependencies are not supported by the local runner"
		case "if":
			job.reason = "job-level conditions are not evaluated locally"
		case "strategy":
			job.reason = "matrix and strategy expansion are not supported by the local runner"
		case "container", "services", "environment":
			job.reason = key + " requires a hosted runner or service environment"
		case "timeout-minutes":
			job.reason = "job timeout policy is not enforced by the local runner"
		case "permissions", "concurrency":
		default:
			job.reason = "unsupported job property " + key
		}
	}
	if job.runsOn == "" {
		job.reason = "job has no scalar runs-on runner"
	}
	for _, value := range job.jobEnv {
		if strings.Contains(value, "${{") {
			job.reason = "job env expressions are not evaluated locally"
		}
	}
	return job, nil
}

func parseWorkflowStep(index int, node *yaml.Node) (parsedWorkflowStep, error) {
	step := parsedWorkflowStep{name: fmt.Sprintf("step %d", index+1)}
	if node.Kind != yaml.MappingNode {
		return step, fmt.Errorf("step %d must be a mapping", index+1)
	}
	for item := 0; item+1 < len(node.Content); item += 2 {
		key, value := node.Content[item].Value, node.Content[item+1]
		switch key {
		case "name":
			if value.Kind == yaml.ScalarNode && value.Value != "" {
				step.name = value.Value
			}
		case "run":
			step.run = nodeString(value)
		case "uses":
			step.uses = nodeString(value)
		case "shell":
			step.shell = nodeString(value)
		case "working-directory":
			step.workingDir = nodeString(value)
		case "env":
			var reason string
			step.env, reason = parseWorkflowEnv(value)
			if reason != "" {
				step.reason = reason
			}
		case "with":
			var reason string
			step.with, reason = parseWorkflowInputs(value)
			if reason != "" {
				step.reason = reason
			}
		case "if":
			step.reason = "step conditions are not evaluated locally"
		case "timeout-minutes":
			step.reason = "step timeout policy is not enforced by the local runner"
		case "continue-on-error":
			if value.Value == "true" {
				step.reason = "continue-on-error semantics are not supported by the local runner"
			}
		case "id":
		default:
			step.reason = "unsupported step property " + key
		}
	}
	if step.run == "" && step.uses == "" {
		step.reason = "step has neither run nor uses"
	}
	if strings.Contains(step.run, "${{") || strings.Contains(step.workingDir, "${{") || strings.Contains(step.shell, "${{") {
		step.reason = "GitHub expression contexts are not evaluated locally"
	}
	if context := unsupportedWorkflowEnvironmentReference(step.run); context != "" {
		step.reason = "GitHub runner variable " + context + " is not simulated locally"
	}
	for _, value := range step.env {
		if strings.Contains(value, "${{") {
			step.reason = "step env expressions are not evaluated locally"
		}
	}
	return step, nil
}

func workflowJobNotApplicableReason(root string, job parsedWorkflowJob) string {
	if job.reason != "" {
		return job.reason
	}
	if runtime.GOOS != "linux" {
		return "workflow runner requires Linux; local host is " + runtime.GOOS
	}
	if job.runsOn != "ubuntu-latest" && job.runsOn != "ubuntu-22.04" && job.runsOn != "ubuntu-24.04" {
		return "runner " + job.runsOn + " is not supported on local host " + runtime.GOOS
	}
	if len(job.steps) == 0 {
		return "job has no steps"
	}
	for _, step := range job.steps {
		if step.reason != "" {
			return "step " + step.name + ": " + step.reason
		}
		if step.uses != "" {
			if reason := localActionNotApplicableReason(root, step); reason != "" {
				return "step " + step.name + ": " + reason
			}
		}
		if step.uses == "" && step.run != "" {
			if _, _, err := workflowShell(step.shell); err != nil {
				return "step " + step.name + ": " + err.Error()
			}
		}
		for key := range step.env {
			if secretEnvironmentKey(key) {
				return "step " + step.name + " declares a secret-like env key " + key
			}
		}
	}
	for key := range job.jobEnv {
		if secretEnvironmentKey(key) {
			return "job declares a secret-like env key " + key
		}
	}
	return ""
}

func localActionNotApplicableReason(root string, step parsedWorkflowStep) string {
	if step.uses == "actions/checkout@v4" {
		for key, value := range step.with {
			switch key {
			case "persist-credentials":
				if value != "true" && value != "false" {
					return "checkout persist-credentials must be a boolean"
				}
			case "fetch-depth":
				depth, err := strconv.Atoi(value)
				if err != nil || depth < 0 {
					return "checkout fetch-depth must be a non-negative integer"
				}
			default:
				return "checkout input " + key + " is not simulated locally"
			}
		}
		return ""
	}
	if step.uses == "actions/setup-go@v5" {
		for key := range step.with {
			if key != "go-version-file" && key != "cache" {
				return "setup-go input " + key + " is not simulated locally"
			}
		}
		if step.with["go-version-file"] != "go.mod" || step.with["cache"] != "false" {
			return "setup-go is supported only with go-version-file: go.mod and cache: false"
		}
		if reason := localGoVersionReason(root); reason != "" {
			return reason
		}
		return ""
	}
	return "unsupported action " + step.uses
}

var goVersionPattern = regexp.MustCompile(`([0-9]+)\.([0-9]+)(?:\.([0-9]+))?`)

func localGoVersionReason(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "setup-go requires a readable go.mod for local toolchain comparison"
	}
	required := ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "go" {
			required = fields[1]
			break
		}
	}
	if required == "" {
		return "setup-go go.mod has no go directive"
	}
	output, err := exec.Command("go", "version").Output()
	if err != nil {
		return "local Go toolchain is unavailable"
	}
	installedMatch := goVersionPattern.FindStringSubmatch(string(output))
	requiredMatch := goVersionPattern.FindStringSubmatch(required)
	if installedMatch == nil || requiredMatch == nil || compareGoVersions(installedMatch, requiredMatch) < 0 {
		return fmt.Sprintf("local Go toolchain %s does not satisfy go.mod go %s", strings.TrimSpace(string(output)), required)
	}
	return ""
}

func compareGoVersions(left, right []string) int {
	for index := 1; index <= 3; index++ {
		leftValue, rightValue := 0, 0
		if index < len(left) && left[index] != "" {
			leftValue, _ = strconv.Atoi(left[index])
		}
		if index < len(right) && right[index] != "" {
			rightValue, _ = strconv.Atoi(right[index])
		}
		if leftValue < rightValue {
			return -1
		}
		if leftValue > rightValue {
			return 1
		}
	}
	return 0
}

func runLocalWorkflowJob(ctx context.Context, root, scratch string, workflow parsedWorkflow, job parsedWorkflowJob) (localWorkflowJob, error) {
	result := localWorkflowJob{Name: job.name, Status: "passed", Steps: []localWorkflowStep{}}
	identity := sha256.Sum256([]byte(workflow.name + "\x00" + job.name))
	env, err := workflowProcessEnvironment(root, scratch, hex.EncodeToString(identity[:8]))
	if err != nil {
		return localWorkflowJob{Name: job.name, Status: "failed", Reason: err.Error()}, err
	}
	for key, value := range job.workflow {
		env = setEnvironment(env, key, value)
	}
	for key, value := range job.jobEnv {
		env = setEnvironment(env, key, value)
	}
	for _, step := range job.steps {
		stepResult := localWorkflowStep{Name: step.name, Status: "passed", Command: step.run}
		if step.uses != "" {
			stepResult.Status = "not_applicable"
			if strings.HasPrefix(step.uses, "actions/checkout@") {
				stepResult.Reason = "private candidate checkout already supplies the repository source"
			} else {
				stepResult.Reason = "local Go toolchain satisfies go.mod; the GitHub setup-go action is not invoked"
			}
			result.Steps = append(result.Steps, stepResult)
			continue
		}
		cwd, err := workflowWorkingDirectory(root, step.workingDir)
		if err != nil {
			stepResult.Status, stepResult.Reason = "not_applicable", err.Error()
			result.Status, result.Reason = "not_applicable", err.Error()
			result.Steps = append(result.Steps, stepResult)
			return result, nil
		}
		stepEnv := append([]string(nil), env...)
		for key, value := range step.env {
			stepEnv = setEnvironment(stepEnv, key, value)
		}
		shell, args, err := workflowShell(step.shell)
		if err != nil {
			stepResult.Status, stepResult.Reason = "not_applicable", err.Error()
			result.Status, result.Reason = "not_applicable", err.Error()
			result.Steps = append(result.Steps, stepResult)
			return result, nil
		}
		command := execgroup.CommandContext(ctx, shell, append(args, step.run)...)
		command.Dir, command.Env = cwd, stepEnv
		output := &workflowOutput{limit: workflowOutputLimit}
		command.Stdout, command.Stderr = output, output
		err = command.Run()
		stepResult.Output = strings.TrimSpace(output.String())
		result.Steps = append(result.Steps, stepResult)
		if err != nil {
			stepResult.Status = "failed"
			result.Steps[len(result.Steps)-1] = stepResult
			result.Status = "failed"
			result.Reason = fmt.Sprintf("step %q failed: %v", step.name, err)
			return result, fmt.Errorf("workflow job %s: %s", job.name, result.Reason)
		}
	}
	return result, nil
}

func workflowWorkingDirectory(root, relative string) (string, error) {
	if relative == "" {
		return root, nil
	}
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("working-directory must be relative to the candidate")
	}
	cwd := filepath.Clean(filepath.Join(root, relative))
	if !pathWithin(root, cwd) {
		return "", fmt.Errorf("working-directory escapes the candidate")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	resolvedCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", fmt.Errorf("working-directory is unavailable: %w", err)
	}
	if !pathWithin(resolvedRoot, resolvedCwd) {
		return "", fmt.Errorf("working-directory resolves outside the candidate")
	}
	return resolvedCwd, nil
}

func workflowShell(shell string) (string, []string, error) {
	switch shell {
	case "", "bash":
		return "/bin/bash", []string{"--noprofile", "--norc", "-e", "-o", "pipefail", "-c"}, nil
	case "sh":
		return "/bin/sh", []string{"-e", "-c"}, nil
	default:
		return "", nil, fmt.Errorf("shell %q is unsupported on the local Linux runner", shell)
	}
}

func workflowProcessEnvironment(root, scratch, jobID string) ([]string, error) {
	if scratch == "" {
		return nil, fmt.Errorf("task scratch path is required for local workflow execution")
	}
	home := filepath.Join(scratch, "workflow-home", jobID)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		return nil, err
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	cache, modCache, goPath := os.Getenv("GOCACHE"), os.Getenv("GOMODCACHE"), os.Getenv("GOPATH")
	if cache == "" {
		cache = filepath.Join(userHome, ".cache", "go-build")
	}
	if modCache == "" {
		modCache = filepath.Join(userHome, "go", "pkg", "mod")
	}
	if goPath == "" {
		goPath = filepath.Join(userHome, "go")
	}
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	return []string{
		"PATH=" + path,
		"HOME=" + home,
		"TMPDIR=" + scratch, "GOTMPDIR=" + scratch, "TMP=" + scratch, "TEMP=" + scratch, "POSSE_TASK_TEMP=" + scratch, "RUNNER_TEMP=" + scratch,
		"GOCACHE=" + cache, "GOMODCACHE=" + modCache, "GOPATH=" + goPath,
		"CI=true", "GITHUB_ACTIONS=true", "GITHUB_EVENT_NAME=pull_request", "GITHUB_WORKSPACE=" + root,
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TZ=UTC",
	}, nil
}

func setEnvironment(env []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func cloneStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

var workflowVariablePattern = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)

func unsupportedWorkflowEnvironmentReference(command string) string {
	for _, match := range workflowVariablePattern.FindAllStringSubmatch(command, -1) {
		name := match[1]
		if strings.HasPrefix(name, "GITHUB_") && name != "GITHUB_ACTIONS" && name != "GITHUB_EVENT_NAME" && name != "GITHUB_WORKSPACE" {
			return name
		}
		if strings.HasPrefix(name, "RUNNER_") && name != "RUNNER_TEMP" {
			return name
		}
	}
	return ""
}

func secretEnvironmentKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "PRIVATE_KEY", "CREDENTIAL", "API_KEY"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return strings.HasPrefix(upper, "AWS_") || strings.HasPrefix(upper, "AZURE_") || strings.HasPrefix(upper, "GOOGLE_")
}

func workflowCoverageJSON(coverage localWorkflowCoverage) (string, error) {
	data, err := json.Marshal(coverage)
	return string(data), err
}

func failedWorkflowJob(encoded string) bool {
	var coverage localWorkflowCoverage
	if err := json.Unmarshal([]byte(encoded), &coverage); err != nil {
		return false
	}
	for _, workflow := range coverage.Workflows {
		for _, job := range workflow.Jobs {
			if job.Status == "failed" {
				return true
			}
		}
	}
	return false
}

func workflowCoverageFailureOutput(encoded string) string {
	var coverage localWorkflowCoverage
	if err := json.Unmarshal([]byte(encoded), &coverage); err != nil {
		return err.Error()
	}
	var details []string
	for _, workflow := range coverage.Workflows {
		for _, job := range workflow.Jobs {
			if job.Status != "failed" {
				continue
			}
			details = append(details, workflow.File+" job "+job.Name+": "+job.Reason)
			for _, step := range job.Steps {
				if step.Status == "failed" && step.Output != "" {
					details = append(details, step.Output)
				}
			}
		}
	}
	return strings.Join(details, "\n")
}
