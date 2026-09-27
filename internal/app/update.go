package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/atomicfile"
	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

const releaseAPI = "https://api.github.com/repos/Thanhbinh1905/posse/releases"
const updateInstallHelp = "Run `posse update` from a live User shell pane or a terminal outside Herdr"

var releaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

type githubRelease struct {
	Tag    string `json:"tag_name"`
	Body   string `json:"body"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

type updateCache struct {
	Checked time.Time     `json:"checked"`
	Release githubRelease `json:"release"`
}

func (s *Service) releaseClient() *http.Client {
	if s.updateClient != nil {
		return s.updateClient
	}
	return &http.Client{Timeout: 8 * time.Second}
}
func (s *Service) releaseEndpoint() string {
	if s.updateURL != "" {
		return s.updateURL
	}
	return releaseAPI
}
func (s *Service) fetchRelease(ctx context.Context, tag string) (githubRelease, error) {
	endpoint := s.releaseEndpoint() + "/latest"
	if tag != "" {
		endpoint = s.releaseEndpoint() + "/tags/" + tag
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return githubRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "posse-update")
	resp, err := s.releaseClient().Do(req)
	if err != nil {
		return githubRelease{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return githubRelease{}, fmt.Errorf("GitHub release: HTTP %d", resp.StatusCode)
	}
	var release githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&release); err != nil {
		return release, err
	}
	if !releaseVersion.MatchString(release.Tag) || tag != "" && tag != release.Tag {
		return release, fmt.Errorf("invalid release tag %q", release.Tag)
	}
	return release, nil
}
func (s *Service) currentVersion() string {
	if s.Version != "" {
		return s.Version
	}
	return "dev"
}
func (s *Service) cachedRelease(ctx context.Context) (githubRelease, bool) {
	home, err := s.homePath()
	if err != nil {
		return githubRelease{}, false
	}
	cachePath := filepath.Join(home, "update-check.json")
	data, _ := os.ReadFile(cachePath)
	var cache updateCache
	if json.Unmarshal(data, &cache) == nil && time.Since(cache.Checked) >= 0 && time.Since(cache.Checked) < 24*time.Hour {
		return cache.Release, cache.Release.Tag != ""
	}
	// Record the attempt, including failures, to avoid polling while offline.
	cache = updateCache{Checked: time.Now()}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	release, err := s.fetchRelease(probe, "")
	if err == nil {
		cache.Release = release
	}
	if data, err := json.Marshal(cache); err == nil {
		if os.MkdirAll(home, 0o700) == nil {
			_ = atomicfile.Write(cachePath, data, 0o600)
		}
	}
	return cache.Release, err == nil
}
func newerVersion(current, latest string) bool {
	if current == "dev" || !releaseVersion.MatchString(latest) {
		return false
	}
	if !releaseVersion.MatchString(current) {
		current = "v" + current
	}
	if !releaseVersion.MatchString(current) {
		return false
	}
	var a, b, c, x, y, z int
	fmt.Sscanf(current, "v%d.%d.%d", &a, &b, &c)
	fmt.Sscanf(latest, "v%d.%d.%d", &x, &y, &z)
	return x > a || x == a && (y > b || y == b && z > c)
}
func (s *Service) update(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("update", args, map[string]flagSpec{"check": {boolean: true}, "version": {}, "force": {boolean: true}, "stop-lookouts": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) > 0 {
		return axi.Usage("update takes no positional arguments")
	}
	tag := parsed.Flags["version"]
	if tag != "" && !releaseVersion.MatchString(tag) {
		return axi.Usage("--version must be vX.Y.Z")
	}
	if parsed.Bool("force") && parsed.Bool("check") {
		return axi.Usage("--force cannot be combined with --check")
	}
	if parsed.Bool("stop-lookouts") && parsed.Bool("check") {
		return axi.Usage("--stop-lookouts cannot be combined with --check")
	}
	home, err := s.homePath()
	if err != nil {
		return err
	}
	if !parsed.Bool("check") {
		if err := s.requireUserUpdateCaller(ctx.Context, home); err != nil {
			return err
		}
	}
	lookouts, err := s.runningLookouts(ctx.Context, home)
	if err != nil {
		return axi.Failure("lookout_discovery_failed", err.Error(), false, "Check process-table access and run `posse update --check` again")
	}
	if !parsed.Bool("check") && !parsed.Bool("stop-lookouts") && len(lookouts) > 0 {
		return lookoutsRunningFailure(lookouts)
	}
	release, err := s.fetchRelease(ctx.Context, tag)
	if err != nil {
		message := err.Error()
		if parsed.Bool("check") && len(lookouts) > 0 {
			message += "; running lookouts: " + formatLookoutProcesses(lookouts)
		}
		return axi.Failure("release_unavailable", message, true, "Retry when GitHub is reachable")
	}
	if parsed.Bool("check") {
		help := []any{updateInstallHelp}
		if len(lookouts) > 0 {
			help = append(help, "Stop running lookouts with `posse update --stop-lookouts` before installing")
		}
		return ctx.Print(axi.Object{{Key: "update", Value: axi.Row{{Key: "current", Value: s.currentVersion()}, {Key: "latest", Value: release.Tag}}}, {Key: "changelog", Value: release.Body}, {Key: "lookouts", Value: lookoutRows(lookouts, false)}, {Key: "help", Value: help}})
	}
	if tag == "" && s.currentVersion() != "dev" && !newerVersion(s.currentVersion(), release.Tag) {
		return axi.Failure("already_current", "no newer release is available", false, "Use `posse update --version vX.Y.Z` to select a release")
	}
	var installed updateInstallResult
	var stopped []lookoutProcess
	if err := s.installRelease(ctx.Context, release, parsed.Bool("force"), parsed.Bool("stop-lookouts"), &installed, &stopped); err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "installed", Value: release.Tag}, {Key: "binary", Value: installed.binary}, {Key: "backup", Value: installed.backup}, {Key: "stopped_lookouts", Value: lookoutRows(stopped, true)}, {Key: "help", Value: []any{"Run `posse doctor` to verify the update"}}})
}

func lookoutsRunningFailure(processes []lookoutProcess) error {
	message := "Posse update is blocked while lookouts are running: " + formatLookoutProcesses(processes)
	return axi.Failure("lookouts_running", message, false, "Run `posse update --stop-lookouts` to stop them before updating")
}

func lookoutRows(processes []lookoutProcess, includeRestart bool) []axi.Object {
	rows := make([]axi.Object, 0, len(processes))
	for _, process := range processes {
		row := axi.Object{
			{Key: "pid", Value: process.PID},
			{Key: "project", Value: process.Project},
			{Key: "kind", Value: process.Kind},
		}
		if includeRestart {
			restart := "The Lead restarts its background lookout on its next wake"
			if process.PollOnly {
				restart = "The Lookout tab owner restarts this process on its next tick"
			} else if process.QuietRoutine {
				restart = "The Lead extension restarts this process automatically"
			}
			if process.ForceKilled {
				restart += " (SIGKILL required)"
			}
			row = append(row, axi.Field{Key: "restart", Value: restart})
		}
		rows = append(rows, row)
	}
	return rows
}

func (s *Service) requireUserUpdateCaller(ctx context.Context, home string) error {
	if os.Getenv(workerHomeEnv) != "" || s.workerCaller(ctx, home) {
		return axi.Failure("worker_forbidden", "a Rider cannot update its own posse home", false)
	}
	if s.herdrContext == nil || !s.herdrContext() {
		return nil
	}
	paneID := os.Getenv("HERDR_PANE_ID")
	if paneID == "" {
		return axi.Failure("update_pane_unknown", "posse cannot identify this Herdr pane", false, updateInstallHelp)
	}
	for _, ancestor := range processAncestors(procRoot, os.Getpid()) {
		if inherited := ancestor.env["HERDR_PANE_ID"]; inherited != "" && inherited != paneID {
			return axi.Failure("update_pane_unknown", "this process inherited a different Herdr pane", false, updateInstallHelp)
		}
	}
	db, err := store.OpenReadOnly(home)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return axi.Failure("update_pane_unknown", "posse cannot verify the pane's role", false, updateInstallHelp)
	}
	if db != nil {
		defer db.Close()
		if _, err := db.TaskByPane(ctx, paneID); err == nil {
			return axi.Failure("update_from_turn", "a Rider pane cannot install a Posse update", false, updateInstallHelp)
		} else if !store.IsNotFound(err) {
			return axi.Failure("update_pane_unknown", "posse cannot verify the pane's role", false, updateInstallHelp)
		}
		if _, err := db.ProjectByLeadPane(ctx, paneID); err == nil {
			return axi.Failure("update_from_turn", "a Lead pane cannot install a Posse update", false, updateInstallHelp)
		} else if !store.IsNotFound(err) {
			return axi.Failure("update_pane_unknown", "posse cannot verify the pane's role", false, updateInstallHelp)
		}
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return axi.Failure("update_pane_unknown", "posse cannot inspect this Herdr pane", false, updateInstallHelp)
	}
	for _, pane := range snapshot.Panes {
		if pane.PaneID != paneID {
			continue
		}
		// Herdr sets agent_status to "unknown" on plain shell panes, so only
		// a detected agent marks an agent pane.
		if pane.Agent != "" {
			return axi.Failure("update_from_turn", "an agent pane cannot install a Posse update", false, updateInstallHelp)
		}
		return nil
	}
	return axi.Failure("update_pane_unknown", "this Herdr pane no longer exists", false, updateInstallHelp)
}

// installRelease is called only after explicit consent, either by `update` or
// by an interactive shell's `up` prompt.
type updateInstallResult struct{ binary, backup string }

func (s *Service) installRelease(ctx context.Context, release githubRelease, force, stopLookouts bool, installed *updateInstallResult, stopped *[]lookoutProcess) error {
	home, err := s.homePath()
	if err != nil {
		return err
	}
	if err := s.requireUserUpdateCaller(ctx, home); err != nil {
		return err
	}
	lookouts, err := s.runningLookouts(ctx, home)
	if err != nil {
		return axi.Failure("lookout_discovery_failed", err.Error(), false, "Check process-table access and run `posse update --check` again")
	}
	if len(lookouts) > 0 && !stopLookouts {
		return lookoutsRunningFailure(lookouts)
	}
	manifest, found, err := readSetupManifest(filepath.Join(home, setupManifestName))
	if err != nil {
		return err
	}
	destination := ""
	if found {
		destination = manifest.Binary
	}
	if destination == "" {
		destination, err = os.Executable()
		if err != nil {
			return err
		}
	}
	destination, err = filepath.EvalSymlinks(destination)
	if err != nil {
		return err
	}
	archiveName := fmt.Sprintf("posse_%s_%s_%s.tar.gz", strings.TrimPrefix(release.Tag, "v"), runtime.GOOS, runtime.GOARCH)
	archiveURL, checksumsURL := "", ""
	for _, asset := range release.Assets {
		if asset.Name == archiveName {
			archiveURL = asset.URL
		}
		if asset.Name == "checksums.txt" {
			checksumsURL = asset.URL
		}
	}
	if archiveURL == "" || checksumsURL == "" {
		return axi.Failure("release_assets_missing", "release lacks archive or checksums.txt for this platform", false)
	}
	archive, err := s.downloadAsset(ctx, archiveURL, 200<<20)
	if err != nil {
		return err
	}
	checksums, err := s.downloadAsset(ctx, checksumsURL, 2<<20)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archive)
	verified := false
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == archiveName {
			verified = strings.EqualFold(fields[0], hex.EncodeToString(sum[:]))
			break
		}
	}
	if !verified {
		return axi.Failure("checksum_mismatch", "release archive SHA-256 does not match checksums.txt", false, "Do not install this archive")
	}
	binary, err := extractPosse(archive)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(destination), ".posse-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(0o755); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(binary); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	// The downloaded binary knows which of its migrations require idle Tasks.
	preflight := exec.CommandContext(ctx, temp.Name(), "_update-preflight")
	preflight.Env = append(os.Environ(), "POSSE_HOME="+home)
	if force {
		preflight.Env = append(preflight.Env, store.ForceMigrationEnv+"=1")
	}
	if output, err := preflight.CombinedOutput(); err != nil {
		return axi.Failure("migration_refused", strings.TrimSpace(string(output)), false, "Finish live Tasks or retry with `posse update --force`")
	}
	backup := ""
	db, dbErr := store.OpenReadOnly(home)
	if dbErr == nil {
		backup = filepath.Join(home, "backup", "posse-pre-"+release.Tag+".db")
		if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
			db.Close()
			return err
		}
		// Never overwrite an earlier pre-update backup for the same version.
		if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
			quoted := strings.ReplaceAll(backup, "'", "''")
			if _, err := db.ExecContext(ctx, "VACUUM INTO '"+quoted+"'"); err != nil {
				db.Close()
				_ = os.Remove(backup)
				return fmt.Errorf("backup database: %w", err)
			}
			if err := os.Chmod(backup, 0o600); err != nil {
				db.Close()
				return err
			}
		} else if err != nil {
			db.Close()
			return err
		}
		if err := db.Close(); err != nil {
			return err
		}
	} else if !errors.Is(dbErr, os.ErrNotExist) {
		return dbErr
	}
	if stopLookouts {
		current, err := s.runningLookouts(ctx, home)
		if err != nil {
			return axi.Failure("lookout_discovery_failed", err.Error(), false, "Check process-table access and retry the update")
		}
		stoppedProcesses, err := stopLookoutProcesses(ctx, home, current)
		if err != nil {
			return axi.Failure("lookout_stop_failed", err.Error(), false, "Inspect the listed process IDs before retrying")
		}
		*stopped = stoppedProcesses
	}
	if err := os.Rename(temp.Name(), destination); err != nil {
		return fmt.Errorf("install binary: %w", err)
	}
	setup := exec.CommandContext(ctx, destination, "setup", "--binary", destination)
	setup.Env = append(os.Environ(), "POSSE_HOME="+home)
	if force {
		setup.Env = append(setup.Env, store.ForceMigrationEnv+"=1")
	}
	output, err := setup.CombinedOutput()
	if err != nil {
		return axi.Failure("setup_failed", fmt.Sprintf("installed %s but setup failed: %s", release.Tag, strings.TrimSpace(string(output))), false, "Run `posse setup` after resolving the reported problem")
	}
	installed.binary, installed.backup = destination, backup
	return nil
}
func (s *Service) downloadAsset(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.releaseClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download %s exceeds size limit", url)
	}
	return data, nil
}
func extractPosse(data []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	archive := tar.NewReader(gz)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Name != "posse" || header.Typeflag != tar.TypeReg {
			continue
		}
		if header.Size < 1 || header.Size > 150<<20 {
			return nil, fmt.Errorf("invalid release binary size")
		}
		return io.ReadAll(io.LimitReader(archive, header.Size))
	}
	return nil, fmt.Errorf("archive does not contain posse")
}
func (s *Service) updatePreflight(ctx *axi.Context, args []string) error {
	if len(args) > 0 {
		return axi.Usage("_update-preflight takes no arguments")
	}
	home, err := s.homePath()
	if err != nil {
		return err
	}
	if s.workerCaller(ctx.Context, home) {
		return axi.Failure("worker_forbidden", "a Rider cannot update its home", false)
	}
	db, err := store.OpenReadOnly(home)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer db.Close()
	pending, err := db.PendingIdleMigrations(ctx.Context)
	if err != nil {
		return err
	}
	return schemaFailure(db.RefuseLiveMigration(ctx.Context, pending, os.Getenv(store.ForceMigrationEnv) == "1"))
}
