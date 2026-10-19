package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

type reviewFindingsFile struct {
	Schema      string                      `json:"schema"`
	Patterns    *[]reviewFindingPatternFile `json:"patterns"`
	Resolutions []reviewFindingResolution   `json:"resolutions,omitempty"`
}

type reviewFindingPatternFile struct {
	Key         string                  `json:"key,omitempty"`
	Title       string                  `json:"title"`
	Rule        string                  `json:"rule"`
	RootCause   string                  `json:"root_cause"`
	FixGuidance string                  `json:"fix_guidance"`
	Severity    string                  `json:"severity,omitempty"`
	Origin      string                  `json:"origin,omitempty"`
	Instances   []reviewFindingInstance `json:"instances"`
}

type reviewFindingInstance struct {
	Repository  string  `json:"repository"`
	Path        string  `json:"path"`
	Line        int     `json:"line,omitempty"`
	Symbol      string  `json:"symbol,omitempty"`
	Excerpt     string  `json:"excerpt"`
	Description string  `json:"description,omitempty"`
	Cause       *string `json:"cause,omitempty"`
	Matches     *string `json:"matches,omitempty"`
	Regresses   *string `json:"regresses,omitempty"`
}

type reviewFindingResolution struct {
	Finding string `json:"finding"`
	Status  string `json:"status"`
	Note    string `json:"note,omitempty"`
}

type changedLineSet map[string]map[int]bool

var (
	reviewHunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)
	findingReference = regexp.MustCompile(`^F-[1-9][0-9]*$`)
)

func decodeReviewFindings(contents []byte) (reviewFindingsFile, error) {
	var findings reviewFindingsFile
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&findings); err != nil {
		return findings, fmt.Errorf("invalid JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return findings, fmt.Errorf("unexpected data after the findings object")
		}
		return findings, fmt.Errorf("invalid trailing JSON: %w", err)
	}
	if findings.Schema != "posse.review-findings.v1" || findings.Patterns == nil {
		return findings, fmt.Errorf("schema must be posse.review-findings.v1 and patterns must be an array")
	}
	for i := range *findings.Patterns {
		pattern := &(*findings.Patterns)[i]
		if strings.TrimSpace(pattern.Title) == "" || strings.TrimSpace(pattern.Rule) == "" || strings.TrimSpace(pattern.RootCause) == "" || strings.TrimSpace(pattern.FixGuidance) == "" || len(pattern.Instances) == 0 {
			return findings, fmt.Errorf("pattern %d requires title, rule, root_cause, fix_guidance and at least one instance", i+1)
		}
		if pattern.Origin == "" {
			pattern.Origin = "introduced"
		} else if pattern.Origin != "introduced" && pattern.Origin != "pre_existing" {
			return findings, fmt.Errorf("pattern %d has unknown origin %q", i+1, pattern.Origin)
		}
		if !oneOfString(pattern.Severity, "P0", "P1", "P2", "P3") {
			pattern.Severity = "unclassified"
		}
		for j := range pattern.Instances {
			instance := &pattern.Instances[j]
			if strings.TrimSpace(instance.Repository) == "" || strings.TrimSpace(instance.Path) == "" || strings.TrimSpace(instance.Excerpt) == "" || instance.Line < 0 {
				return findings, fmt.Errorf("pattern %d instance %d requires repository, path and excerpt; line cannot be negative", i+1, j+1)
			}
			if instance.Matches != nil && *instance.Matches != "" && !findingReference.MatchString(*instance.Matches) {
				return findings, fmt.Errorf("pattern %d instance %d has invalid matches reference %q", i+1, j+1, *instance.Matches)
			}
			if instance.Regresses != nil && *instance.Regresses != "" && !findingReference.MatchString(*instance.Regresses) {
				return findings, fmt.Errorf("pattern %d instance %d has invalid regresses reference %q", i+1, j+1, *instance.Regresses)
			}
		}
	}
	for i, resolution := range findings.Resolutions {
		if !findingReference.MatchString(resolution.Finding) || (resolution.Status != "resolved" && resolution.Status != "unresolved") {
			return findings, fmt.Errorf("resolution %d must name an F-n finding and use resolved or unresolved", i+1)
		}
	}
	return findings, nil
}

func normalizedFindingText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func normalizedTextLines(contents []byte) (string, []int) {
	var normalized strings.Builder
	var lineByByte []int
	line := 1
	pendingSpace := false
	for _, r := range string(contents) {
		if unicode.IsSpace(r) {
			if normalized.Len() > 0 {
				pendingSpace = true
			}
			if r == '\n' {
				line++
			}
			continue
		}
		if pendingSpace {
			normalized.WriteByte(' ')
			lineByByte = append(lineByByte, line)
			pendingSpace = false
		}
		encoded := []byte(string(r))
		normalized.Write(encoded)
		for range encoded {
			lineByByte = append(lineByByte, line)
		}
	}
	return normalized.String(), lineByByte
}

type anchoredFindingExcerpt struct {
	Start int
	End   int
}

func findAnchoredExcerpts(contents []byte, excerpt string) ([]anchoredFindingExcerpt, error) {
	normalizedExcerpt := normalizedFindingText(excerpt)
	if normalizedExcerpt == "" {
		return nil, fmt.Errorf("excerpt is empty after whitespace normalization")
	}
	normalizedContents, lineByByte := normalizedTextLines(contents)
	var matches []anchoredFindingExcerpt
	for start := 0; start <= len(normalizedContents)-len(normalizedExcerpt); {
		found := strings.Index(normalizedContents[start:], normalizedExcerpt)
		if found < 0 {
			break
		}
		start += found
		end := start + len(normalizedExcerpt) - 1
		if start < len(lineByByte) && end < len(lineByByte) {
			matches = append(matches, anchoredFindingExcerpt{Start: lineByByte[start], End: lineByByte[end]})
		}
		start++
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("excerpt does not occur in the reviewed file after whitespace normalization")
	}
	return matches, nil
}

func cleanReviewFindingPath(value string) (string, error) {
	if strings.Contains(value, "\\") || path.IsAbs(value) {
		return "", fmt.Errorf("path must be repository-relative and use forward slashes")
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return "", fmt.Errorf("path must be a clean repository-relative path")
	}
	return clean, nil
}

func changedLinesFromPatch(patch []byte, repositories []string, workspace bool) changedLineSet {
	changed := changedLineSet{}
	var currentPath string
	newLine := 0
	inHunk := false
	for _, raw := range strings.Split(string(patch), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if strings.HasPrefix(line, "+++ ") {
			currentPath = strings.TrimPrefix(line[4:], "b/")
			if strings.HasPrefix(currentPath, "\"") {
				if decoded, err := strconv.Unquote(currentPath); err == nil {
					currentPath = strings.TrimPrefix(decoded, "b/")
				}
			}
			if currentPath == "/dev/null" {
				currentPath = ""
			}
			inHunk = false
			continue
		}
		if matches := reviewHunkHeader.FindStringSubmatch(line); len(matches) == 3 {
			newLine, _ = strconv.Atoi(matches[1])
			inHunk = currentPath != ""
			continue
		}
		if !inHunk || line == "" {
			continue
		}
		switch line[0] {
		case '+':
			key := currentPath
			matchedRepository := false
			if workspace {
				for _, repository := range repositories {
					prefix := repository + "/"
					if strings.HasPrefix(key, prefix) {
						key = repository + "\x00" + strings.TrimPrefix(key, prefix)
						matchedRepository = true
						break
					}
				}
			}
			if !matchedRepository && len(repositories) == 1 {
				key = repositories[0] + "\x00" + key
			}
			if changed[key] == nil {
				changed[key] = map[int]bool{}
			}
			changed[key][newLine] = true
			newLine++
		case ' ':
			newLine++
		case '-':
		default:
			inHunk = false
		}
	}
	return changed
}

func causeNamesChangedLine(cause, repository string, changed changedLineSet, workspace bool) (bool, error) {
	if cause == "" {
		return false, nil
	}
	separator := strings.LastIndex(cause, ":")
	if separator < 1 {
		return false, fmt.Errorf("cause must use path:line to identify a changed line in review/delta.patch")
	}
	file, lineText := cause[:separator], cause[separator+1:]
	lineText = strings.TrimPrefix(lineText, "+")
	line, err := strconv.Atoi(lineText)
	if err != nil || line < 1 {
		return false, fmt.Errorf("cause must use path:line to identify a changed line in review/delta.patch")
	}
	if workspace {
		file = strings.TrimPrefix(file, repository+"/")
	}
	clean, err := cleanReviewFindingPath(file)
	if err != nil {
		return false, fmt.Errorf("invalid cause path: %w", err)
	}
	return changed[repository+"\x00"+clean][line], nil
}

func classifyReviewFindingScope(round int, origin, repository, file string, anchors []anchoredFindingExcerpt, cause string, changed changedLineSet, workspace bool) (bool, error) {
	inScope := round == 1 && origin == "introduced"
	for _, anchor := range anchors {
		for line := anchor.Start; line <= anchor.End; line++ {
			if changed[repository+"\x00"+file][line] {
				inScope = true
				break
			}
		}
		if inScope {
			break
		}
	}
	causeScope, err := causeNamesChangedLine(cause, repository, changed, workspace)
	if err != nil {
		return false, err
	}
	return inScope || causeScope, nil
}

func readReviewDeltaInput(task store.Task) ([]byte, error) {
	plan, err := task.AdmittedPlan()
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, fmt.Errorf("Review Task has no admitted plan")
	}
	for _, input := range plan.Inputs {
		if input.Role == "review-round-file" && input.Source == "review/delta.patch" {
			return append([]byte(nil), input.Bytes...), nil
		}
	}
	return nil, fmt.Errorf("Review Task has no frozen review/delta.patch")
}

func buildReviewFindingsInput(ctx context.Context, db *store.DB, project store.Project, review store.Task, contents []byte, rounds []store.ReviewRound, blockAt string) (*store.ReviewFindingsInput, error) {
	file, err := decodeReviewFindings(contents)
	if err != nil {
		return nil, axi.Failure("review_findings_invalid", err.Error(), false)
	}
	delta, err := readReviewDeltaInput(review)
	if err != nil {
		return nil, axi.Failure("review_findings_invalid", err.Error(), false)
	}
	repositories := make([]string, 0, len(rounds))
	bindings := map[string]store.ReviewRound{}
	for _, round := range rounds {
		repositories = append(repositories, round.Repository)
		bindings[round.Repository] = round
	}
	roots := map[string]string{project.Name: review.WorktreePath}
	if project.IsWorkspace() {
		repos, err := db.TaskRepos(ctx, review.ID)
		if err != nil {
			return nil, err
		}
		roots = make(map[string]string, len(repos))
		for _, repo := range repos {
			roots[repo.Repo] = repo.WorktreePath
		}
	}
	for _, round := range rounds {
		if roots[round.Repository] == "" {
			return nil, axi.Failure("review_findings_invalid", "Review Round has no checkout for repository "+round.Repository, false)
		}
	}
	if err := verifyReviewDeltaBindings(ctx, project, rounds, delta, roots); err != nil {
		return nil, axi.Failure("review_findings_invalid", err.Error(), false)
	}
	changedLines := changedLinesFromPatch(delta, repositories, project.IsWorkspace())
	result := &store.ReviewFindingsInput{BlockAt: blockAt}
	for _, pattern := range *file.Patterns {
		converted := store.ReviewFindingPatternInput{
			Key: pattern.Key, Title: pattern.Title, Rule: pattern.Rule, RootCause: pattern.RootCause,
			FixGuidance: pattern.FixGuidance, Severity: pattern.Severity, Origin: pattern.Origin,
		}
		for _, finding := range pattern.Instances {
			binding, found := bindings[finding.Repository]
			root := roots[finding.Repository]
			if !found || root == "" {
				return nil, axi.Failure("review_findings_invalid", fmt.Sprintf("instance names repository %q outside this Review Round", finding.Repository), false)
			}
			clean, err := cleanReviewFindingPath(finding.Path)
			if err != nil {
				return nil, axi.Failure("review_findings_invalid", fmt.Sprintf("invalid finding path %q: %v", finding.Path, err), false)
			}
			contentsAtHead, err := gitOutputRaw(ctx, root, "show", binding.HeadSHA+":"+clean)
			if err != nil {
				return nil, axi.Failure("review_finding_unanchored", fmt.Sprintf("excerpt path %s is not present at reviewed head %s", clean, shortSHA(binding.HeadSHA)), false)
			}
			anchors, err := findAnchoredExcerpts([]byte(contentsAtHead), finding.Excerpt)
			if err != nil {
				return nil, axi.Failure("review_finding_unanchored", fmt.Sprintf("%s:%s: %v", finding.Repository, clean, err), false)
			}
			line := finding.Line
			if line == 0 {
				line = anchors[0].Start
			}
			cause := ""
			if finding.Cause != nil {
				cause = strings.TrimSpace(*finding.Cause)
			}
			inScope, scopeErr := classifyReviewFindingScope(binding.Round, pattern.Origin, finding.Repository, clean, anchors, cause, changedLines, project.IsWorkspace())
			if scopeErr != nil {
				return nil, axi.Failure("review_findings_invalid", scopeErr.Error(), false)
			}
			matches, regresses := "", ""
			if finding.Matches != nil {
				matches = *finding.Matches
			}
			if finding.Regresses != nil {
				regresses = *finding.Regresses
			}
			converted.Instances = append(converted.Instances, store.ReviewFindingInput{
				Repository: finding.Repository, Path: clean, Symbol: finding.Symbol,
				Excerpt: strings.TrimSpace(finding.Excerpt), Description: finding.Description, Line: line,
				InScope: inScope, Matches: matches, Regresses: regresses,
			})
		}
		result.Patterns = append(result.Patterns, converted)
	}
	for _, resolution := range file.Resolutions {
		result.Resolutions = append(result.Resolutions, store.ReviewFindingResolutionInput{Finding: resolution.Finding, Status: resolution.Status, Note: resolution.Note})
	}
	return result, nil
}

func reviewRoundForSignal(ctx context.Context, db *store.DB, home string, project store.Project, review store.Task, supplied string) (*store.ReviewFindingsInput, []byte, error) {
	rounds, err := db.ReviewRoundForTask(ctx, review.ID)
	if err != nil {
		return nil, nil, err
	}
	if len(rounds) == 0 {
		if supplied != "" {
			return nil, nil, axi.Failure("review_findings_unexpected", "--findings is only valid for a numbered Review Round", false)
		}
		return nil, nil, nil
	}
	if supplied == "" {
		return nil, nil, axi.Failure("review_findings_required", "a numbered Review Round requires --findings <file>", false)
	}
	for _, round := range rounds {
		if round.Status != "admitted" {
			return nil, nil, axi.Failure("review_findings_invalid", "Review Round is not admitted", false)
		}
	}
	ship, err := db.TaskByID(ctx, project.ID, review.ReviewsTaskID)
	if err != nil {
		return nil, nil, err
	}
	plan, err := ship.AdmittedPlan()
	if err != nil || plan == nil || plan.ValidationLoops == nil || !plan.ValidationLoops.Review.Enabled {
		return nil, nil, axi.Failure("review_findings_invalid", "Ship Task has no enabled, frozen Review Loop", false)
	}
	contents, err := readSignalFile(home, project, review, supplied)
	if err != nil {
		return nil, nil, axi.Failure("signal_invalid", err.Error(), false)
	}
	findings, err := buildReviewFindingsInput(ctx, db, project, review, contents, rounds, plan.ValidationLoops.Review.BlockAt)
	if err != nil {
		return nil, nil, err
	}
	return findings, contents, nil
}

func readSignalFile(home string, project store.Project, task store.Task, supplied string) ([]byte, error) {
	_, contents, err := resolveSignalFile(home, project, task, supplied)
	return contents, err
}

func resolveSignalFile(home string, project store.Project, task store.Task, supplied string) (string, []byte, error) {
	worktreeRoot, err := filepath.Abs(task.WorktreePath)
	if err != nil {
		return "", nil, err
	}
	scratchRoot, err := taskScratchPath(home, project, task)
	if err != nil {
		return "", nil, err
	}
	validatedScratchRoot, err := validatedTaskScratchRoot(home, project, task)
	if err != nil {
		return "", nil, fmt.Errorf("invalid Task scratch directory %s; Signal files must be inside the Rider's worktree (%s) or this Task's scratch directory: %w", scratchRoot, worktreeRoot, err)
	}
	allowedRoots := []string{worktreeRoot, validatedScratchRoot}
	allowedLocations := fmt.Sprintf("the Rider's worktree (%s) or this Task's scratch directory (%s)", worktreeRoot, scratchRoot)
	filePath, err := filepath.Abs(supplied)
	if err != nil {
		return "", nil, err
	}
	filePath, err = filepath.EvalSymlinks(filePath)
	if err != nil {
		return "", nil, fmt.Errorf("cannot resolve Signal file %q; files must be inside %s: %w", supplied, allowedLocations, err)
	}
	insideAllowedRoot := false
	for _, root := range allowedRoots {
		relative, relErr := filepath.Rel(root, filePath)
		if relErr == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			insideAllowedRoot = true
			break
		}
	}
	if !insideAllowedRoot {
		return "", nil, fmt.Errorf("report and findings files must be inside %s", allowedLocations)
	}
	contents, err := os.ReadFile(filePath)
	if err != nil {
		return "", nil, fmt.Errorf("read Signal file %q inside %s: %w", supplied, allowedLocations, err)
	}
	return filePath, contents, nil
}

func writeReviewFindingsArtifact(project store.Project, review store.Task, home string, contents []byte) error {
	destination := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(review.Seq), "findings.json")
	return writeFile(destination, contents)
}

func verifyReviewDeltaBindings(ctx context.Context, project store.Project, rounds []store.ReviewRound, patch []byte, roots map[string]string) error {
	var rebuilt bytes.Buffer
	for _, round := range rounds {
		args := []string{"diff", "--binary", "--no-ext-diff"}
		if project.IsWorkspace() {
			prefix := round.Repository + "/"
			args = append(args, "--src-prefix=a/"+prefix, "--dst-prefix=b/"+prefix)
		}
		args = append(args, round.BaselineTree, round.HeadSHA)
		output, err := gitOutputRaw(ctx, roots[round.Repository], args...)
		if err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(output))
		if hex.EncodeToString(sum[:]) != round.DeltaSHA256 {
			return fmt.Errorf("frozen delta digest differs from Review Round binding for %s", round.Repository)
		}
		rebuilt.WriteString(output)
	}
	if !bytes.Equal(rebuilt.Bytes(), patch) {
		return fmt.Errorf("frozen review/delta.patch differs from its repository bindings")
	}
	return nil
}
