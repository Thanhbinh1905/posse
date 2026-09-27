package dispatch

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/config"
)

type Brief struct {
	Type           string
	Title          string
	DoneWhen       string
	LandingMode    string
	ReviewOf       string
	AutonomyReview string
	AutonomyLand   string
	// Repos names the workspace members the Task works on; empty for a repo Project.
	Repos  []string
	Issues []IssueRef
	Refs   []IssueRef
	Body   string
}

type IssueRef struct {
	Repository string
	Number     int
}

func (ref IssueRef) String() string {
	issue := "#" + strconv.Itoa(ref.Number)
	if ref.Repository != "" {
		return ref.Repository + issue
	}
	return issue
}

type BriefError struct {
	Field  string
	Reason string
}

func (e *BriefError) Error() string { return fmt.Sprintf("%s: %s", e.Field, e.Reason) }

func ParseBrief(path string) (Brief, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return Brief{}, err
	}
	return ParseBriefText(string(contents))
}

func ParseBriefText(contents string) (Brief, error) {
	lines := strings.Split(strings.ReplaceAll(contents, "\r\n", "\n"), "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return Brief{}, &BriefError{Field: "frontmatter", Reason: "must start with --- and include a closing ---"}
	}
	closing := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			closing = i
			break
		}
	}
	if closing < 0 {
		return Brief{}, &BriefError{Field: "frontmatter", Reason: "missing closing ---"}
	}
	values := map[string]string{}
	for i := 1; i < closing; i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			return Brief{}, &BriefError{Field: "frontmatter", Reason: fmt.Sprintf("line %d must be key: value", i+1)}
		}
		key = strings.TrimSpace(key)
		value = stripTrailingComment(value)
		if !oneOf(key, "type", "title", "done_when", "landing_mode", "review_of", "autonomy", "repos", "issues", "refs") {
			return Brief{}, &BriefError{Field: key, Reason: "unknown Brief field"}
		}
		if _, exists := values[key]; exists {
			return Brief{}, &BriefError{Field: key, Reason: "field is repeated"}
		}
		values[key] = value
	}
	brief := Brief{
		Type:        unquote(values["type"]),
		Title:       unquote(values["title"]),
		DoneWhen:    unquote(values["done_when"]),
		LandingMode: unquote(values["landing_mode"]),
		ReviewOf:    unquote(values["review_of"]),
		Body:        strings.TrimSpace(strings.Join(lines[closing+1:], "\n")),
	}
	if raw, found := values["repos"]; found {
		repos, err := parseRepos(raw)
		if err != nil {
			return Brief{}, err
		}
		brief.Repos = repos
	}
	if raw, found := values["issues"]; found {
		issues, err := parseIssueRefs(raw, "issues")
		if err != nil {
			return Brief{}, err
		}
		brief.Issues = issues
	}
	if raw, found := values["refs"]; found {
		refs, err := parseIssueRefs(raw, "refs")
		if err != nil {
			return Brief{}, err
		}
		brief.Refs = refs
	}
	seenIssueRefs := make(map[IssueRef]string, len(brief.Issues)+len(brief.Refs))
	for _, ref := range brief.Issues {
		seenIssueRefs[ref] = "issues"
	}
	for _, ref := range brief.Refs {
		if previous := seenIssueRefs[ref]; previous != "" {
			return Brief{}, &BriefError{Field: "refs", Reason: ref.String() + " is already listed in " + previous}
		}
	}
	if raw, found := values["autonomy"]; found {
		autonomy, err := parseAutonomy(raw)
		if err != nil {
			return Brief{}, err
		}
		brief.AutonomyLand = autonomy.Land
		brief.AutonomyReview = autonomy.Review
	}
	if brief.Type == "" {
		return Brief{}, &BriefError{Field: "type", Reason: "is required"}
	}
	if !oneOf(brief.Type, "ship", "scout", "review") {
		return Brief{}, &BriefError{Field: "type", Reason: "must be ship, scout or review"}
	}
	if brief.Title == "" {
		return Brief{}, &BriefError{Field: "title", Reason: "is required"}
	}
	if brief.DoneWhen == "" {
		return Brief{}, &BriefError{Field: "done_when", Reason: "is required"}
	}
	if brief.LandingMode != "" && !oneOf(brief.LandingMode, "local", "pr", "no-mistakes") {
		return Brief{}, &BriefError{Field: "landing_mode", Reason: "must be local, pr or no-mistakes"}
	}
	if brief.Type == "review" {
		if !regexp.MustCompile(`^t[1-9][0-9]*$`).MatchString(brief.ReviewOf) {
			return Brief{}, &BriefError{Field: "review_of", Reason: "is required for a Review Task and must be a Task id such as t12"}
		}
	} else if brief.ReviewOf != "" {
		return Brief{}, &BriefError{Field: "review_of", Reason: "is valid only for a Review Task"}
	}
	return brief, nil
}

var repoName = regexp.MustCompile(`^[a-z0-9_-]+$`)
var issueNumber = regexp.MustCompile(`^[1-9][0-9]*$`)

func parseIssueRefs(raw, field string) ([]IssueRef, error) {
	text := strings.TrimSpace(raw)
	if strings.HasPrefix(text, "[") {
		if !strings.HasSuffix(text, "]") {
			return nil, &BriefError{Field: field, Reason: "must be a list such as [12, member#16]"}
		}
		text = strings.TrimSuffix(strings.TrimPrefix(text, "["), "]")
	}
	refs := []IssueRef{}
	seen := map[IssueRef]bool{}
	for _, item := range strings.Split(text, ",") {
		value := unquote(strings.TrimSpace(item))
		if value == "" {
			continue
		}
		repository, numberText, found := strings.Cut(value, "#")
		if found && (!repoName.MatchString(repository) || strings.Contains(numberText, "#")) {
			return nil, &BriefError{Field: field, Reason: fmt.Sprintf("%q must be an issue number or member#number", value)}
		}
		if !found {
			numberText = repository
			repository = ""
		}
		if !issueNumber.MatchString(numberText) {
			return nil, &BriefError{Field: field, Reason: fmt.Sprintf("%q must use a positive issue number", value)}
		}
		number, err := strconv.Atoi(numberText)
		if err != nil {
			return nil, &BriefError{Field: field, Reason: fmt.Sprintf("%q is too large", value)}
		}
		ref := IssueRef{Repository: repository, Number: number}
		if seen[ref] {
			return nil, &BriefError{Field: field, Reason: ref.String() + " is listed twice"}
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return nil, &BriefError{Field: field, Reason: "must list at least one issue"}
	}
	return refs, nil
}

// parseRepos reads `repos: [a, b]` or `repos: a, b`.
func parseRepos(raw string) ([]string, error) {
	text := strings.TrimSpace(raw)
	if strings.HasPrefix(text, "[") {
		if !strings.HasSuffix(text, "]") {
			return nil, &BriefError{Field: "repos", Reason: "must be a list such as [backend, worker]"}
		}
		text = strings.TrimSuffix(strings.TrimPrefix(text, "["), "]")
	}
	repos := []string{}
	seen := map[string]bool{}
	for _, item := range strings.Split(text, ",") {
		name := unquote(strings.TrimSpace(item))
		if name == "" {
			continue
		}
		if !repoName.MatchString(name) {
			return nil, &BriefError{Field: "repos", Reason: fmt.Sprintf("%q is not a member name", name)}
		}
		if seen[name] {
			return nil, &BriefError{Field: "repos", Reason: name + " is listed twice"}
		}
		seen[name] = true
		repos = append(repos, name)
	}
	if len(repos) == 0 {
		return nil, &BriefError{Field: "repos", Reason: "must name at least one member"}
	}
	return repos, nil
}

func stripTrailingComment(value string) string {
	doubleQuoted, singleQuoted, escaped := false, false, false
	for index := 0; index < len(value); index++ {
		character := value[index]
		if doubleQuoted {
			if escaped {
				escaped = false
				continue
			}
			if character == '\\' {
				escaped = true
				continue
			}
			if character == '"' {
				doubleQuoted = false
			}
			continue
		}
		if singleQuoted {
			if character == '\'' {
				if index+1 < len(value) && value[index+1] == '\'' {
					index++
					continue
				}
				singleQuoted = false
			}
			continue
		}
		switch character {
		case '"':
			doubleQuoted = true
		case '\'':
			singleQuoted = true
		case '#':
			if index == 0 || value[index-1] == ' ' || value[index-1] == '\t' {
				return strings.TrimSpace(value[:index])
			}
		}
	}
	return strings.TrimSpace(value)
}

type briefAutonomy struct{ Review, Land string }

func parseAutonomy(raw string) (briefAutonomy, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return briefAutonomy{}, nil
	}
	if strings.HasPrefix(raw, "{") && strings.HasSuffix(raw, "}") {
		raw = strings.TrimSpace(raw[1 : len(raw)-1])
		result := briefAutonomy{}
		for _, part := range strings.Split(raw, ",") {
			key, value, found := strings.Cut(part, ":")
			if !found {
				return briefAutonomy{}, &BriefError{Field: "autonomy", Reason: "must contain key: value entries"}
			}
			if err := setAutonomy(&result, strings.TrimSpace(key), unquote(strings.TrimSpace(value))); err != nil {
				return briefAutonomy{}, err
			}
		}
		return result, nil
	}
	return briefAutonomy{}, &BriefError{Field: "autonomy", Reason: "must be an inline map such as { land: ask }"}
}

func setAutonomy(result *briefAutonomy, key, value string) error {
	switch key {
	case "review":
		if result.Review != "" {
			return &BriefError{Field: "autonomy.review", Reason: "field is repeated"}
		}
		if !oneOf(value, "ask", "lead") {
			return &BriefError{Field: "autonomy.review", Reason: "must be ask or lead"}
		}
		result.Review = value
	case "land":
		if result.Land != "" {
			return &BriefError{Field: "autonomy.land", Reason: "field is repeated"}
		}
		if !oneOf(value, "ask", "auto") {
			return &BriefError{Field: "autonomy.land", Reason: "must be ask or auto"}
		}
		result.Land = value
	default:
		return &BriefError{Field: "autonomy." + key, Reason: "unknown Autonomy field"}
	}
	return nil
}

func ValidateTightening(brief Brief, mode string, autonomy config.Autonomy) (string, config.Autonomy, error) {
	resultMode := mode
	if brief.LandingMode != "" {
		if landingRank(brief.LandingMode) < landingRank(mode) {
			return "", config.Autonomy{}, &BriefError{Field: "landing_mode", Reason: "cannot loosen the Project Landing Mode"}
		}
		resultMode = brief.LandingMode
	}
	if autonomy.Review == "" {
		autonomy.Review = "ask"
	}
	if autonomy.Land == "" {
		autonomy.Land = "ask"
	}
	if brief.AutonomyReview != "" {
		if reviewRank(brief.AutonomyReview) > reviewRank(autonomy.Review) {
			return "", config.Autonomy{}, &BriefError{Field: "autonomy.review", Reason: "cannot grant more Autonomy than the Project"}
		}
		autonomy.Review = brief.AutonomyReview
	}
	if brief.AutonomyLand != "" {
		if landRank(brief.AutonomyLand) > landRank(autonomy.Land) {
			return "", config.Autonomy{}, &BriefError{Field: "autonomy.land", Reason: "cannot grant more Autonomy than the Project"}
		}
		autonomy.Land = brief.AutonomyLand
	}
	return resultMode, autonomy, nil
}

func landingRank(value string) int {
	switch value {
	case "local":
		return 0
	case "pr":
		return 1
	case "no-mistakes":
		return 2
	default:
		return -1
	}
}

func reviewRank(value string) int {
	if value == "lead" {
		return 1
	}
	return 0
}

func landRank(value string) int {
	if value == "auto" {
		return 1
	}
	return 0
}

func unquote(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		if value[0] == '"' {
			if decoded, err := strconv.Unquote(value); err == nil {
				return decoded
			}
		}
		return value[1 : len(value)-1]
	}
	return value
}

func oneOf(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if value == candidate {
			return true
		}
	}
	return false
}
