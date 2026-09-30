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

func TestUpdateCheckReportsRunningLookouts(t *testing.T) {
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
	lead := startUpdateLookoutProcess(t, home, "lookout")
	lookoutTab := startUpdateLookoutProcess(t, home, "lookout", "--poll-only")

	server, url := fakeRelease(t, "bad")
	defer server.Close()
	service := testService(home, nil)
	service.Version = "0.1.0"
	service.updateURL = url
	code, output := runCLI(t, service, "update", "--check", "--json")
	if code != 0 || !strings.Contains(output, `"lookouts"`) || !strings.Contains(output, `"project":"demo"`) || !strings.Contains(output, `"kind":"lead"`) || !strings.Contains(output, `"kind":"lookout_tab"`) || !strings.Contains(output, fmt.Sprintf(`"pid":%d`, lead.command.Process.Pid)) || !strings.Contains(output, fmt.Sprintf(`"pid":%d`, lookoutTab.command.Process.Pid)) {
		t.Fatalf("update --check omitted running Lead/Lookout processes: %d %s", code, output)
	}
	for _, process := range []updateLookoutHelper{lead, lookoutTab} {
		if err := process.command.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("--check stopped lookout PID %d: %v", process.command.Process.Pid, err)
		}
	}
}

type updateLookoutHelper struct {
	command *exec.Cmd
	stopped string
}

func startUpdateLookoutProcess(t *testing.T, home string, args ...string) updateLookoutHelper {
	t.Helper()
	previousVerifier := lookoutExecutableVerifier
	lookoutExecutableVerifier = func(string) bool { return true }
	t.Cleanup(func() { lookoutExecutableVerifier = previousVerifier })
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
		t.Fatalf("build native Lookout test process: %v\n%s", err, output)
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
		t.Fatalf("lookout helper PID %d did not start", command.Process.Pid)
	}
	return updateLookoutHelper{command: command, stopped: stop}
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

func TestUpdateRefusesRunningLookoutsUnlessRequested(t *testing.T) {
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
	startUpdateLookoutProcess(t, home, "lookout")
	startUpdateLookoutProcess(t, home, "lookout", "--poll-only")
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
	if code != 1 || !strings.Contains(output, "lookouts_running") || !strings.Contains(output, "demo") || !strings.Contains(output, "lookout_tab") || !strings.Contains(output, "lead") {
		t.Fatalf("update did not refuse with the running process list: %d %s", code, output)
	}
	if requests != 0 {
		t.Fatalf("update fetched a release before refusing: %d request(s)", requests)
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

func TestUpdateNeverInstallsInsideRiderTurn(t *testing.T) {
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "demo", home, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 1, Type: "scout", Title: "Check update", State: store.StateSpawning, LandingMode: "local", PaneID: "w1:p2"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	userShellUpdatePane(t, service)
	code, output := runCLI(t, service, "update")
	if code != 1 || !strings.Contains(output, "worker_forbidden") {
		t.Fatalf("update from Rider pane: %d %s", code, output)
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
	for range 3 {
		if _, ok := service.availableUpdate(context.Background(), db, &project); !ok {
			t.Fatal("missing update")
		}
	}
	notices, err := db.Notices(context.Background(), project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "update_available" || calls != 1 {
		t.Fatalf("notices=%v calls=%d err=%v", notices, calls, err)
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
	lead := startUpdateLookoutProcess(t, home, "lookout")
	lookoutTab := startUpdateLookoutProcess(t, home, "lookout", "--poll-only")
	script := []byte("#!/bin/sh\ncase \"$1\" in _update-preflight) exit 0;; setup) printf installed > \"$POSSE_HOME/setup-called\";; lookout) printf v0.2.0 > \"$POSSE_HOME/new-binary-lookout\";; *) exit 1;; esac\n")
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
	code, output := runCLI(t, service, "update", "--stop-lookouts")
	if code != 0 {
		t.Fatalf("update --stop-lookouts: %d %s", code, output)
	}
	if !strings.Contains(output, "stopped_lookouts") || !strings.Contains(output, "lookout_tab") || !strings.Contains(output, "lead") || !strings.Contains(output, "Lookout tab owner") || !strings.Contains(output, "Lead restarts") {
		t.Fatalf("update omitted stopped processes or restart owners: %s", output)
	}
	leadStopped := waitForFile(2*time.Second, lead.stopped)
	tabStopped := waitForFile(2*time.Second, lookoutTab.stopped)
	if !leadStopped || !tabStopped {
		t.Fatalf("update did not stop both lookouts: lead=%t tab=%t", leadStopped, tabStopped)
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
	restart := exec.Command(binaryPath, "lookout", "--poll-only")
	restart.Env = []string{"POSSE_HOME=" + home}
	if output, err := restart.CombinedOutput(); err != nil {
		t.Fatalf("new binary did not restart its lookout: %v %s", err, output)
	}
	if data, err := os.ReadFile(filepath.Join(home, "new-binary-lookout")); err != nil || string(data) != "v0.2.0" {
		t.Fatalf("lookout did not run from the installed binary: %q %v", data, err)
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
	id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "active", LandingMode: "pr"})
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
