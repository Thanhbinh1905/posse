//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOfficial014HomeCanBeRepairedAfter015Update(t *testing.T) {
	if testProcessHasHerdrAncestor(os.Getpid()) {
		t.Skip("release update fixture requires a process tree outside an active Herdr session")
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("official v0.14.0 release fixture is Linux amd64")
	}
	f := newPRLifecycleFixtureWithManualWatchOwner(t)
	defer f.db.Close()

	stableBinaryDir := newFixtureRootAt(t, "/var/tmp", fixturePrefix("official-update-installed-"))
	legacyBinary := downloadOfficialReleaseBinary(t, stableBinaryDir, "v0.14.0")
	home := filepath.Join(f.root, "legacy-release-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	env := setEnv(f.env, "POSSE_HOME", home)

	if code, output := runReleaseCLI(t, legacyBinary, f.repo, env, "config", "set", "defaults.review", "on_risk"); code != 0 {
		t.Fatalf("v0.14.0 could not create the legacy review setting: %d %s", code, output)
	}
	if code, output := runReleaseCLI(t, legacyBinary, f.repo, env, "config", "set", "defaults.landing_mode", "pr"); code != 0 {
		t.Fatalf("v0.14.0 could not create its legacy vocabulary setting: %d %s", code, output)
	}
	configPath := filepath.Join(home, "config.toml")
	original, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	officialRelease := downloadOfficialReleaseAsset(t, "v0.15.0")
	officialProxyURL, officialCertPath := startUpdateReleaseProxy(t, officialRelease)
	officialUpdateEnv := setEnv(f.callerEnv, "HTTPS_PROXY", officialProxyURL)
	officialUpdateEnv = setEnv(officialUpdateEnv, "https_proxy", officialProxyURL)
	officialUpdateEnv = setEnv(officialUpdateEnv, "NO_PROXY", "localhost,127.0.0.1,::1")
	officialUpdateEnv = setEnv(officialUpdateEnv, "no_proxy", "localhost,127.0.0.1,::1")
	officialUpdateEnv = setEnv(officialUpdateEnv, "SSL_CERT_FILE", officialCertPath)
	updateCode, updateOutput := runReleaseCLI(t, legacyBinary, f.repo, officialUpdateEnv, "update", "--version", "v0.15.0")
	if updateCode != 1 || !strings.Contains(updateOutput, "setup_failed") || !strings.Contains(updateOutput, "installed v0.15.0") || !strings.Contains(updateOutput, "legacy review policy") {
		t.Fatalf("official v0.14.0 update did not reproduce the 0.15.0 setup failure: code=%d output=%s", updateCode, updateOutput)
	}
	if code, output := runReleaseCLI(t, legacyBinary, f.repo, env, "--version"); code != 0 || !strings.Contains(output, "0.15.0") {
		t.Fatalf("official v0.15.0 binary was not installed: code=%d output=%s", code, output)
	}
	for _, args := range [][]string{
		{"config", "set", "defaults.review", "none"},
		{"config", "unset", "defaults.review"},
	} {
		if code, output := runReleaseCLI(t, legacyBinary, f.repo, env, args...); code == 0 || !strings.Contains(output, "config_invalid") {
			t.Fatalf("official v0.15.0 did not reproduce the config repair deadlock for %v: code=%d output=%s", args, code, output)
		}
	}
	if data, err := os.ReadFile(configPath); err != nil || !bytes.Equal(data, original) {
		t.Fatalf("failed v0.15.0 repair attempts changed config: %q err=%v", data, err)
	}

	// Use a local TLS proxy for the second release so the real v0.15 updater installs this Task's binary.
	currentBinary, err := os.ReadFile(f.binary)
	if err != nil {
		t.Fatal(err)
	}
	currentRelease := makeUpdateTestRelease(t, f.binary, "v0.15.1")
	proxyURL, certPath := startUpdateReleaseProxy(t, currentRelease)
	updateEnv := setEnv(f.callerEnv, "HTTPS_PROXY", proxyURL)
	updateEnv = setEnv(updateEnv, "https_proxy", proxyURL)
	updateEnv = setEnv(updateEnv, "NO_PROXY", "localhost,127.0.0.1,::1")
	updateEnv = setEnv(updateEnv, "no_proxy", "localhost,127.0.0.1,::1")
	updateEnv = setEnv(updateEnv, "SSL_CERT_FILE", certPath)
	updateCode, updateOutput = runReleaseCLI(t, legacyBinary, f.repo, updateEnv, "update", "--version", currentRelease.tag)
	if updateCode != 1 || !strings.Contains(updateOutput, "setup_failed") || !strings.Contains(updateOutput, "installed "+currentRelease.tag) || !strings.Contains(updateOutput, "posse config set defaults.review none") {
		t.Fatalf("official v0.15.0 update did not install the repair-capable binary: code=%d output=%s", updateCode, updateOutput)
	}
	installedBinary, err := os.ReadFile(legacyBinary)
	if err != nil || !bytes.Equal(installedBinary, currentBinary) {
		t.Fatalf("repair-capable binary was not installed: bytes=%d err=%v", len(installedBinary), err)
	}
	if code, output := runReleaseCLI(t, legacyBinary, f.repo, env, "config", "set", "defaults.review", "none"); code != 0 {
		t.Fatalf("current binary could not repair the v0.14 home: code=%d output=%s", code, output)
	}
	if code, output := runReleaseCLI(t, legacyBinary, f.repo, env, "config", "show"); code != 0 {
		t.Fatalf("repaired config did not load: code=%d output=%s", code, output)
	}
	backup, err := os.ReadFile(configPath + ".v014")
	if err != nil || !bytes.Equal(backup, original) {
		t.Fatalf("migration backup did not preserve the v0.14 bytes: %q err=%v; want %q", backup, err, original)
	}
	setupBinDir := filepath.Join(f.root, "legacy-setup-bin")
	if err := os.MkdirAll(setupBinDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(setupBinDir, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(herdrBinary, filepath.Join(setupBinDir, "herdr")); err != nil {
		t.Fatal(err)
	}
	setupEnv := setEnv(env, "PATH", setupBinDir+string(os.PathListSeparator)+"/usr/bin:/bin")
	if code, output := runReleaseCLI(t, legacyBinary, f.repo, setupEnv, "setup", "--binary", legacyBinary); code != 0 || !strings.Contains(output, "manifest:") {
		t.Fatalf("setup did not complete after command-line repair: code=%d output=%s", code, output)
	}
}

func testProcessHasHerdrAncestor(pid int) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	for pid > 1 {
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err == nil {
			for _, entry := range bytes.Split(environ, []byte{0}) {
				key, _, _ := strings.Cut(string(entry), "=")
				if strings.HasPrefix(key, "HERDR_") {
					return true
				}
			}
		}
		status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			return false
		}
		parent := 0
		for _, line := range strings.Split(string(status), "\n") {
			if value, found := strings.CutPrefix(line, "PPid:"); found {
				parent, _ = strconv.Atoi(strings.TrimSpace(value))
				break
			}
		}
		if parent <= 0 || parent == pid {
			return false
		}
		pid = parent
	}
	return false
}

func runReleaseCLI(t *testing.T, binary, cwd string, env []string, args ...string) (int, string) {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Dir, command.Env = cwd, env
	output, err := command.CombinedOutput()
	if err == nil {
		return 0, string(output)
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), string(output)
	}
	t.Fatalf("run %s %s: %v\n%s", binary, strings.Join(args, " "), err, output)
	return 1, string(output)
}

func makeUpdateTestRelease(t *testing.T, binaryPath, tag string) testReleaseAsset {
	t.Helper()
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
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
	archiveBytes := archive.Bytes()
	checksum := sha256.Sum256(archiveBytes)
	return testReleaseAsset{tag: tag, archive: archiveBytes, archiveName: archiveName, checksum: hex.EncodeToString(checksum[:])}
}

func downloadOfficialReleaseBinary(t *testing.T, root, tag string) string {
	t.Helper()
	asset := downloadOfficialReleaseAsset(t, tag)
	gz, err := gzip.NewReader(bytes.NewReader(asset.archive))
	if err != nil {
		t.Fatalf("open official %s release archive: %v", tag, err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read official %s release archive: %v", tag, err)
		}
		if header.Name != "posse" || header.Typeflag != tar.TypeReg {
			continue
		}
		if header.Size < 1 || header.Size > 150<<20 {
			t.Fatalf("official %s release binary has invalid size %d", tag, header.Size)
		}
		binary, err := io.ReadAll(io.LimitReader(reader, header.Size))
		if err != nil {
			t.Fatalf("extract official %s release binary: %v", tag, err)
		}
		binaryPath := filepath.Join(root, "release-"+strings.TrimPrefix(tag, "v"), "posse")
		if err := os.MkdirAll(filepath.Dir(binaryPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(binaryPath, binary, 0o700); err != nil {
			t.Fatal(err)
		}
		return binaryPath
	}
	t.Fatalf("official %s release archive does not contain posse", tag)
	return ""
}

func downloadOfficialReleaseAsset(t *testing.T, tag string) testReleaseAsset {
	t.Helper()
	assetName := fmt.Sprintf("posse_%s_linux_amd64.tar.gz", strings.TrimPrefix(tag, "v"))
	baseURL := "https://github.com/Thanhbinh1905/posse/releases/download/" + tag + "/"
	client := &http.Client{Timeout: 2 * time.Minute}
	fetch := func(name string, limit int64) []byte {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+name, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("User-Agent", "posse-e2e")
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("download official %s release asset %s: %v", tag, name, err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("download official %s release asset %s: HTTP %d", tag, name, response.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
		if err != nil || int64(len(data)) > limit {
			t.Fatalf("read official %s release asset %s: %v", tag, name, err)
		}
		return data
	}
	archive := fetch(assetName, 200<<20)
	checksums := fetch("checksums.txt", 2<<20)
	actual := sha256.Sum256(archive)
	verified := false
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == assetName && strings.EqualFold(fields[0], hex.EncodeToString(actual[:])) {
			verified = true
			break
		}
	}
	if !verified {
		t.Fatalf("official %s release checksum did not match %s", tag, assetName)
	}
	return testReleaseAsset{tag: tag, archive: archive, archiveName: assetName, checksum: hex.EncodeToString(actual[:])}
}
