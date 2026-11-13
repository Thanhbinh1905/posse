package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/thanhbinh1905/posse/internal/store"
)

type comparisonFinding struct {
	File        string `json:"file"`
	Explanation string `json:"explanation"`
}

type comparisonHead struct {
	TaskID int64
	Repo   string
	Head   string
}

type builtinFixedFinding struct {
	TaskID int64
	Head   string
	Repo   string
	Path   string
	Cause  string
}

type reviewComparisonRow struct {
	Project                string   `json:"project"`
	ReviewedHeads          int      `json:"reviewed_heads"`
	Requests               int      `json:"requests"`
	Verdicts               int      `json:"verdicts"`
	Reliability            float64  `json:"reliability"`
	ReliabilityPass        *bool    `json:"reliability_pass"`
	WrongHead              int      `json:"wrong_head"`
	WrongHeadRate          float64  `json:"wrong_head_rate"`
	WrongHeadPass          *bool    `json:"wrong_head_pass"`
	Inconclusive           int      `json:"inconclusive"`
	InconclusiveShare      float64  `json:"inconclusive_share"`
	InconclusivePass       *bool    `json:"inconclusive_pass"`
	Recall                 *float64 `json:"recall,omitempty"`
	RecallPass             *bool    `json:"recall_pass"`
	RecallMatched          int      `json:"recall_matched"`
	RecallPairs            int      `json:"recall_pairs"`
	UnmatchedPairs         []string `json:"unmatched_pairs"`
	Precision              string   `json:"precision"`
	MedianLatencyMS        *int64   `json:"median_latency_ms"`
	BuiltinMedianLatencyMS *int64   `json:"builtin_median_latency_ms"`
	LatencyLimitMS         *int64   `json:"latency_limit_ms"`
	LatencyPass            *bool    `json:"latency_pass"`
	ProjectsWithReviews    int      `json:"projects_with_reviews,omitempty"`
	SamplePass             *bool    `json:"sample_pass,omitempty"`
}

func reviewComparison(ctx context.Context, db *store.DB) (map[string]any, error) {
	projects, err := db.Projects(ctx)
	if err != nil {
		return nil, err
	}
	globalBuiltinLatency, err := builtinReviewLatency(ctx, db, 0)
	if err != nil {
		return nil, err
	}
	var rows []reviewComparisonRow
	var totalRequests, totalVerdicts, totalWrong, totalInconclusive, totalMatched, totalPairs, projectsWithReviews int
	var allLatency []int64
	for _, project := range projects {
		row, latencies, err := reviewComparisonForProject(ctx, db, project)
		if err != nil {
			return nil, err
		}
		if row.BuiltinMedianLatencyMS == nil && globalBuiltinLatency != nil {
			row.BuiltinMedianLatencyMS = globalBuiltinLatency
			limit := *globalBuiltinLatency * 3 / 2
			row.LatencyLimitMS = &limit
			if row.MedianLatencyMS != nil {
				pass := *row.MedianLatencyMS <= limit
				row.LatencyPass = &pass
			}
		}
		rows = append(rows, row)
		totalRequests += row.Requests
		totalVerdicts += row.Verdicts
		totalWrong += row.WrongHead
		totalInconclusive += row.Inconclusive
		totalMatched += row.RecallMatched
		totalPairs += row.RecallPairs
		allLatency = append(allLatency, latencies...)
		if row.ReviewedHeads > 0 {
			projectsWithReviews++
		}
	}
	total := reviewComparisonRow{Project: "total", Requests: totalRequests, Verdicts: totalVerdicts, WrongHead: totalWrong, Inconclusive: totalInconclusive, RecallMatched: totalMatched, RecallPairs: totalPairs, Precision: "not measured in G1"}
	for _, row := range rows {
		total.ReviewedHeads += row.ReviewedHeads
		total.UnmatchedPairs = append(total.UnmatchedPairs, row.UnmatchedPairs...)
	}
	total.ProjectsWithReviews = projectsWithReviews
	samplePass := total.ReviewedHeads >= 50 && projectsWithReviews >= 2
	total.SamplePass = &samplePass
	if total.Requests > 0 {
		total.Reliability = float64(total.Verdicts) / float64(total.Requests)
		pass := total.Reliability >= 0.95
		total.ReliabilityPass = &pass
	}
	if total.Requests > 0 {
		total.WrongHeadRate = float64(total.WrongHead) / float64(total.Requests)
	}
	wrongPass := total.WrongHead == 0
	total.WrongHeadPass = &wrongPass
	if total.Verdicts > 0 {
		total.InconclusiveShare = float64(total.Inconclusive) / float64(total.Verdicts)
		pass := total.InconclusiveShare <= 0.10
		total.InconclusivePass = &pass
	}
	if total.RecallPairs > 0 {
		recall := float64(total.RecallMatched) / float64(total.RecallPairs)
		total.Recall = &recall
		pass := recall >= 0.90
		total.RecallPass = &pass
	}
	if len(allLatency) > 0 {
		median := medianInt64(allLatency)
		total.MedianLatencyMS = &median
	}
	if globalBuiltinLatency != nil && total.MedianLatencyMS != nil {
		median := *globalBuiltinLatency
		total.BuiltinMedianLatencyMS = &median
		limit := median * 3 / 2
		total.LatencyLimitMS = &limit
		pass := *total.MedianLatencyMS <= limit
		total.LatencyPass = &pass
	}
	return map[string]any{"projects": rows, "total": total}, nil
}

func reviewComparisonForProject(ctx context.Context, db *store.DB, project store.Project) (reviewComparisonRow, []int64, error) {
	row := reviewComparisonRow{Project: project.Name, Precision: "not measured in G1", UnmatchedPairs: []string{}}
	requests, err := db.GreenhouseReviewRequests(ctx, project.ID)
	if err != nil {
		return row, nil, err
	}
	headSet := map[string]bool{}
	requestedHeads := map[comparisonHead]bool{}
	latencies := []int64{}
	for _, request := range requests {
		row.Requests++
		requestedHeads[comparisonHead{TaskID: request.TaskID, Repo: request.Repository, Head: request.RequestedHead}] = true
		if request.Outcome == "head_mismatch" {
			row.WrongHead++
		}
		if request.Outcome == "verdict" {
			headSet[fmt.Sprintf("%d/%s/%s/%s", request.TaskID, request.Repository, request.RequestedHead, request.Provider)] = true
			row.Verdicts++
			latencies = append(latencies, request.SettledAt-request.RequestedAt)
			var outcome string
			if err := db.QueryRowContext(ctx, `SELECT verdict FROM verdicts WHERE id=?`, request.VerdictID).Scan(&outcome); err != nil {
				return row, nil, err
			}
			if outcome == "inconclusive" {
				row.Inconclusive++
				row.InconclusiveShare++
			}
		}
	}
	row.ReviewedHeads = len(headSet)
	if row.Requests > 0 {
		row.Reliability = float64(row.Verdicts) / float64(row.Requests)
	}
	if row.Verdicts > 0 {
		row.InconclusiveShare /= float64(row.Verdicts)
	}
	if len(latencies) > 0 {
		median := medianInt64(latencies)
		row.MedianLatencyMS = &median
	}
	if row.Requests > 0 {
		pass := row.Reliability >= 0.95
		row.ReliabilityPass = &pass
	}
	if row.Requests > 0 {
		row.WrongHeadRate = float64(row.WrongHead) / float64(row.Requests)
	}
	wrongPass := row.WrongHead == 0
	row.WrongHeadPass = &wrongPass
	if row.Verdicts > 0 {
		pass := row.InconclusiveShare <= 0.10
		row.InconclusivePass = &pass
	}
	builtinLatency, err := builtinReviewLatency(ctx, db, project.ID)
	if err != nil {
		return row, nil, err
	}
	if builtinLatency != nil {
		row.BuiltinMedianLatencyMS = builtinLatency
		limit := *builtinLatency * 3 / 2
		row.LatencyLimitMS = &limit
		if row.MedianLatencyMS != nil {
			pass := *row.MedianLatencyMS <= limit
			row.LatencyPass = &pass
		}
	}
	findings, err := builtinFixedFindings(ctx, db, project.ID)
	if err != nil {
		return row, nil, err
	}
	parallelByHead, err := greenhouseFindingsByHead(ctx, db, project.ID, requestedHeads)
	if err != nil {
		return row, nil, err
	}
	builtinByHead := map[comparisonHead][]builtinFixedFinding{}
	for _, finding := range findings {
		key := comparisonHead{TaskID: finding.TaskID, Repo: finding.Repo, Head: finding.Head}
		if requestedHeads[key] {
			builtinByHead[key] = append(builtinByHead[key], finding)
		}
	}
	for key, builtins := range builtinByHead {
		row.RecallPairs += len(builtins)
		parallel := parallelByHead[key]
		matchedParallel := make([]bool, len(parallel))
		for _, builtin := range builtins {
			matched := false
			for i, parallelFinding := range parallel {
				if !matchedParallel[i] && parallelFinding.File == builtin.Path && strings.Contains(strings.ToLower(parallelFinding.Explanation), strings.ToLower(builtin.Cause)) {
					matched = true
					matchedParallel[i] = true
					break
				}
			}
			if matched {
				row.RecallMatched++
			} else {
				row.UnmatchedPairs = append(row.UnmatchedPairs, fmt.Sprintf("builtin-only t%d %s@%s: %s", key.TaskID, key.Repo, key.Head, builtin.Path+" | "+builtin.Cause))
			}
		}
		for i, parallelFinding := range parallel {
			if !matchedParallel[i] {
				row.UnmatchedPairs = append(row.UnmatchedPairs, fmt.Sprintf("greenhouse-only t%d %s@%s: %s | %s", key.TaskID, key.Repo, key.Head, parallelFinding.File, parallelFinding.Explanation))
			}
		}
	}
	for key, parallel := range parallelByHead {
		if _, hasBuiltin := builtinByHead[key]; hasBuiltin {
			continue
		}
		for _, finding := range parallel {
			row.UnmatchedPairs = append(row.UnmatchedPairs, fmt.Sprintf("greenhouse-only t%d %s@%s: %s | %s", key.TaskID, key.Repo, key.Head, finding.File, finding.Explanation))
		}
	}
	if row.RecallPairs > 0 {
		recall := float64(row.RecallMatched) / float64(row.RecallPairs)
		row.Recall = &recall
		pass := recall >= 0.90
		row.RecallPass = &pass
	}
	sort.Strings(row.UnmatchedPairs)
	return row, latencies, nil
}

func greenhouseFindingsByHead(ctx context.Context, db *store.DB, projectID int64, requested map[comparisonHead]bool) (map[comparisonHead][]comparisonFinding, error) {
	rows, err := db.QueryContext(ctx, `SELECT r.task_id,r.repo,r.requested_head,v.findings_path FROM greenhouse_review_requests r JOIN verdicts v ON v.id=r.verdict_id WHERE r.project_id=? AND r.outcome='verdict' ORDER BY r.task_id,r.repo,r.requested_head,r.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	findingsByHead := map[comparisonHead][]comparisonFinding{}
	seen := map[comparisonHead]map[comparisonFinding]bool{}
	for rows.Next() {
		var key comparisonHead
		var path string
		if err := rows.Scan(&key.TaskID, &key.Repo, &key.Head, &path); err != nil {
			return nil, err
		}
		if !requested[key] {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var findings []comparisonFinding
		if err := json.Unmarshal(data, &findings); err != nil {
			return nil, err
		}
		if seen[key] == nil {
			seen[key] = map[comparisonFinding]bool{}
		}
		for _, finding := range findings {
			if seen[key][finding] {
				continue
			}
			seen[key][finding] = true
			findingsByHead[key] = append(findingsByHead[key], finding)
		}
	}
	return findingsByHead, rows.Err()
}

func builtinReviewLatency(ctx context.Context, db *store.DB, projectID int64) (*int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT rr.created_at, (SELECT MIN(s.at) FROM signals s WHERE s.task_id=rr.review_task_id AND s.verb='done') FROM review_rounds rr JOIN tasks t ON t.id=rr.ship_task_id WHERE (?=0 OR t.project_id=?) AND rr.status='verdict'`, projectID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []int64
	for rows.Next() {
		var started int64
		var settled sql.NullInt64
		if err := rows.Scan(&started, &settled); err != nil {
			return nil, err
		}
		if settled.Valid && settled.Int64 >= started {
			values = append(values, settled.Int64-started)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, nil
	}
	median := medianInt64(values)
	return &median, nil
}

func builtinFixedFindings(ctx context.Context, db *store.DB, projectID int64) ([]builtinFixedFinding, error) {
	rows, err := db.QueryContext(ctx, `SELECT f.ship_task_id,f.head_sha,f.repository,f.path,p.root_cause FROM review_findings f JOIN review_patterns p ON p.id=f.pattern_id JOIN tasks t ON t.id=f.ship_task_id WHERE t.project_id=? AND f.origin='introduced' AND f.in_scope=1 AND f.state='fixed' AND f.severity IN ('P0','P1') ORDER BY f.ship_task_id,f.repository,f.head_sha,f.path,f.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var findings []builtinFixedFinding
	for rows.Next() {
		var finding builtinFixedFinding
		if err := rows.Scan(&finding.TaskID, &finding.Head, &finding.Repo, &finding.Path, &finding.Cause); err != nil {
			return nil, err
		}
		findings = append(findings, finding)
	}
	return findings, rows.Err()
}
