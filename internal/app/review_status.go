package app

import (
	"database/sql"
	"sort"
	"strconv"

	"github.com/thanhbinh1905/posse/internal/axi"
)

func (s *Service) reviewStatus(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("review", args, map[string]flagSpec{"full": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("review requires one Change Task ID")
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if err := s.requireLeadOrUser(ctx.Context, db, project); err != nil {
		return err
	}
	change, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	if change.Type != "change" {
		return axi.Failure("review_task_invalid", "review status requires a Change Task", false)
	}
	plan, err := change.AdmittedPlan()
	if err != nil {
		return admittedPlanFailure(err)
	}
	max := 0
	if plan != nil && plan.ValidationLoops != nil && plan.ValidationLoops.Review.Enabled {
		max = plan.ValidationLoops.MaxReviewRounds()
	}
	usage, err := db.ReviewCapUsage(ctx.Context, change.ID)
	if err != nil {
		return err
	}
	rounds, err := db.ReviewRoundsFor(ctx.Context, change.ID)
	if err != nil {
		return err
	}
	findings, err := db.ReviewFindingsForTask(ctx.Context, change.ID)
	if err != nil {
		return err
	}
	type patternStatus struct {
		ID        string           `json:"id"`
		Seq       int              `json:"seq"`
		Title     string           `json:"title"`
		Rule      string           `json:"rule,omitempty"`
		RootCause string           `json:"root_cause,omitempty"`
		Findings  []map[string]any `json:"findings"`
	}
	patterns := map[int]*patternStatus{}
	for _, finding := range findings {
		pattern := patterns[finding.PatternSeq]
		if pattern == nil {
			pattern = &patternStatus{ID: "P-" + strconv.Itoa(finding.PatternSeq), Seq: finding.PatternSeq, Title: finding.Title}
			if parsed.Bool("full") {
				pattern.Rule, pattern.RootCause = finding.Rule, finding.RootCause
			}
			patterns[finding.PatternSeq] = pattern
		}
		state := finding.State
		if finding.InheritedFrom > 0 {
			state = "inherited"
		}
		row := map[string]any{"id": "F-" + strconv.Itoa(finding.FindingSeq), "state": state, "severity": finding.Severity, "origin": finding.Origin, "in_scope": finding.InScope, "repository": finding.Repository, "path": finding.Path, "symbol": finding.Symbol}
		if finding.InheritedFrom > 0 {
			row["inherited"] = true
		}
		if parsed.Bool("full") {
			row["excerpt"] = finding.Excerpt
			row["fix_guidance"] = finding.FixGuidance
		}
		pattern.Findings = append(pattern.Findings, row)
	}
	patternSeqs := make([]int, 0, len(patterns))
	for seq := range patterns {
		patternSeqs = append(patternSeqs, seq)
	}
	sort.Ints(patternSeqs)
	var patternRows []any
	for _, seq := range patternSeqs {
		patternRows = append(patternRows, patterns[seq])
	}
	var roundRows []any
	pendingRounds := 0
	for _, round := range rounds {
		if round.Status == "admitted" {
			pendingRounds++
		}
		row := map[string]any{"round": round.Round, "review_task_id": round.ReviewTaskID, "repository": round.Repository, "head": round.HeadSHA, "scope": round.ScopeKind, "status": round.Status, "verdict": round.Verdict}
		if parsed.Bool("full") {
			row["scope_base"] = round.ScopeBaseSHA
			row["target"] = round.TargetCommitSHA
			row["baseline_tree"] = round.BaselineTree
			row["delta_sha256"] = round.DeltaSHA256
			row["conflicted_paths"] = round.ConflictedPaths
		}
		roundRows = append(roundRows, row)
	}
	var carries []any
	rows, err := db.QueryContext(ctx.Context, `SELECT repository,from_head,to_head,target_commit_sha,tree,created_at FROM review_carries WHERE ship_task_id=? ORDER BY created_at,repository,from_head,to_head`, change.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var repository, fromHead, toHead, target, tree string
		var createdAt int64
		if err := rows.Scan(&repository, &fromHead, &toHead, &target, &tree, &createdAt); err != nil {
			rows.Close()
			return err
		}
		carry := map[string]any{"repository": repository, "from_head": fromHead, "to_head": toHead}
		if parsed.Bool("full") {
			carry["target"] = target
			carry["tree"] = tree
			carry["created_at"] = createdAt
		}
		carries = append(carries, carry)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	remaining := max - usage.Total() - pendingRounds
	if remaining < 0 {
		remaining = 0
	}
	var dispositions []any
	rows, err = db.QueryContext(ctx.Context, `SELECT head_sha,decision_id,user_quote,at FROM review_dispositions WHERE ship_task_id=? ORDER BY at,id`, change.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var head, quote string
		var decisionID sql.NullInt64
		var at int64
		if err := rows.Scan(&head, &decisionID, &quote, &at); err != nil {
			rows.Close()
			return err
		}
		disposition := map[string]any{"head": head}
		if parsed.Bool("full") {
			if decisionID.Valid {
				disposition["decision_id"] = decisionID.Int64
			}
			disposition["user_quote"], disposition["at"] = quote, at
		}
		dispositions = append(dispositions, disposition)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return ctx.Print(axi.Object{
		{Key: "task", Value: taskIDString(change.Seq)},
		{Key: "review_rounds_used", Value: usage.ReviewRounds},
		{Key: "repair_rounds_used", Value: usage.RepairRounds},
		{Key: "pending_review_rounds", Value: pendingRounds},
		{Key: "total_used", Value: usage.Total()},
		{Key: "max_rounds", Value: max},
		{Key: "remaining", Value: remaining},
		{Key: "rounds", Value: roundRows},
		{Key: "patterns", Value: patternRows},
		{Key: "carries", Value: carries},
		{Key: "dispositions", Value: dispositions},
	})
}
