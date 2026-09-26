package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func upReleaseFixture(t *testing.T, script string) (home, binary, url string, closeServer func(), requests *int) {
	t.Helper()
	home = t.TempDir()
	binary = filepath.Join(home, "bin", "posse")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("old executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSetupManifest(filepath.Join(home, setupManifestName), setupManifest{Binary: binary, Version: "0.1.0"}); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	tarball := tar.NewWriter(gz)
	if err := tarball.WriteHeader(&tar.Header{Name: "posse", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(script))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tarball, script); err != nil {
		t.Fatal(err)
	}
	if err := tarball.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive := body.Bytes()
	sum := sha256.Sum256(archive)
	name := fmt.Sprintf("posse_0.2.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	n := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v0.2.0", "body": "### Bug Fixes\n* repaired startup", "assets": []any{map[string]string{"name": name, "browser_download_url": server.URL + "/archive"}, map[string]string{"name": "checksums.txt", "browser_download_url": server.URL + "/checksums"}}})
		case "/archive":
			n++
			_, _ = w.Write(archive)
		case "/checksums":
			fmt.Fprintf(w, "%x  %s\n", sum, name)
		default:
			http.NotFound(w, r)
		}
	}))
	return home, binary, server.URL, server.Close, &n
}

func TestUpOffersUpdateBeforeRegistrationThenReexecsExactArguments(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses a shell fixture")
	}
	home, binary, url, closeServer, requests := upReleaseFixture(t, "#!/bin/sh\ncase \"$1\" in _update-preflight) exit 0;; setup) exit 0;; *) exit 1;; esac\n")
	defer closeServer()
	service := testService(home, nil)
	service.Version = "0.1.0"
	service.updateURL = url
	userShellUpdatePane(t, service)
	prompted := 0
	service.updateConfirm = func(_ io.Writer, question string) (bool, bool, error) {
		prompted++
		if !strings.Contains(question, "v0.2.0") || !strings.Contains(question, "repaired startup") {
			t.Fatalf("prompt: %s", question)
		}
		return true, true, nil
	}
	initialCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{binary, "up", "--yes", "--name", "demo"}
	sentinel := errors.New("intercepted exec")
	service.reexecUpdate = func(path string, args, env []string) error {
		if path != binary || !reflect.DeepEqual(args, wantArgs) {
			t.Fatalf("exec: %s %q", path, args)
		}
		if dir, _ := os.Getwd(); dir != initialCWD {
			t.Fatalf("cwd changed: %s", dir)
		}
		if !reflect.DeepEqual(env, os.Environ()) {
			t.Fatal("environment changed")
		}
		if _, err := os.Stat(filepath.Join(home, "posse.db")); !os.IsNotExist(err) {
			t.Fatalf("registered before update: %v", err)
		}
		return sentinel
	}
	code, output := runCLI(t, service, "up", "--yes", "--name", "demo")
	if code != 1 || !strings.Contains(output, "intercepted exec") || prompted != 1 || *requests != 1 {
		t.Fatalf("up: code=%d prompt=%d downloads=%d output=%s", code, prompted, *requests, output)
	}
	if installed, err := os.ReadFile(binary); err != nil || !strings.HasPrefix(string(installed), "#!/bin/sh") {
		t.Fatalf("binary not replaced: %v %s", err, installed)
	}
}

func TestUpDeclineAndNonInteractiveDoNotInstallEvenWithYes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		interactive bool
	}{{"decline", true}, {"not interactive", false}} {
		t.Run(tc.name, func(t *testing.T) {
			home, binary, url, closeServer, requests := upReleaseFixture(t, "#!/bin/sh\nexit 0\n")
			defer closeServer()
			service := testService(home, nil)
			service.Version = "0.1.0"
			service.updateURL = url
			userShellUpdatePane(t, service)
			service.updateConfirm = func(io.Writer, string) (bool, bool, error) { return false, tc.interactive, nil }
			code, output := runCLI(t, service, "up", "--yes")
			if code == 0 || !strings.Contains(output, "posse update") || *requests != 0 {
				t.Fatalf("up: %d requests=%d %s", code, *requests, output)
			}
			data, err := os.ReadFile(binary)
			if err != nil || string(data) != "old executable" {
				t.Fatalf("installed without consent: %v %s", err, data)
			}
		})
	}
}

func TestUpDoesNotOfferUpdateInsideExistingLeadTurn(t *testing.T) {
	home, binary, url, closeServer, requests := upReleaseFixture(t, "#!/bin/sh\nexit 0\n")
	defer closeServer()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "demo", home, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "workspace", "lead-pane", "posse:demo:lead"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	service := testService(home, nil)
	service.Version = "0.1.0"
	service.updateURL = url
	userShellUpdatePane(t, service)
	t.Setenv("HERDR_PANE_ID", "lead-pane")
	service.updateConfirm = func(io.Writer, string) (bool, bool, error) {
		t.Fatal("prompted from Lead turn")
		return false, false, nil
	}
	_, _ = runCLI(t, service, "up", "--yes")
	if *requests != 0 {
		t.Fatalf("downloaded from Lead turn: %d", *requests)
	}
	if data, err := os.ReadFile(binary); err != nil || string(data) != "old executable" {
		t.Fatalf("installed from Lead turn: %v %s", err, data)
	}
}

func TestUpUpdateMigrationRefusalDoesNotRegisterOrReplaceBinary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses a shell fixture")
	}
	home, binary, url, closeServer, _ := upReleaseFixture(t, "#!/bin/sh\necho 'migrations require no live Tasks' >&2\nexit 7\n")
	defer closeServer()
	service := testService(home, nil)
	service.Version = "0.1.0"
	service.updateURL = url
	userShellUpdatePane(t, service)
	service.updateConfirm = func(io.Writer, string) (bool, bool, error) { return true, true, nil }
	code, output := runCLI(t, service, "up")
	if code != 1 || !strings.Contains(output, "migration_refused") || !strings.Contains(output, "live Tasks") {
		t.Fatalf("up: %d %s", code, output)
	}
	if data, err := os.ReadFile(binary); err != nil || string(data) != "old executable" {
		t.Fatalf("replaced binary: %v %s", err, data)
	}
	if _, err := os.Stat(filepath.Join(home, "posse.db")); !os.IsNotExist(err) {
		t.Fatalf("registered before migration check: %v", err)
	}
}
