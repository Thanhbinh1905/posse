package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

const (
	validationBlockStart = "<!-- posse:validation:start -->"
	validationBlockEnd   = "<!-- posse:validation:end -->"
)

// validationSummary is the read-only report of a Change Task's built-in
// validation evidence. It never authorizes publication or Merge.
type validationSummary struct {
	Head          string
	Review        string
	MaxRounds     int
	RoundsUsed    int
	RepairUsed    int
	Pending       int
	Remaining     int
	LatestVerdict string
	Findings      map[string]int
	Carries       int
	Dispositions  int
	CI            string
}

func validationSummaryFor(ctx context.Context, db *store.DB, task store.Task) (validationSummary, error) {
	summary := validationSummary{Head: task.GatedSHA, Review: "unknown", Findings: map[string]int{}, CI: "unknown"}
	plan, err := task.AdmittedPlan()
	if err != nil {
		return summary, err
	}
	if plan != nil {
		summary.Review = plan.Review
		if plan.ValidationLoops != nil && plan.ValidationLoops.Review.Enabled {
			summary.MaxRounds = plan.ValidationLoops.MaxReviewRounds()
		}
	}
	usage, err := db.ReviewCapUsage(ctx, task.ID)
	if err != nil {
		return summary, err
	}
	summary.RoundsUsed, summary.RepairUsed = usage.ReviewRounds, usage.RepairRounds
	rounds, err := db.ReviewRoundsFor(ctx, task.ID)
	if err != nil {
		return summary, err
	}
	for _, round := range rounds {
		if round.Status == "admitted" {
			summary.Pending++
		}
		if round.Verdict != "" {
			summary.LatestVerdict = round.Verdict
		}
		if summary.Head == "" {
			summary.Head = round.HeadSHA
		}
	}
	summary.Remaining = max(0, summary.MaxRounds-reviewCapUsed(task, usage)-summary.Pending)
	findings, err := db.ReviewFindingsForTask(ctx, task.ID)
	if err != nil {
		return summary, err
	}
	for _, finding := range findings {
		state := finding.State
		if finding.InheritedFrom > 0 {
			state = "inherited"
		}
		summary.Findings[state]++
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_carries WHERE ship_task_id=?`, task.ID).Scan(&summary.Carries); err != nil {
		return summary, err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_dispositions WHERE ship_task_id=?`, task.ID).Scan(&summary.Dispositions); err != nil {
		return summary, err
	}
	if task.PRURL != "" {
		observation, err := db.LatestPRObservation(ctx, task.ID)
		switch {
		case err == nil:
			checks := decodeChecks(observation.Checks)
			summary.CI = strings.ToLower(checks.State)
			if len(checks.Failures) > 0 {
				summary.CI = "failed: " + failedCheckNames(checks.Failures)
			}
			if summary.CI == "" {
				summary.CI = "unknown"
			}
		case store.IsNotFound(err):
			summary.CI = "pending"
		default:
			return summary, err
		}
	}
	return summary, nil
}

func (summary validationSummary) findingStates() []string {
	states := make([]string, 0, len(summary.Findings))
	for state := range summary.Findings {
		states = append(states, state)
	}
	sort.Strings(states)
	return states
}

// view is the TOON/JSON form shared by `posse review` and `posse show`.
func (summary validationSummary) view() axi.Object {
	findings := axi.Object{}
	total := 0
	for _, state := range summary.findingStates() {
		findings = append(findings, axi.Field{Key: state, Value: summary.Findings[state]})
		total += summary.Findings[state]
	}
	return axi.Object{
		{Key: "head", Value: summary.Head},
		{Key: "review", Value: summary.Review},
		{Key: "rounds_used", Value: summary.RoundsUsed},
		{Key: "repair_rounds_used", Value: summary.RepairUsed},
		{Key: "pending_rounds", Value: summary.Pending},
		{Key: "max_rounds", Value: summary.MaxRounds},
		{Key: "remaining", Value: summary.Remaining},
		{Key: "latest_verdict", Value: summary.LatestVerdict},
		{Key: "findings_total", Value: total},
		{Key: "findings", Value: findings},
		{Key: "carries", Value: summary.Carries},
		{Key: "dispositions", Value: summary.Dispositions},
		{Key: "ci", Value: summary.CI},
	}
}

// markdown renders the block that lives inside a tokenized PR body span.
func (summary validationSummary) markdown() string {
	head := "unknown"
	if summary.Head != "" {
		head = "`" + shortSHA(summary.Head) + "`"
	}
	rounds := fmt.Sprintf("%d used, %d remaining of %d", summary.RoundsUsed, summary.Remaining, summary.MaxRounds)
	if summary.MaxRounds == 0 {
		rounds = "review loop off"
	}
	verdict := defaultValue(summary.LatestVerdict, "none yet")
	findings := "none"
	if states := summary.findingStates(); len(states) > 0 {
		parts := make([]string, 0, len(states))
		for _, state := range states {
			parts = append(parts, fmt.Sprintf("%s %d", state, summary.Findings[state]))
		}
		findings = strings.Join(parts, ", ")
	}
	return strings.Join([]string{
		validationBlockStart,
		"## Validation",
		"",
		"- Head: " + head,
		"- Review: " + summary.Review + " (" + rounds + "; latest verdict: " + verdict + ")",
		"- Findings: " + findings,
		fmt.Sprintf("- Carries: %d, dispositions: %d", summary.Carries, summary.Dispositions),
		"- CI: " + summary.CI,
		validationBlockEnd,
	}, "\n")
}

func (summary validationSummary) digest() string {
	sum := sha256.Sum256([]byte(summary.markdown()))
	return hex.EncodeToString(sum[:])
}

// replaceValidationBlock rewrites only the Validation block inside the span
// owned by token. Text outside that span, and the rest of the span, is kept.
// It reports false when the body has no unambiguous owned span.
func replaceValidationBlock(existing, token, block string) (string, bool) {
	startMarker, endMarker := publishBodyMarker("start", token), publishBodyMarker("end", token)
	if strings.Count(existing, startMarker) != 1 || strings.Count(existing, endMarker) != 1 {
		return "", false
	}
	start, end := strings.Index(existing, startMarker), strings.Index(existing, endMarker)
	if end < start || !markerIsWholeLine(existing, start, startMarker) || !markerIsWholeLine(existing, end, endMarker) {
		return "", false
	}
	inner := existing[start+len(startMarker) : end]
	blockStart, blockEnd := strings.Index(inner, validationBlockStart), strings.Index(inner, validationBlockEnd)
	if blockStart >= 0 && blockEnd > blockStart && strings.Count(inner, validationBlockStart) == 1 && strings.Count(inner, validationBlockEnd) == 1 {
		inner = inner[:blockStart] + block + inner[blockEnd+len(validationBlockEnd):]
	} else if blockStart < 0 && blockEnd < 0 {
		inner = strings.TrimRight(inner, "\n") + "\n\n" + block + "\n"
	} else {
		return "", false
	}
	return existing[:start+len(startMarker)] + inner + existing[end:], true
}

// refreshPRValidationBlock rewrites the Validation block of an open PR/MR when
// the evidence digest changed since the last write. It is best effort: a body
// without Posse's token span is left alone.
func (s *Service) refreshPRValidationBlock(ctx context.Context, db *store.DB, project store.Project, task store.Task, forge repositoryForge, prURL, member string) error {
	if task.Type != "change" || prURL == "" {
		return nil
	}
	marker, err := db.GetPRBodyMarker(ctx, task.ID, member)
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return err
	}
	if marker.PRURL != prURL {
		return nil
	}
	summary, err := validationSummaryFor(ctx, db, task)
	if err != nil {
		return err
	}
	digest := summary.digest()
	var written string
	if err := db.QueryRowContext(ctx, `SELECT validation_digest FROM pr_body_markers WHERE task_id=? AND repo=?`, task.ID, member).Scan(&written); err != nil {
		return err
	}
	if written == digest {
		return nil
	}
	existing, err := readForgeBody(ctx, forge, prURL)
	if err != nil {
		return err
	}
	next, ok := replaceValidationBlock(existing, marker.Token, summary.markdown())
	if ok && next != existing {
		if err := writeForgeBody(ctx, forge, prURL, next); err != nil {
			return err
		}
	}
	_, err = db.ExecContext(ctx, `UPDATE pr_body_markers SET validation_digest=? WHERE task_id=? AND repo=? AND marker_token=?`, digest, task.ID, member, marker.Token)
	return err
}

func readForgeBody(ctx context.Context, forge repositoryForge, prURL string) (string, error) {
	if forge.Kind == "gitlab" {
		number, err := forgeReference(prURL, forge)
		if err != nil {
			return "", err
		}
		output, err := commandOutputArgs(ctx, forge.Root, "glab", "api", "--hostname", forge.Host, "projects/"+url.PathEscape(forge.Path)+"/merge_requests/"+strconv.Itoa(number))
		if err != nil {
			return "", fmt.Errorf("read merge request body: %w", err)
		}
		var mr gitlabMergeRequest
		if err := json.Unmarshal([]byte(output), &mr); err != nil || mr.WebURL != prURL {
			return "", fmt.Errorf("GitLab returned invalid merge request metadata")
		}
		return mr.Description, nil
	}
	output, err := commandOutputArgs(ctx, forge.Root, "gh", "pr", "view", prURL, "--json", "body")
	if err != nil {
		return "", fmt.Errorf("read pull request body: %w", err)
	}
	var pull struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(output), &pull); err != nil {
		return "", fmt.Errorf("gh returned invalid pull request metadata: %w", err)
	}
	return pull.Body, nil
}

func writeForgeBody(ctx context.Context, forge repositoryForge, prURL, body string) error {
	if forge.Kind == "gitlab" {
		number, err := forgeReference(prURL, forge)
		if err != nil {
			return err
		}
		_, err = commandOutputArgs(ctx, forge.Root, "glab", "api", "--hostname", forge.Host, "--method", "PUT", "--raw-field", "description="+body, "projects/"+url.PathEscape(forge.Path)+"/merge_requests/"+strconv.Itoa(number))
		return err
	}
	_, err := commandOutputArgs(ctx, forge.Root, "gh", "pr", "edit", prURL, "--body", body)
	return err
}
