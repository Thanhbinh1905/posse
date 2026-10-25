package app

import (
	"context"
	"fmt"
	"math"
	"slices"
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
func projectProfileStats(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, now time.Time) ([]ProfileStats, error) {
	since := now.AddDate(0, 0, -statsWindowDays).UnixMilli()
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
			counts := taskUsage(task, cfg)
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

func (s *Service) stats(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("stats", args, map[string]flagSpec{})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("stats does not take positional arguments")
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
	stats, err := projectProfileStats(ctx.Context, db, project, cfg, time.Now())
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
	return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "window_days", Value: statsWindowDays}, {Key: "profiles", Value: rows}, {Key: "help", Value: help}})
}
