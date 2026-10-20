package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func fakeRelease(t *testing.T, checksum string) (*httptest.Server, string) {
	t.Helper()
	var url string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest", "/tags/v0.2.0":
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v0.2.0", "body": "### Bug Fixes\n* repair", "assets": []any{map[string]string{"name": "posse_0.2.0_linux_amd64.tar.gz", "browser_download_url": url + "/archive"}, map[string]string{"name": "checksums.txt", "browser_download_url": url + "/checksums"}}})
		case "/archive":
			fmt.Fprint(w, "not an archive")
		case "/checksums":
			fmt.Fprintf(w, "%s  posse_0.2.0_linux_amd64.tar.gz\n", checksum)
		default:
			http.NotFound(w, r)
		}
	}))
	url = server.URL
	return server, url
}

func buildReleaseArchive(t *testing.T, tag string) ([]byte, string, []byte, string) {
	t.Helper()
	root := filepath.Clean(filepath.Join("..", ".."))
	binaryPath := filepath.Join(t.TempDir(), "posse-current")
	build := exec.Command("go", "build", "-o", binaryPath, "./cmd/posse")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build current release binary: %v\n%s", err, output)
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	var archiveBody bytes.Buffer
	gz := gzip.NewWriter(&archiveBody)
	tarball := tar.NewWriter(gz)
	archiveName := fmt.Sprintf("posse_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), runtime.GOOS, runtime.GOARCH)
	if err := tarball.WriteHeader(&tar.Header{Name: "posse", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarball.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tarball.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive := archiveBody.Bytes()
	digest := sha256.Sum256(archive)
	return binary, archiveName, archive, fmt.Sprintf("%x", digest)
}

func TestUpdatePreflightAllowsKnownConfigRepair(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte("[defaults]\nreview = 'on_risk'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	code, output := runCLI(t, service, "_update-preflight")
	if code != 0 {
		t.Fatalf("update preflight blocked the release that can repair config: %d %s", code, output)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "[defaults]\nreview = 'on_risk'\n" {
		t.Fatalf("read-only preflight changed config: %q %v", data, err)
	}
}

func TestUpdateRollsBackBinaryWhenSetupFails(t *testing.T) {
	_, archiveName, archive, digest := buildReleaseArchive(t, "v0.15.1")
	var url string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v0.15.1", "assets": []any{map[string]string{"name": archiveName, "browser_download_url": url + "/archive"}, map[string]string{"name": "checksums.txt", "browser_download_url": url + "/checksums"}}})
		case "/archive":
			_, _ = w.Write(archive)
		case "/checksums":
			fmt.Fprintf(w, "%s  %s\n", digest, archiveName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	url = server.URL

	root := t.TempDir()
	home := filepath.Join(root, "posse")
	binaryPath := filepath.Join(root, "bin", "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o700); err != nil {
		t.Fatal(err)
	}
	oldBinary := []byte("installed binary before setup failure")
	if err := os.WriteFile(binaryPath, oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSetupManifest(filepath.Join(home, setupManifestName), setupManifest{Binary: binaryPath, Version: "0.15.0"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nreview = 'none'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	service.Version = "0.15.0"
	service.updateURL = url
	path := filepath.Join(root, "empty-path")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", path)
	code, output := runCLI(t, service, "update")
	if code != 1 || !strings.Contains(output, "setup_failed") || !strings.Contains(output, "previous binary was restored") {
		t.Fatalf("failed setup did not report rollback: code=%d output=%s", code, output)
	}
	if data, err := os.ReadFile(binaryPath); err != nil || !bytes.Equal(data, oldBinary) {
		t.Fatalf("setup failure did not restore the installed binary: %q %v", data, err)
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(binaryPath), ".posse-update-rollback-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("setup rollback left binary backups %v err=%v", leftovers, err)
	}
}

func TestUpdateKeepsNewBinaryWhenSetupFailsAfterWritingMigrations(t *testing.T) {
	newBinary, archiveName, archive, digest := buildReleaseArchive(t, "v0.15.1")
	var url string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v0.15.1", "assets": []any{map[string]string{"name": archiveName, "browser_download_url": url + "/archive"}, map[string]string{"name": "checksums.txt", "browser_download_url": url + "/checksums"}}})
		case "/archive":
			_, _ = w.Write(archive)
		case "/checksums":
			fmt.Fprintf(w, "%s  %s\n", digest, archiveName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	url = server.URL

	root := t.TempDir()
	home := filepath.Join(root, "posse")
	binaryPath := filepath.Join(root, "bin", "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o700); err != nil {
		t.Fatal(err)
	}
	oldBinary := []byte("installed binary before migrated setup failure")
	if err := os.WriteFile(binaryPath, oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSetupManifest(filepath.Join(home, setupManifestName), setupManifest{Binary: binaryPath, Version: "0.15.0"}); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `ALTER TABLE messages DROP COLUMN wait_for_idle`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `DELETE FROM goose_db_version WHERE version_id = 12`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	service := testService(home, nil)
	service.Version = "0.15.0"
	service.updateURL = url
	service.updateSetup = func(ctx context.Context, destination, setupHome string, force bool) ([]byte, error) {
		if destination != binaryPath || setupHome != home || force {
			t.Fatalf("setup arguments = destination %q home %q force %t", destination, setupHome, force)
		}
		db, err := store.Open(home)
		if err != nil {
			return []byte(err.Error()), err
		}
		if err := db.Close(); err != nil {
			return []byte(err.Error()), err
		}
		return []byte(`{"code":"setup_failed","message":"simulated failure after migration"}`), fmt.Errorf("simulated setup failure")
	}
	code, output := runCLI(t, service, "update")
	if code != 1 || !strings.Contains(output, "migration state changed") || !strings.Contains(output, "new binary remains installed") || !strings.Contains(output, "posse setup") {
		t.Fatalf("migration failure did not retain the new binary with repair guidance: code=%d output=%s", code, output)
	}
	if data, err := os.ReadFile(binaryPath); err != nil || !bytes.Equal(data, newBinary) {
		t.Fatalf("setup failure after migration restored the old binary: %v size=%d want=%d", err, len(data), len(newBinary))
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(binaryPath), ".posse-update-rollback-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("migration failure left old binary backups %v err=%v", leftovers, err)
	}
}

func TestUpdateInstallsRepairBinaryForLegacyReviewAndAllowsConfigRepair(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	target := filepath.Join(t.TempDir(), "posse-current")
	build := exec.Command("go", "build", "-o", target, "./cmd/posse")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build current release binary: %v\n%s", err, output)
	}
	binary, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var archiveBody bytes.Buffer
	gz := gzip.NewWriter(&archiveBody)
	tarball := tar.NewWriter(gz)
	archiveName := fmt.Sprintf("posse_0.15.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	if err := tarball.WriteHeader(&tar.Header{Name: "posse", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarball.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tarball.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive := archiveBody.Bytes()
	digest := sha256.Sum256(archive)
	var url string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v0.15.0", "body": "### Migration note\nChoose a supported review policy before updating.", "assets": []any{map[string]string{"name": archiveName, "browser_download_url": url + "/archive"}, map[string]string{"name": "checksums.txt", "browser_download_url": url + "/checksums"}}})
		case "/archive":
			_, _ = w.Write(archive)
		case "/checksums":
			fmt.Fprintf(w, "%x  %s\n", digest, archiveName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	url = server.URL

	for _, action := range []string{"set", "unset"} {
		t.Run(action, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "posse")
			binaryPath := filepath.Join(root, "bin", "posse")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(binaryPath), 0o700); err != nil {
				t.Fatal(err)
			}
			oldBinary := []byte("installed 0.14.0 binary")
			if err := os.WriteFile(binaryPath, oldBinary, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := writeSetupManifest(filepath.Join(home, setupManifestName), setupManifest{Binary: binaryPath, Version: "0.14.0"}); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(home, "config.toml")
			original := `# 0.14 User config
[defaults]
review = "on_risk"
landing_mode = "pr"
`
			if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			service := testService(home, nil)
			service.Version = "0.14.0"
			service.updateURL = url
			code, output := runCLI(t, service, "update")
			if code != 1 || !strings.Contains(output, "config_repair_required") || !strings.Contains(output, "setup was skipped") || !strings.Contains(output, "legacy review policy") || !strings.Contains(output, "posse config set defaults.review none") {
				t.Fatalf("update did not install the repair-capable binary and defer setup: code=%d output=%s", code, output)
			}
			if data, err := os.ReadFile(binaryPath); err != nil || !bytes.Equal(data, binary) {
				t.Fatalf("repair-capable binary was not installed: %v size=%d want=%d", err, len(data), len(binary))
			}
			if data, err := os.ReadFile(configPath); err != nil || string(data) != original {
				t.Fatalf("preflight changed the legacy config: %q %v", data, err)
			}
			if _, err := os.Stat(configPath + ".v014"); !os.IsNotExist(err) {
				t.Fatalf("preflight wrote a rollback config backup: %v", err)
			}
			args := []string{"config", action, "defaults.review"}
			if action == "set" {
				args = append(args, "none")
			}
			if code, output := runCLI(t, service, args...); code != 0 {
				t.Fatalf("config %s could not repair the rejected key: %d %s", action, code, output)
			}
			if code, output := runCLI(t, service, "config", "show"); code != 0 {
				t.Fatalf("repaired config did not load: %d %s", code, output)
			}
			backup, err := os.ReadFile(configPath + ".v014")
			if err != nil || string(backup) != original {
				t.Fatalf("migration backup did not preserve the original bytes: %q %v; want %q", backup, err, original)
			}
		})
	}
}

func TestUpdateAlreadyCurrentPointsToSetup(t *testing.T) {
	server, url := fakeRelease(t, "bad")
	defer server.Close()
	service := testService(t.TempDir(), nil)
	service.Version = "0.2.0"
	service.updateURL = url
	code, output := runCLI(t, service, "update")
	if code != 1 || !strings.Contains(output, "already_current") || !strings.Contains(output, "posse setup") {
		t.Fatalf("already-current update omitted setup recovery: code=%d output=%s", code, output)
	}
}

func TestUpdateCheckReportsRunningWatches(t *testing.T) {
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "demo", home, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:demo:lead"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lead := startUpdateWatchProcess(t, home, "watch")
	watchTab := startUpdateWatchProcess(t, home, "watch", "--poll-only")

	server, url := fakeRelease(t, "bad")
	defer server.Close()
	service := testService(home, nil)
	service.Version = "0.1.0"
	service.updateURL = url
	code, output := runCLI(t, service, "update", "--check", "--json")
	if code != 0 || !strings.Contains(output, `"watches"`) || !strings.Contains(output, `"project":"demo"`) || !strings.Contains(output, `"kind":"lead"`) || !strings.Contains(output, `"kind":"watch"`) || !strings.Contains(output, fmt.Sprintf(`"pid":%d`, lead.command.Process.Pid)) || !strings.Contains(output, fmt.Sprintf(`"pid":%d`, watchTab.command.Process.Pid)) {
		t.Fatalf("update --check omitted running Lead/Watch processes: %d %s", code, output)
	}
	for _, process := range []updateWatchHelper{lead, watchTab} {
		if err := process.command.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("--check stopped watch PID %d: %v", process.command.Process.Pid, err)
		}
	}
}

type updateWatchHelper struct {
	command *exec.Cmd
	stopped string
}

func startUpdateWatchProcess(t *testing.T, home string, args ...string) updateWatchHelper {
	t.Helper()
	previousVerifier := watchExecutableVerifier
	watchExecutableVerifier = func(string) bool { return true }
	t.Cleanup(func() { watchExecutableVerifier = previousVerifier })
	directory := t.TempDir()
	binary := filepath.Join(directory, "posse")
	source := filepath.Join(directory, "helper.go")
	ready := filepath.Join(directory, "ready")
	stop := filepath.Join(directory, "stop")
	program := `package main
import (
	"os"
	"os/signal"
	"syscall"
)
func main() {
	_ = os.WriteFile(os.Getenv("POSSE_TEST_READY_FILE"), []byte("ready"), 0600)
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, syscall.SIGTERM)
	<-stopped
	_ = os.WriteFile(os.Getenv("POSSE_TEST_STOP_FILE"), []byte("stopped"), 0600)
}`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", binary, source)
	build.Dir = directory
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build native Watch test process: %v\n%s", err, output)
	}
	command := exec.Command(binary, args...)
	command.Dir = home
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found && (strings.HasPrefix(key, "HERDR_") || key == "POSSE_HOME") {
			continue
		}
		command.Env = append(command.Env, entry)
	}
	command.Env = append(command.Env, "POSSE_HOME="+home, "PWD="+home, "POSSE_TEST_READY_FILE="+ready, "POSSE_TEST_STOP_FILE="+stop)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Signal(syscall.SIGTERM)
			_ = command.Wait()
		}
	})
	if !waitForFile(2*time.Second, ready) {
		t.Fatalf("watch helper PID %d did not start", command.Process.Pid)
	}
	return updateWatchHelper{command: command, stopped: stop}
}

func waitForFile(timeout time.Duration, path string) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestUpdateAllowsRunningWatchesToReexec(t *testing.T) {
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProject(context.Background(), "demo", home, "main"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	startUpdateWatchProcess(t, home, "watch")
	startUpdateWatchProcess(t, home, "watch", "--poll-only")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.2.0"})
	}))
	defer server.Close()
	service := testService(home, nil)
	userShellUpdatePane(t, service)
	service.Version = "0.1.0"
	service.updateURL = server.URL
	code, output := runCLI(t, service, "update")
	if code != 1 || strings.Contains(output, "watches_running") || !strings.Contains(output, "release_assets_missing") {
		t.Fatalf("update did not proceed to release verification with live watches: %d %s", code, output)
	}
	if requests == 0 {
		t.Fatal("update refused before fetching the release while watches were running")
	}
}

func TestRequestWatchReexecSkipsAnAlreadyUpdatedProcess(t *testing.T) {
	home := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSetupManifest(filepath.Join(home, setupManifestName), setupManifest{Binary: binary}); err != nil {
		t.Fatal(err)
	}
	previousVerifier := watchExecutableVerifier
	watchExecutableVerifier = func(string) bool { return true }
	t.Cleanup(func() { watchExecutableVerifier = previousVerifier })
	current, ok := inspectWatchExecutable(os.Getpid())
	if !ok {
		t.Fatal("test process executable identity is unavailable")
	}
	process := watchProcess{PID: os.Getpid(), Executable: current}
	if err := requestWatchReexec(home, []watchProcess{process}); err != nil {
		t.Fatalf("already updated process produced a re-exec warning: %v", err)
	}
	if _, err := os.Stat(watchReexecMarker(home, os.Getpid())); !os.IsNotExist(err) {
		t.Fatalf("already updated process got another re-exec marker: %v", err)
	}
}

func TestRequestWatchReexecSkipsUpdatedProcessesAndMarksRemainingOnes(t *testing.T) {
	home := t.TempDir()
	old := startUpdateWatchProcess(t, home, "watch")
	installed := filepath.Join(t.TempDir(), "posse")
	binary, err := os.ReadFile(old.command.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, binary, 0o700); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	stopped := filepath.Join(t.TempDir(), "stopped")
	current := exec.Command(installed, "watch")
	current.Dir = home
	current.Env = append(os.Environ(), "POSSE_HOME="+home, "PWD="+home, "POSSE_TEST_READY_FILE="+ready, "POSSE_TEST_STOP_FILE="+stopped)
	if err := current.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if current.Process != nil {
			_ = current.Process.Signal(syscall.SIGTERM)
			_ = current.Wait()
		}
	})
	if !waitForFile(2*time.Second, ready) {
		t.Fatalf("updated Watch helper PID %d did not start", current.Process.Pid)
	}
	if err := writeSetupManifest(filepath.Join(home, setupManifestName), setupManifest{Binary: installed}); err != nil {
		t.Fatal(err)
	}
	previousVerifier := watchExecutableVerifier
	watchExecutableVerifier = func(string) bool { return true }
	t.Cleanup(func() { watchExecutableVerifier = previousVerifier })
	processes, err := systemWatchProcesses(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(processes) != 2 {
		t.Fatalf("expected old and updated helper processes, found %#v", processes)
	}
	if err := requestWatchReexec(home, processes); err != nil {
		t.Fatalf("an already updated Watch caused a warning: %v", err)
	}
	if _, err := os.Stat(watchReexecMarker(home, current.Process.Pid)); !os.IsNotExist(err) {
		t.Fatalf("updated process was requested to re-exec again: %v", err)
	}
	if _, err := os.Stat(watchReexecMarker(home, old.command.Process.Pid)); err != nil {
		t.Fatalf("remaining old process did not receive a re-exec marker: %v", err)
	}
}

func TestRequestWatchReexecWritesAProcessScopedMarker(t *testing.T) {
	home := t.TempDir()
	watch := startUpdateWatchProcess(t, home, "watch", "--poll-only")
	processes, err := systemWatchProcesses(home)
	if err != nil {
		t.Fatal(err)
	}
	var found *watchProcess
	for index := range processes {
		if processes[index].PID == watch.command.Process.Pid {
			found = &processes[index]
			break
		}
	}
	if found == nil {
		t.Fatalf("watch process %d was not discovered: %#v", watch.command.Process.Pid, processes)
	}
	if err := requestWatchReexec(home, []watchProcess{*found}); err != nil {
		t.Fatal(err)
	}
	marker := watchReexecMarker(home, watch.command.Process.Pid)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("update did not request watch re-exec: %v", err)
	}
}

func TestUpdateCheckReadsReleaseWithoutInstalling(t *testing.T) {
	server, url := fakeRelease(t, "bad")
	defer server.Close()
	home := t.TempDir()
	service := testService(home, nil)
	service.Version = "0.1.0"
	service.updateURL = url
	code, output := runCLI(t, service, "update", "--check")
	if code != 0 || !strings.Contains(output, "v0.2.0") || !strings.Contains(output, "0.1.0") || !strings.Contains(output, "repair") {
		t.Fatalf("update --check: %d %s", code, output)
	}
	if _, err := os.Stat(filepath.Join(home, "posse.db")); !os.IsNotExist(err) {
		t.Fatalf("check created database: %v", err)
	}
}

func TestUpdateNeverInstallsInsideLeadTurn(t *testing.T) {
	server, url := fakeRelease(t, strings.Repeat("0", 64))
	defer server.Close()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "demo", home, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p2", "posse:demo:lead"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	userShellUpdatePane(t, service)
	service.Version = "0.1.0"
	service.updateURL = url
	code, output := runCLI(t, service, "update")
	if code != 1 || !strings.Contains(output, "update_from_turn") {
		t.Fatalf("update in Lead turn: %d %s", code, output)
	}
}

func TestUpdateFromUserShellPaneReachesReleaseVerification(t *testing.T) {
	server, url := fakeRelease(t, strings.Repeat("0", 64))
	defer server.Close()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	userShellUpdatePane(t, service)
	service.Version = "0.1.0"
	service.updateURL = url
	code, output := runCLI(t, service, "update")
	if code != 1 || !strings.Contains(output, "checksum_mismatch") {
		t.Fatalf("User shell pane update: %d %s", code, output)
	}
}

func TestUpdateRejectsAgentAndUnverifiedHerdrPanes(t *testing.T) {
	server, url := fakeRelease(t, strings.Repeat("0", 64))
	defer server.Close()
	for _, testCase := range []struct {
		name string
		code string
		set  func(*testing.T, *Service)
	}{
		{"agent pane", "update_from_turn", func(_ *testing.T, service *Service) {
			service.Herdr.(*herdr.Fake).SnapshotValue.Panes[0].Agent = "codex"
		}},
		{"stale pane", "update_pane_unknown", func(_ *testing.T, service *Service) {
			service.Herdr.(*herdr.Fake).SnapshotValue.Panes = nil
		}},
		{"missing pane ID", "update_pane_unknown", func(t *testing.T, _ *Service) {
			t.Setenv("HERDR_PANE_ID", "")
		}},
		{"Herdr unavailable", "update_pane_unknown", func(_ *testing.T, service *Service) {
			service.Herdr.(*herdr.Fake).Errors["session.snapshot"] = fmt.Errorf("server unavailable")
		}},
		{"inherited another pane", "update_pane_unknown", func(t *testing.T, _ *Service) {
			procRoot = fakeProc(t, "/", []string{"HERDR_PANE_ID=w1:p1"})
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service := testService(t.TempDir(), nil)
			userShellUpdatePane(t, service)
			service.Version = "0.1.0"
			service.updateURL = url
			testCase.set(t, service)
			code, output := runCLI(t, service, "update")
			if code != 1 || !strings.Contains(output, testCase.code) {
				t.Fatalf("update from %s: %d %s", testCase.name, code, output)
			}
		})
	}
}

func TestUpdateNeverInstallsInsideWorkerTurn(t *testing.T) {
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "demo", home, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 1, Type: "research", Title: "Check update", State: store.StateSpawning, MergeMode: "local", PaneID: "w1:p2"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	userShellUpdatePane(t, service)
	code, output := runCLI(t, service, "update")
	if code != 1 || !strings.Contains(output, "worker_forbidden") {
		t.Fatalf("update from Worker pane: %d %s", code, output)
	}
}

func TestUpdateCheckWorksInAgentPane(t *testing.T) {
	server, url := fakeRelease(t, "bad")
	defer server.Close()
	service := testService(t.TempDir(), nil)
	userShellUpdatePane(t, service)
	service.Herdr.(*herdr.Fake).SnapshotValue.Panes[0].Agent = "codex"
	service.Version = "0.1.0"
	service.updateURL = url
	code, output := runCLI(t, service, "update", "--check")
	if code != 0 || !strings.Contains(output, "v0.2.0") {
		t.Fatalf("update --check from agent pane: %d %s", code, output)
	}
}

func userShellUpdatePane(t *testing.T, service *Service) {
	t.Helper()
	previousProcRoot := procRoot
	procRoot = t.TempDir()
	t.Cleanup(func() { procRoot = previousProcRoot })
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p2")
	fake := herdr.NewFake()
	// Herdr reports agent_status "unknown" for a plain shell pane.
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p2", WorkspaceID: "w1", AgentStatus: "unknown"}}}
	service.Herdr = fake
}

func TestUpdateNoticeOncePerReleaseAndDailyCache(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.2.0"})
	}))
	defer server.Close()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(context.Background(), "demo", home, "main")
	if err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	service.Version = "0.1.0"
	service.updateURL = server.URL
	if err := writeUpdateCache(home, updateCache{Checked: time.Now(), Release: githubRelease{Tag: "v0.2.0"}}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, ok := service.availableUpdate(context.Background(), db, &project); !ok {
			t.Fatal("missing update")
		}
	}
	notices, err := db.Notices(context.Background(), project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "update_available" || calls != 0 {
		t.Fatalf("notices=%v calls=%d err=%v", notices, calls, err)
	}
}

func TestAvailableUpdateDoesNotFetchWithColdCache(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.2.0"})
	}))
	defer server.Close()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(context.Background(), "demo", home, "main")
	if err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	service.Version = "0.1.0"
	service.updateURL = server.URL
	if _, ok := service.availableUpdate(context.Background(), db, &project); ok {
		t.Fatal("cold cache advertised an update")
	}
	if calls != 0 {
		t.Fatalf("cold cache fetched a release: %d request(s)", calls)
	}
}

func TestUpdateCheckPersistsLatestReleaseForFutureUp(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.2.0"})
	}))
	defer server.Close()
	service := testService(t.TempDir(), nil)
	service.Version = "0.1.0"
	service.updateURL = server.URL
	if code, output := runCLI(t, service, "update", "--check"); code != 0 {
		t.Fatalf("update --check: %d %s", code, output)
	}
	if release, ok := service.readCachedRelease(); !ok || release.Tag != "v0.2.0" {
		t.Fatalf("latest release was not cached: %#v %t", release, ok)
	}
	ctx := &axi.Context{Context: context.Background(), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}, JSON: true}
	known, err := service.offerUpUpdate(ctx, nil)
	if err != nil || !known || calls != 1 {
		t.Fatalf("future up cache lookup: known=%t calls=%d err=%v", known, calls, err)
	}
}

func TestUpdateRefusesChecksumMismatchBeforeBackupOrInstall(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("fixture archive is linux/amd64")
	}
	server, url := fakeRelease(t, strings.Repeat("0", 64))
	defer server.Close()
	home := t.TempDir()
	service := testService(home, nil)
	service.Version = "0.1.0"
	service.updateURL = url
	code, output := runCLI(t, service, "update")
	if code != 1 || !strings.Contains(output, "checksum_mismatch") {
		t.Fatalf("update mismatch: %d %s", code, output)
	}
	if _, err := os.Stat(filepath.Join(home, "backup")); !os.IsNotExist(err) {
		t.Fatalf("mismatch created backup: %v", err)
	}
}

func TestUpdateFromUserShellPaneBacksUpAndAtomicallyInstallsVerifiedArchive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fixture uses a shell executable")
	}
	home := t.TempDir()
	binaryPath := filepath.Join(home, "bin", "posse")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("old executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSetupManifest(filepath.Join(home, setupManifestName), setupManifest{Binary: binaryPath, Version: "0.1.0"}); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "demo", home, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:demo:lead"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lead := startUpdateWatchProcess(t, home, "watch")
	watchTab := startUpdateWatchProcess(t, home, "watch", "--poll-only")
	script := []byte("#!/bin/sh\ncase \"$1\" in _update-preflight) exit 0;; setup) printf installed > \"$POSSE_HOME/setup-called\";; watch) printf v0.2.0 > \"$POSSE_HOME/new-binary-watch\";; *) exit 1;; esac\n")
	var contents bytes.Buffer
	gz := gzip.NewWriter(&contents)
	tarball := tar.NewWriter(gz)
	if err := tarball.WriteHeader(&tar.Header{Name: "posse", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarball.Write(script); err != nil {
		t.Fatal(err)
	}
	if err := tarball.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive := contents.Bytes()
	digest := sha256.Sum256(archive)
	name := fmt.Sprintf("posse_0.2.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	var url string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v0.2.0", "assets": []any{map[string]string{"name": name, "browser_download_url": url + "/archive"}, map[string]string{"name": "checksums.txt", "browser_download_url": url + "/checksums"}}})
		case "/archive":
			_, _ = w.Write(archive)
		case "/checksums":
			fmt.Fprintf(w, "%x  %s\n", digest, name)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	url = server.URL
	service := testService(home, nil)
	userShellUpdatePane(t, service)
	service.Version = "0.1.0"
	service.updateURL = url
	code, output := runCLI(t, service, "update", "--stop-watches")
	if code != 0 {
		t.Fatalf("update --stop-watches: %d %s", code, output)
	}
	if !strings.Contains(output, "stopped_watches") || !strings.Contains(output, "watch") || !strings.Contains(output, "lead") || !strings.Contains(output, "Project watch re-execs") || !strings.Contains(output, "Lead restarts") {
		t.Fatalf("update omitted stopped processes or restart owners: %s", output)
	}
	leadStopped := waitForFile(2*time.Second, lead.stopped)
	tabStopped := waitForFile(2*time.Second, watchTab.stopped)
	if !leadStopped || !tabStopped {
		t.Fatalf("update did not stop both watches: lead=%t tab=%t", leadStopped, tabStopped)
	}
	got, err := os.ReadFile(binaryPath)
	if err != nil || !bytes.Equal(got, script) {
		t.Fatalf("installed binary: %v %s", err, got)
	}
	if _, err := os.Stat(filepath.Join(home, "backup", "posse-pre-v0.2.0.db")); err != nil {
		t.Fatalf("missing backup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "setup-called")); err != nil {
		t.Fatalf("setup did not run: %v", err)
	}
	restart := exec.Command(binaryPath, "watch", "--poll-only")
	restart.Env = []string{"POSSE_HOME=" + home}
	if output, err := restart.CombinedOutput(); err != nil {
		t.Fatalf("new binary did not restart its watch: %v %s", err, output)
	}
	if data, err := os.ReadFile(filepath.Join(home, "new-binary-watch")); err != nil || string(data) != "v0.2.0" {
		t.Fatalf("watch did not run from the installed binary: %q %v", data, err)
	}
}

func TestReadOnlyDatabaseCanVacuumIntoPreUpdateBackup(t *testing.T) {
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = store.OpenReadOnly(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backup := filepath.Join(home, "pre.db")
	if _, err := db.ExecContext(context.Background(), "VACUUM INTO '"+backup+"'"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatal(err)
	}
}

func TestIdleMigrationRefusesLiveTasksUnlessForced(t *testing.T) {
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "test", home, "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "active", MergeMode: "pr"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	err = db.RefuseLiveMigration(ctx, []int64{42}, false)
	if err == nil || !strings.Contains(err.Error(), "live Tasks") {
		t.Fatalf("migration was not refused: %v", err)
	}
	if err := db.RefuseLiveMigration(ctx, []int64{42}, true); err != nil {
		t.Fatalf("force refused: %v", err)
	}
}
