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
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
	service := testService(t.TempDir(), nil)
	service.Version = "0.1.0"
	service.updateURL = url
	service.herdrContext = func() bool { return true }
	code, output := runCLI(t, service, "update")
	if code != 1 || !strings.Contains(output, "update_from_turn") {
		t.Fatalf("update in Lead turn: %d %s", code, output)
	}
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

func TestUpdateBacksUpAndAtomicallyInstallsVerifiedArchive(t *testing.T) {
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
	db.Close()
	script := []byte("#!/bin/sh\ncase \"$1\" in _update-preflight) exit 0;; setup) printf installed > \"$POSSE_HOME/setup-called\";; *) exit 1;; esac\n")
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
	service.Version = "0.1.0"
	service.updateURL = url
	code, output := runCLI(t, service, "update")
	if code != 0 {
		t.Fatalf("update: %d %s", code, output)
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
