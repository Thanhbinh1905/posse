package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/thanhbinh1905/posse/internal/atomicfile"
	"github.com/thanhbinh1905/posse/internal/config"
)

const (
	forgeProbeTimeout = 500 * time.Millisecond
	forgeProbeCache   = "forge-auth-cache.json"
)

var forgeProbeCacheTTL = 5 * time.Second

type forgeProbeOutcome struct {
	GitHubChecked       bool `json:"github_checked"`
	GitLabChecked       bool `json:"gitlab_checked"`
	GitHubAuthenticated bool `json:"github_authenticated"`
	GitLabAuthenticated bool `json:"gitlab_authenticated"`
	GitHubTimedOut      bool `json:"github_timed_out"`
	GitLabTimedOut      bool `json:"gitlab_timed_out"`
}

func (o forgeProbeOutcome) authenticated(forge string) bool {
	switch forge {
	case "github":
		return o.GitHubAuthenticated
	case "gitlab":
		return o.GitLabAuthenticated
	default:
		return false
	}
}

func (o forgeProbeOutcome) timedOut(forge string) bool {
	switch forge {
	case "github":
		return o.GitHubTimedOut
	case "gitlab":
		return o.GitLabTimedOut
	default:
		return false
	}
}

func (o forgeProbeOutcome) anyTimedOut() bool {
	return o.GitHubTimedOut || o.GitLabTimedOut
}

func (o forgeProbeOutcome) covers(mode string) bool {
	switch mode {
	case "auto":
		return o.GitHubChecked && o.GitLabChecked
	case "github":
		return o.GitHubChecked
	case "gitlab":
		return o.GitLabChecked
	default:
		return false
	}
}

type forgeProbeCacheEntry struct {
	CheckedAt time.Time         `json:"checked_at"`
	Outcome   forgeProbeOutcome `json:"outcome"`
}

type forgeProbeDiskCache struct {
	Entries map[string]forgeProbeCacheEntry `json:"entries"`
}

type forgeProbeFlight struct {
	done    chan struct{}
	outcome forgeProbeOutcome
	err     error
}

var forgeProbeState = struct {
	sync.Mutex
	memory   map[string]forgeProbeCacheEntry
	inFlight map[string]*forgeProbeFlight
}{memory: map[string]forgeProbeCacheEntry{}, inFlight: map[string]*forgeProbeFlight{}}

var forgeProbeDiskMu sync.Mutex

// cachedForgeProbe shares work for one host/configuration in this process and
// persists successful, failed, and timed-out results briefly for later CLI
// calls. Config and executable identity are part of the key, so changing
// Project policy or the active CLI environment invalidates old results.
func cachedForgeProbe(ctx context.Context, root, host string, cfg config.Config, mode string) (forgeProbeOutcome, error) {
	key, home, err := forgeProbeCacheKey(host, cfg)
	if err != nil {
		return forgeProbeOutcome{}, err
	}
	memoryKey := home + "\x00" + key

	for {
		var entry forgeProbeCacheEntry
		forgeProbeState.Lock()
		if cached := forgeProbeState.memory[memoryKey]; forgeProbeCacheFresh(cached, time.Now()) {
			entry = cached
		}
		if !entry.CheckedAt.IsZero() && entry.Outcome.covers(mode) {
			forgeProbeState.Unlock()
			return entry.Outcome, nil
		}
		flight := forgeProbeState.inFlight[memoryKey]
		owner := flight == nil
		if owner {
			flight = &forgeProbeFlight{done: make(chan struct{})}
			forgeProbeState.inFlight[memoryKey] = flight
		}
		forgeProbeState.Unlock()
		if !owner {
			select {
			case <-ctx.Done():
				return forgeProbeOutcome{}, ctx.Err()
			case <-flight.done:
				if flight.err != nil {
					return forgeProbeOutcome{}, flight.err
				}
				continue
			}
		}

		if entry.CheckedAt.IsZero() {
			entry, _ = readForgeProbeCache(home, key, time.Now())
		}
		outcome := entry.Outcome
		var probeErr error
		if !forgeProbeCacheFresh(entry, time.Now()) || !outcome.covers(mode) {
			needed := missingForgeProbeMode(outcome, mode)
			var probed forgeProbeOutcome
			probed, probeErr = runForgeProbe(ctx, root, host, needed)
			outcome = mergeForgeProbeOutcomes(outcome, probed)
			if probeErr == nil {
				entry = forgeProbeCacheEntry{CheckedAt: time.Now(), Outcome: outcome}
				writeForgeProbeCache(home, key, entry)
			}
		}

		forgeProbeState.Lock()
		if probeErr == nil {
			forgeProbeState.memory[memoryKey] = entry
			pruneForgeProbeMemory(time.Now())
		}
		flight.outcome, flight.err = outcome, probeErr
		delete(forgeProbeState.inFlight, memoryKey)
		close(flight.done)
		forgeProbeState.Unlock()
		return outcome, probeErr
	}
}

func missingForgeProbeMode(outcome forgeProbeOutcome, requested string) string {
	github := requested == "auto" || requested == "github"
	gitlab := requested == "auto" || requested == "gitlab"
	github = github && !outcome.GitHubChecked
	gitlab = gitlab && !outcome.GitLabChecked
	switch {
	case github && gitlab:
		return "auto"
	case github:
		return "github"
	case gitlab:
		return "gitlab"
	default:
		return requested
	}
}

func mergeForgeProbeOutcomes(previous, current forgeProbeOutcome) forgeProbeOutcome {
	if current.GitHubChecked {
		previous.GitHubChecked = true
		previous.GitHubAuthenticated = current.GitHubAuthenticated
		previous.GitHubTimedOut = current.GitHubTimedOut
	}
	if current.GitLabChecked {
		previous.GitLabChecked = true
		previous.GitLabAuthenticated = current.GitLabAuthenticated
		previous.GitLabTimedOut = current.GitLabTimedOut
	}
	return previous
}

func runForgeProbe(ctx context.Context, root, host, mode string) (forgeProbeOutcome, error) {
	clis := []struct {
		name  string
		forge string
	}{}
	switch mode {
	case "auto":
		clis = append(clis, struct {
			name  string
			forge string
		}{"glab", "gitlab"}, struct {
			name  string
			forge string
		}{"gh", "github"})
	case "gitlab":
		clis = append(clis, struct {
			name  string
			forge string
		}{"glab", "gitlab"})
	case "github":
		clis = append(clis, struct {
			name  string
			forge string
		}{"gh", "github"})
	default:
		return forgeProbeOutcome{}, errors.New("invalid forge probe mode")
	}

	probeCtx, cancel := context.WithTimeout(ctx, forgeProbeTimeout)
	defer cancel()
	type result struct {
		forge         string
		authenticated bool
		timedOut      bool
	}
	results := make(chan result, len(clis))
	for _, cli := range clis {
		go func(name, forge string) {
			_, err := commandOutputArgsWithTimeout(probeCtx, forgeProbeTimeout, root, name, "auth", "status", "--hostname", host)
			results <- result{
				forge:         forge,
				authenticated: err == nil,
				timedOut:      errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil,
			}
		}(cli.name, cli.forge)
	}
	outcome := forgeProbeOutcome{}
	for range clis {
		result := <-results
		switch result.forge {
		case "github":
			outcome.GitHubChecked = true
			outcome.GitHubAuthenticated, outcome.GitHubTimedOut = result.authenticated, result.timedOut
		case "gitlab":
			outcome.GitLabChecked = true
			outcome.GitLabAuthenticated, outcome.GitLabTimedOut = result.authenticated, result.timedOut
		}
	}
	if err := ctx.Err(); err != nil {
		return forgeProbeOutcome{}, err
	}
	return outcome, nil
}

func forgeProbeCacheKey(host string, cfg config.Config) (string, string, error) {
	configuration, err := json.Marshal(cfg)
	if err != nil {
		return "", "", err
	}
	cliPaths := map[string]string{}
	for _, cli := range []string{"glab", "gh"} {
		path, lookupErr := exec.LookPath(cli)
		if lookupErr != nil {
			path = "<missing>"
		}
		cliPaths[cli] = path
	}
	home, homeErr := config.DefaultHome()
	if homeErr != nil {
		home = ""
	}
	identity, err := json.Marshal(struct {
		Host          string            `json:"host"`
		Configuration json.RawMessage   `json:"configuration"`
		Executables   map[string]string `json:"executables"`
		Home          string            `json:"home"`
		UserHome      string            `json:"user_home"`
		XDGConfigHome string            `json:"xdg_config_home"`
		GHConfigDir   string            `json:"gh_config_dir"`
		GLABConfigDir string            `json:"glab_config_dir"`
	}{host, configuration, cliPaths, home, os.Getenv("HOME"), os.Getenv("XDG_CONFIG_HOME"), os.Getenv("GH_CONFIG_DIR"), os.Getenv("GLAB_CONFIG_DIR")})
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(identity)
	return hex.EncodeToString(sum[:]), home, nil
}

func forgeProbeCacheFresh(entry forgeProbeCacheEntry, now time.Time) bool {
	age := now.Sub(entry.CheckedAt)
	return !entry.CheckedAt.IsZero() && age >= 0 && age < forgeProbeCacheTTL
}

// readForgeProbeCache returns only fresh outcomes so a partial refresh cannot
// merge with the unprobed portion of an expired result.
func readForgeProbeCache(home, key string, now time.Time) (forgeProbeCacheEntry, bool) {
	if home == "" {
		return forgeProbeCacheEntry{}, false
	}
	data, err := os.ReadFile(filepath.Join(home, forgeProbeCache))
	if err != nil {
		return forgeProbeCacheEntry{}, false
	}
	var cache forgeProbeDiskCache
	if json.Unmarshal(data, &cache) != nil {
		return forgeProbeCacheEntry{}, false
	}
	entry, ok := cache.Entries[key]
	if !ok || !forgeProbeCacheFresh(entry, now) {
		return forgeProbeCacheEntry{}, false
	}
	return entry, true
}

func writeForgeProbeCache(home, key string, entry forgeProbeCacheEntry) {
	if home == "" {
		return
	}
	forgeProbeDiskMu.Lock()
	defer forgeProbeDiskMu.Unlock()
	cache := forgeProbeDiskCache{Entries: map[string]forgeProbeCacheEntry{}}
	if data, err := os.ReadFile(filepath.Join(home, forgeProbeCache)); err == nil {
		_ = json.Unmarshal(data, &cache)
		if cache.Entries == nil {
			cache.Entries = map[string]forgeProbeCacheEntry{}
		}
	}
	now := time.Now()
	for cachedKey, cachedEntry := range cache.Entries {
		if !forgeProbeCacheFresh(cachedEntry, now) {
			delete(cache.Entries, cachedKey)
		}
	}
	cache.Entries[key] = entry
	data, err := json.Marshal(cache)
	if err == nil {
		_ = atomicfile.Write(filepath.Join(home, forgeProbeCache), data, 0o600)
	}
}

func pruneForgeProbeMemory(now time.Time) {
	for key, entry := range forgeProbeState.memory {
		if !forgeProbeCacheFresh(entry, now) {
			delete(forgeProbeState.memory, key)
		}
	}
}
