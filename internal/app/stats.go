package app

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

const statsWindowDays = 30

// ProfileStats summarizes one Profile's Tasks created in the stats window.
// It is informational: nothing here feeds Dispatch Rule resolution.
type ProfileStats struct {
	Profile           string  `json:"profile"`
	Tasks             int     `json:"tasks"`
	Finished          int     `json:"finished"`
	Landed            int     `json:"landed"`
	LandedRate        float64 `json:"landed_rate"`
	MedianDoneSeconds *int64  `json:"median_done_seconds"`
	TokensPerLanded   *int64  `json:"tokens_per_landed"`
	Relaunches        int     `json:"relaunches"`
}

type UsageStats struct {
	Tasks            int      `json:"tasks"`
	UnknownCostTasks int      `json:"unknown_cost_tasks"`
	UnknownCostShare float64  `json:"unknown_cost_share"`
	MedianCostUSD    *float64 `json:"median_cost_usd"`
	P90CostUSD       *float64 `json:"p90_cost_usd"`
	MedianTokens     *int64   `json:"median_tokens"`
	P90Tokens        *int64   `json:"p90_tokens"`
}

type usageSample struct {
	cost        *float64
	tokens      int64
	tokensKnown bool
}

type statsTask struct {
	profile    string
	state      store.State
	landed     bool
	doneSecs   int64 // -1 when the Task never reached done
	tokens     int64
	tokensSeen bool
	relaunches int
}

func taskFinished(state store.State) bool {
	switch state {
	case store.StateMerged, store.StateReported, store.StateFailed, store.StateLost, store.StateTornDown:
		return true
	}
	return false
}

// aggregateProfileStats groups Task facts by Profile, ordered by Profile name.
// The landed rate divides landed Tasks by finished Tasks; Tasks still in
// flight count only toward Tasks and Relaunches.
func aggregateProfileStats(tasks []statsTask) []ProfileStats {
	byProfile := map[string]*ProfileStats{}
	durations := map[string][]int64{}
	tokens := map[string][]int64{}
	for _, task := range tasks {
		stats := byProfile[task.profile]
		if stats == nil {
			stats = &ProfileStats{Profile: task.profile}
			byProfile[task.profile] = stats
		}
		stats.Tasks++
		stats.Relaunches += task.relaunches
		if task.landed || taskFinished(task.state) {
			stats.Finished++
		}
		if task.landed {
			stats.Landed++
			if task.tokensSeen {
				tokens[task.profile] = append(tokens[task.profile], task.tokens)
			}
		}
		if task.doneSecs >= 0 {
			durations[task.profile] = append(durations[task.profile], task.doneSecs)
		}
	}
	result := make([]ProfileStats, 0, len(byProfile))
	for name, stats := range byProfile {
		if stats.Finished > 0 {
			stats.LandedRate = math.Round(float64(stats.Landed)/float64(stats.Finished)*1000) / 1000
		}
		if values := durations[name]; len(values) > 0 {
			median := medianInt64(values)
			stats.MedianDoneSeconds = &median
		}
		if values := tokens[name]; len(values) > 0 {
			var total int64
			for _, value := range values {
				total += value
			}
			mean := total / int64(len(values))
			stats.TokensPerLanded = &mean
		}
		result = append(result, *stats)
	}
	slices.SortFunc(result, func(a, b ProfileStats) int { return strings.Compare(a.Profile, b.Profile) })
	return result
}

func medianInt64(values []int64) int64 {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

// projectProfileStats reads the Project's Task history for the stats window.
// Token usage is read only for landed Tasks.
func projectProfileStats(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, now time.Time, days int) ([]ProfileStats, error) {
	since := now.AddDate(0, 0, -days).UnixMilli()
	all, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return nil, err
	}
	histories, err := db.TaskHistories(ctx, project.ID, since)
	if err != nil {
		return nil, err
	}
	var tasks []statsTask
	for _, task := range all {
		if task.CreatedAt < since || task.Profile == "" || task.State == store.StateQueued {
			continue
		}
		history := histories[task.ID]
		row := statsTask{profile: task.Profile, state: task.State, doneSecs: -1, relaunches: max(task.Launches-1, 0)}
		row.landed = history.Landed || task.MergedRef != "" || task.State == store.StateMerged || task.State == store.StateReported
		if history.FirstDoneAt > 0 && history.FirstDoneAt >= task.CreatedAt {
			row.doneSecs = (history.FirstDoneAt - task.CreatedAt) / 1000
		}
		if row.landed {
			counts := taskUsage(ctx, db, task, cfg)
			row.tokensSeen = counts.Known
			row.tokens = counts.Input + counts.Output + counts.CacheRead + counts.CacheWrite
		}
		tasks = append(tasks, row)
	}
	return aggregateProfileStats(tasks), nil
}

// statsSummary renders one Profile's stats as a single line.
func statsSummary(stats ProfileStats) string {
	if stats.Tasks == 0 {
		return fmt.Sprintf("no Tasks in the last %d days", statsWindowDays)
	}
	landed := "landed n/a (0 finished)"
	if stats.Finished > 0 {
		landed = fmt.Sprintf("landed %.0f%% (%d/%d)", stats.LandedRate*100, stats.Landed, stats.Finished)
	}
	median, tokens := "n/a", "n/a"
	if stats.MedianDoneSeconds != nil {
		median = (time.Duration(*stats.MedianDoneSeconds) * time.Second).String()
	}
	if stats.TokensPerLanded != nil {
		tokens = fmt.Sprintf("%d", *stats.TokensPerLanded)
	}
	return fmt.Sprintf("%s, median to done %s, tokens per landed %s, relaunches %d", landed, median, tokens, stats.Relaunches)
}

// statsFor returns the stats row for a Profile, or an empty row when it has no
// recent Tasks.
func statsFor(stats []ProfileStats, profile string) ProfileStats {
	for _, row := range stats {
		if row.Profile == profile {
			return row
		}
	}
	return ProfileStats{Profile: profile}
}

func projectUsageStats(ctx context.Context, db *store.DB, project store.Project, days int, now time.Time) (map[string]any, error) {
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return nil, err
	}
	since := now.AddDate(0, 0, -days).UnixMilli()
	var all []usageSample
	byType, byProfile := map[string][]usageSample{}, map[string][]usageSample{}
	for _, task := range tasks {
		if task.CreatedAt < since || task.State == store.StateQueued {
			continue
		}
		total, err := db.TaskUsageTotal(ctx, task.ID)
		if err != nil {
			return nil, err
		}
		sample := usageSample{tokens: total.Input + total.Output + total.CacheRead + total.CacheWrite, tokensKnown: total.Known}
		if total.KnownCost {
			sample.cost = total.CostUSD
		}
		all = append(all, sample)
		byType[task.Type] = append(byType[task.Type], sample)
		launches, err := db.TaskLaunchUsages(ctx, task.ID)
		if err != nil {
			return nil, err
		}
		if len(launches) == 0 {
			byProfile[task.Profile] = append(byProfile[task.Profile], usageSample{})
			continue
		}
		perProfile := map[string]usageSample{}
		unknownCost := map[string]bool{}
		for _, launch := range launches {
			profile := launch.Profile
			if profile == "" {
				profile = task.Profile
			}
			current, exists := perProfile[profile]
			if !exists {
				current.tokensKnown = true
			}
			current.tokens += launch.Input + launch.Output + launch.CacheRead + launch.CacheWrite
			if launch.CostUSD == nil {
				unknownCost[profile] = true
			} else if current.cost == nil {
				value := *launch.CostUSD
				current.cost = &value
			} else {
				value := *current.cost + *launch.CostUSD
				current.cost = &value
			}
			perProfile[profile] = current
		}
		for profile, item := range perProfile {
			if unknownCost[profile] {
				item.cost = nil
			}
			byProfile[profile] = append(byProfile[profile], item)
		}
	}
	grouped := func(groups map[string][]usageSample) map[string]UsageStats {
		result := make(map[string]UsageStats, len(groups))
		for key, samples := range groups {
			result[key] = summarizeUsage(samples)
		}
		return result
	}
	return map[string]any{"overall": summarizeUsage(all), "by_type": grouped(byType), "by_profile": grouped(byProfile)}, nil
}

func summarizeUsage(samples []usageSample) UsageStats {
	stats := UsageStats{Tasks: len(samples)}
	var costs, tokens []float64
	var tokenValues []int64
	for _, sample := range samples {
		if sample.cost == nil {
			stats.UnknownCostTasks++
		} else {
			costs = append(costs, *sample.cost)
		}
		if sample.tokensKnown {
			tokens = append(tokens, float64(sample.tokens))
			tokenValues = append(tokenValues, sample.tokens)
		}
	}
	if stats.Tasks > 0 {
		stats.UnknownCostShare = float64(stats.UnknownCostTasks) / float64(stats.Tasks)
	}
	if len(costs) > 0 {
		median, p90 := quantileFloat(costs, 0.5), quantileFloat(costs, 0.9)
		stats.MedianCostUSD, stats.P90CostUSD = &median, &p90
	}
	if len(tokenValues) > 0 {
		median := int64(quantileFloat(tokens, 0.5))
		p90 := int64(quantileFloat(tokens, 0.9))
		stats.MedianTokens, stats.P90Tokens = &median, &p90
	}
	return stats
}

func quantileFloat(values []float64, q float64) float64 {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	if q == 0.5 && len(sorted)%2 == 0 {
		return (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2
	}
	index := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[max(index, 0)]
}

func (s *Service) stats(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("stats", args, map[string]flagSpec{"days": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("stats does not take positional arguments")
	}
	days := statsWindowDays
	if value := parsed.Flags["days"]; value != "" {
		parsedDays, parseErr := strconv.Atoi(value)
		if parseErr != nil || parsedDays < 1 || parsedDays > 3650 {
			return axi.Usage("--days must be an integer from 1 to 3650")
		}
		days = parsedDays
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
	if err := s.requireLead(ctx.Context, db, project); err != nil {
		return err
	}
	cfg, err := s.prepareProjectForTask(ctx.Context, db, project)
	if err != nil {
		return err
	}
	stats, err := projectProfileStats(ctx.Context, db, project, cfg, time.Now(), days)
	if err != nil {
		return err
	}
	rows := make([]any, 0, len(stats))
	for _, row := range stats {
		rows = append(rows, row)
	}
	help := []string{"Stats inform Profile choice with `posse start --profile <name>`; they never change Dispatch Rules"}
	if len(rows) == 0 {
		help = []string{"No Tasks started in the last 30 days"}
	}
	usageStats, err := projectUsageStats(ctx.Context, db, project, days, time.Now())
	if err != nil {
		return err
	}
	reviewUsage, err := db.ProjectReviewUsageTotal(ctx.Context, project.ID, time.Now().AddDate(0, 0, -days).UnixMilli())
	if err != nil {
		return err
	}
	comparison, err := reviewComparison(ctx.Context, db)
	if err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "window_days", Value: days}, {Key: "profiles", Value: rows}, {Key: "usage", Value: usageStats}, {Key: "review_cost_usd", Value: reviewUsage.CostUSD}, {Key: "review_cost_unknown_requests", Value: reviewUsage.Unknown}, {Key: "review_comparison", Value: comparison}, {Key: "help", Value: help}})
}
