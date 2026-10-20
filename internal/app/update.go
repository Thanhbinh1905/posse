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
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/atomicfile"
	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
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

type updateConfigRepair struct {
	Key     string   `json:"key"`
	Message string   `json:"message"`
	Help    []string `json:"help"`
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
func (s *Service) readCachedRelease() (githubRelease, bool) {
	home, err := s.homePath()
	if err != nil {
		return githubRelease{}, false
	}
	data, _ := os.ReadFile(filepath.Join(home, "update-check.json"))
	var cache updateCache
	if json.Unmarshal(data, &cache) == nil && time.Since(cache.Checked) >= 0 && time.Since(cache.Checked) < 24*time.Hour {
		return cache.Release, cache.Release.Tag != ""
	}
	return githubRelease{}, false
}

func writeUpdateCache(home string, cache updateCache) error {
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(home, "update-check.json"), data, 0o600)
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
	parsed, err := parseArgs("update", args, map[string]flagSpec{"check": {boolean: true}, "version": {}, "force": {boolean: true}, "stop-watches": {boolean: true}})
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
	if parsed.Bool("stop-watches") && parsed.Bool("check") {
		return axi.Usage("--stop-watches cannot be combined with --check")
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
	watches, err := s.runningWatches(ctx.Context, home)
	if err != nil {
		return axi.Failure("watch_discovery_failed", err.Error(), false, "Check process-table access and run `posse update --check` again")
	}
	release, err := s.fetchRelease(ctx.Context, tag)
	if err != nil {
		message := err.Error()
		if parsed.Bool("check") && len(watches) > 0 {
			message += "; running watches: " + formatWatchProcesses(watches)
		}
		return axi.Failure("release_unavailable", message, true, "Retry when GitHub is reachable")
	}
	if parsed.Bool("check") {
		if tag == "" {
			if err := writeUpdateCache(home, updateCache{Checked: time.Now(), Release: release}); err != nil {
				return axi.Failure("update_cache_failed", "could not save the release check: "+err.Error(), true, "Retry `posse update --check`")
			}
		}
		help := []any{updateInstallHelp}
		if len(watches) > 0 {
			help = append(help, "Running Watches re-exec automatically after an update; pass `--stop-watches` only to force-stop them")
		}
		return ctx.Print(axi.Object{{Key: "update", Value: axi.Row{{Key: "current", Value: s.currentVersion()}, {Key: "latest", Value: release.Tag}}}, {Key: "changelog", Value: release.Body}, {Key: "watches", Value: watchRows(watches, false)}, {Key: "help", Value: help}})
	}
	if tag == "" && s.currentVersion() != "dev" && !newerVersion(s.currentVersion(), release.Tag) {
		return axi.Failure("already_current", "no newer release is available", false, "Run `posse setup` to finish or repair integration setup", "Use `posse update --version vX.Y.Z` to select a release")
	}
	var installed updateInstallResult
	var stopped []watchProcess
	if err := s.installRelease(ctx.Context, release, parsed.Bool("force"), parsed.Bool("stop-watches"), &installed, &stopped); err != nil {
		return err
	}
	result := axi.Object{{Key: "installed", Value: release.Tag}, {Key: "binary", Value: installed.binary}, {Key: "backup", Value: installed.backup}, {Key: "stopped_watches", Value: watchRows(stopped, true)}, {Key: "reexec_watches", Value: watchRows(installed.reexecWatches, true)}, {Key: "help", Value: []any{"Run `posse doctor` to verify the update"}}}
	if installed.reexecWarning != "" {
		result = append(result, axi.Field{Key: "watch_reexec_warning", Value: installed.reexecWarning})
	}
	if installed.backupWarning != "" {
		result = append(result, axi.Field{Key: "binary_backup_warning", Value: installed.backupWarning})
	}
	return ctx.Print(result)
}

func watchRows(processes []watchProcess, includeRestart bool) []axi.Object {
	rows := make([]axi.Object, 0, len(processes))
	for _, process := range processes {
		row := axi.Object{
			{Key: "pid", Value: process.PID},
			{Key: "project", Value: process.Project},
			{Key: "kind", Value: process.Kind},
		}
		if includeRestart {
			restart := "The Lead restarts its background Watch on its next wake"
			if process.PollOnly {
				restart = "The Project watch re-execs this process after installation, or the next posse command repairs it"
			} else if process.StopHook {
				restart = "The next Claude Stop hook owns the next bounded Notice wait"
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
		return axi.Failure("worker_forbidden", "a Worker cannot update its own posse home", false)
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
			return axi.Failure("update_from_turn", "a Worker pane cannot install a Posse update", false, updateInstallHelp)
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
type updateInstallResult struct {
	binary, backup string
	reexecWatches  []watchProcess
	reexecWarning  string
	backupWarning  string
}

func (s *Service) installRelease(ctx context.Context, release githubRelease, force, stopWatches bool, installed *updateInstallResult, stopped *[]watchProcess) error {
	home, err := s.homePath()
	if err != nil {
		return err
	}
	if err := s.requireUserUpdateCaller(ctx, home); err != nil {
		return err
	}
	watches, err := s.runningWatches(ctx, home)
	if err != nil {
		return axi.Failure("watch_discovery_failed", err.Error(), false, "Check process-table access and run `posse update --check` again")
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
	preflight := exec.CommandContext(ctx, temp.Name(), "_update-preflight", "--json")
	preflight.Env = append(os.Environ(), "POSSE_HOME="+home)
	if force {
		preflight.Env = append(preflight.Env, store.ForceMigrationEnv+"=1")
	}
	preflightOutput, err := preflight.CombinedOutput()
	if err != nil {
		var failure struct {
			Code      string   `json:"code"`
			Message   string   `json:"message"`
			Retryable bool     `json:"retryable"`
			Help      []string `json:"help"`
		}
		if json.Unmarshal(preflightOutput, &failure) == nil && failure.Code != "" {
			return axi.Failure(failure.Code, failure.Message, failure.Retryable, failure.Help...)
		}
		return axi.Failure("migration_refused", strings.TrimSpace(string(preflightOutput)), false, "Finish live Tasks or retry with `posse update --force`")
	}
	var preflightResult struct {
		ConfigRepair *updateConfigRepair `json:"config_repair"`
	}
	if len(bytes.TrimSpace(preflightOutput)) > 0 {
		if err := json.Unmarshal(preflightOutput, &preflightResult); err != nil {
			return axi.Failure("update_preflight_invalid", "target binary returned an invalid preflight result", false, strings.TrimSpace(string(preflightOutput)))
		}
	}
	backup := ""
	migrationState := store.MigrationState{}
	db, dbErr := store.OpenReadOnly(home)
	if dbErr == nil {
		migrationState, err = db.MigrationState(ctx)
		if err != nil {
			db.Close()
			return err
		}
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
	binaryBackup := ""
	if preflightResult.ConfigRepair == nil {
		binaryBackup, err = backupExecutable(destination)
		if err != nil {
			return fmt.Errorf("backup installed binary: %w", err)
		}
	}
	if stopWatches {
		current, err := s.runningWatches(ctx, home)
		if err != nil {
			_ = os.Remove(binaryBackup)
			return axi.Failure("watch_discovery_failed", err.Error(), false, "Check process-table access and retry the update")
		}
		stoppedProcesses, err := stopWatchProcesses(ctx, home, current)
		if err != nil {
			_ = os.Remove(binaryBackup)
			return axi.Failure("watch_stop_failed", err.Error(), false, "Inspect the listed process IDs before retrying")
		}
		*stopped = stoppedProcesses
	}
	if err := os.Rename(temp.Name(), destination); err != nil {
		if binaryBackup != "" {
			_ = os.Remove(binaryBackup)
		}
		return fmt.Errorf("install binary: %w", err)
	}
	if preflightResult.ConfigRepair != nil {
		installed.binary, installed.backup = destination, backup
		repair := preflightResult.ConfigRepair
		return axi.Failure("config_repair_required", fmt.Sprintf("installed %s; setup was skipped until configuration is repaired: %s", release.Tag, repair.Message), false, repair.Help...)
	}
	output, err := s.runUpdateSetup(ctx, destination, home, force)
	if err != nil {
		var failure struct {
			Code    string   `json:"code"`
			Message string   `json:"message"`
			Help    []string `json:"help"`
		}
		if json.Unmarshal(output, &failure) == nil && failure.Code == "config_invalid" {
			if _, repairable := configRepairForError(config.ValidateAll(home)); repairable {
				help := append([]string(nil), failure.Help...)
				if err := os.Remove(binaryBackup); err != nil && !errors.Is(err, os.ErrNotExist) {
					help = append(help, "Previous binary backup remains at "+binaryBackup+": "+err.Error())
				}
				return axi.Failure("config_repair_required", fmt.Sprintf("installed %s; setup could not complete until configuration is repaired: %s", release.Tag, failure.Message), false, help...)
			}
		}
		afterMigrationState, stateErr := store.ReadMigrationState(ctx, home)
		if stateErr != nil || !reflect.DeepEqual(migrationState, afterMigrationState) {
			help := []string{"Resolve the setup failure and run `posse setup` to retry with the installed binary"}
			if removeErr := os.Remove(binaryBackup); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				help = append(help, "Previous binary backup remains at "+binaryBackup+": "+removeErr.Error())
			}
			message := "setup failed; database migration state changed, so the new binary remains installed"
			if stateErr != nil {
				message = "setup failed; database migration state could not be verified, so the new binary remains installed: " + stateErr.Error()
			}
			return axi.Failure("setup_failed", fmt.Sprintf("%s: %s", message, strings.TrimSpace(string(output))), false, help...)
		}
		if rollbackErr := os.Rename(binaryBackup, destination); rollbackErr != nil {
			return axi.Failure("setup_failed", fmt.Sprintf("setup failed and the previous binary could not be restored: %s", strings.TrimSpace(string(output))), false, "Restore the previous binary from "+binaryBackup, rollbackErr.Error())
		}
		return axi.Failure("setup_failed", fmt.Sprintf("setup failed before any database migrations were written; the previous binary was restored: %s", strings.TrimSpace(string(output))), false, "Resolve the setup failure and retry `posse update`")
	}
	if err := os.Remove(binaryBackup); err != nil && !errors.Is(err, os.ErrNotExist) {
		installed.backupWarning = "could not remove previous binary backup " + binaryBackup + ": " + err.Error()
	}
	installed.binary, installed.backup = destination, backup
	if !stopWatches && len(watches) > 0 {
		if err := requestWatchReexec(home, watches); err != nil {
			installed.reexecWarning = err.Error()
		} else {
			installed.reexecWatches = watches
		}
	}
	return nil
}
func (s *Service) runUpdateSetup(ctx context.Context, destination, home string, force bool) ([]byte, error) {
	if s.updateSetup != nil {
		return s.updateSetup(ctx, destination, home, force)
	}
	setup := exec.CommandContext(ctx, destination, "setup", "--binary", destination, "--json")
	setup.Env = append(os.Environ(), "POSSE_HOME="+home)
	if force {
		setup.Env = append(setup.Env, store.ForceMigrationEnv+"=1")
	}
	return setup.CombinedOutput()
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
func configRepairForError(err error) (*updateConfigRepair, bool) {
	var invalid *config.InvalidError
	if !errors.As(err, &invalid) {
		return nil, false
	}
	if _, ok := config.Lookup(invalid.Key); !ok {
		return nil, false
	}
	return &updateConfigRepair{Key: invalid.Key, Message: invalid.Error(), Help: configRepairHelp(invalid)}, true
}

func backupExecutable(path string) (string, error) {
	source, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return "", err
	}
	backup, err := os.CreateTemp(filepath.Dir(path), ".posse-update-rollback-*")
	if err != nil {
		return "", err
	}
	backupPath := backup.Name()
	complete := false
	defer func() {
		if !complete {
			_ = backup.Close()
			_ = os.Remove(backupPath)
		}
	}()
	if err := backup.Chmod(info.Mode().Perm()); err != nil {
		return "", err
	}
	if _, err := io.Copy(backup, source); err != nil {
		return "", err
	}
	if err := backup.Sync(); err != nil {
		return "", err
	}
	if err := backup.Close(); err != nil {
		return "", err
	}
	complete = true
	return backupPath, nil
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
		return axi.Failure("worker_forbidden", "a Worker cannot update its home", false)
	}
	var repair *updateConfigRepair
	if err := config.ValidateAll(home); err != nil {
		var ok bool
		repair, ok = configRepairForError(err)
		if !ok {
			return configError(err)
		}
	}
	db, err := store.OpenReadOnly(home)
	if errors.Is(err, os.ErrNotExist) {
		return printUpdateConfigRepair(ctx, repair)
	}
	if err != nil {
		return err
	}
	defer db.Close()
	pending, err := db.PendingIdleMigrations(ctx.Context)
	if err != nil {
		return err
	}
	if err := schemaFailure(db.RefuseLiveMigration(ctx.Context, pending, os.Getenv(store.ForceMigrationEnv) == "1")); err != nil {
		return err
	}
	return printUpdateConfigRepair(ctx, repair)
}

func printUpdateConfigRepair(ctx *axi.Context, repair *updateConfigRepair) error {
	if repair == nil {
		return nil
	}
	return ctx.Print(axi.Object{{Key: "config_repair", Value: repair}, {Key: "help", Value: repair.Help}})
}
