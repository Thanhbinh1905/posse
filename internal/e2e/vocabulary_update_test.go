//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
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

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestOfficial015HomeRefusesDirectUpgradeBeforeV033(t *testing.T) {
	if testProcessHasHerdrAncestor(os.Getpid()) {
		t.Skip("release update fixture requires a process tree outside an active Herdr session")
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("official v0.15.0 release fixture is Linux amd64")
	}
	f := newPRLifecycleFixtureWithManualWatchOwner(t)
	defer f.db.Close()
	userEnv := newPRLifecycleUserShellEnv(t, f)
	// Update runs setup against the running binary, which refuses binaries
	// under /tmp. Keep the release fixture in the Task scratch root when set;
	// CI's /tmp default falls back to /var/tmp like the 0.14 upgrade test.
	binaryRoot := filepath.Clean(os.TempDir())
	if binaryRoot == "/tmp" || strings.HasPrefix(binaryRoot, "/tmp/") {
		binaryRoot = "/var/tmp"
	}
	stableBinaryDir := newFixtureRootAt(t, binaryRoot, fixturePrefix("official-update-installed-"))
	legacyBinary := downloadOfficialReleaseBinary(t, stableBinaryDir, "v0.15.0")
	home := filepath.Join(f.root, "official-015-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	env := setEnv(userEnv, "POSSE_HOME", home)
	configPath := filepath.Join(home, "config.toml")
	original := []byte("# posse vocabulary: 1\n# Keep this User comment.\n[defaults]\nreview = 'local' # Existing review behavior\nmerge_mode = 'local' # Local delivery is unchanged\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, legacyBinary, f.repo, env, "config", "show"); code != 0 || !strings.Contains(output, "review") {
		t.Fatalf("official v0.15.0 did not load its legacy review config: code=%d output=%s", code, output)
	}

	release := makeUpdateTestRelease(t, f.binary, "v0.15.1")
	proxyURL, certPath := startUpdateReleaseProxy(t, release)
	updateEnv := setEnv(env, "PATH", updateFixturePath(t, f))
	updateEnv = setEnv(updateEnv, "HTTPS_PROXY", proxyURL)
	updateEnv = setEnv(updateEnv, "https_proxy", proxyURL)
	updateEnv = setEnv(updateEnv, "NO_PROXY", "localhost,127.0.0.1,::1")
	updateEnv = setEnv(updateEnv, "no_proxy", "localhost,127.0.0.1,::1")
	updateEnv = setEnv(updateEnv, "SSL_CERT_FILE", certPath)
	if code, output := runReleaseCLIInUserPane(t, f, legacyBinary, f.repo, updateEnv, "update", "--version", release.tag); code != 1 || !strings.Contains(output, "setup_failed") || !strings.Contains(output, "update to posse v0.33.0 first") {
		t.Fatalf("v0.15.0 home update did not refuse the unsupported schema: code=%d output=%s", code, output)
	}
	want := "# posse vocabulary: 2\n# Keep this User comment.\n[defaults]\nreview = 'quick' # Existing review behavior\nmerge_mode = 'local' # Local delivery is unchanged\n"
	if got, err := os.ReadFile(configPath); err != nil || string(got) != want {
		t.Fatalf("update config = %q err=%v; want migrated config %q", got, err, want)
	}
}

func TestOfficial014HomeRefusesDirectUpgradeBeforeV033(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("official v0.14.0 release fixture is Linux amd64")
	}
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	stableBinaryDir := newFixtureRootAt(t, "/var/tmp", fixturePrefix("official-update-installed-"))
	legacyBinary := downloadOfficialReleaseBinary(t, stableBinaryDir, "v0.14.0")
	home := filepath.Join(f.root, "legacy-release-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	env := setEnv(f.env, "POSSE_HOME", home)
	if code, output := runReleaseCLI(t, legacyBinary, f.repo, env, "config", "set", "defaults.landing_mode", "pr"); code != 0 {
		t.Fatalf("v0.14.0 could not create its legacy delivery setting: %d %s", code, output)
	}
	command := exec.Command(f.binary, "config", "set", "defaults.review", "none")
	command.Dir, command.Env = f.repo, env
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "update to posse v0.33.0 first") {
		t.Fatalf("current binary did not refuse the official v0.14.0 home: err=%v output=%s", err, output)
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

func runReleaseCLIInUserPane(t *testing.T, fixture *prLifecycleFixture, binary, cwd string, env []string, args ...string) (int, string) {
	t.Helper()
	paneID := testEnvValue(env, "HERDR_PANE_ID")
	if paneID == "" {
		t.Fatal("User shell environment has no HERDR_PANE_ID")
	}
	outputPath := filepath.Join(fixture.root, "release-cli-output")
	statusPath := filepath.Join(fixture.root, "release-cli-status")
	scriptPath := filepath.Join(fixture.root, "run-release-cli.sh")
	var script strings.Builder
	script.WriteString("#!/bin/sh\ncd " + shellQuote(cwd) + " || exit 125\nunset POSSE_TEST_HERDR\n")
	for _, key := range []string{"PATH", "POSSE_HOME", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy", "SSL_CERT_FILE"} {
		if value := testEnvValue(env, key); value != "" {
			script.WriteString("export " + key + "=" + shellQuote(value) + "\n")
		}
	}
	script.WriteString("\"$@\" >" + shellQuote(outputPath) + " 2>&1\ncode=$?\nprintf '%s\n' \"$code\" >" + shellQuote(statusPath) + "\nexit 0\n")
	if err := os.WriteFile(scriptPath, []byte(script.String()), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(outputPath)
	_ = os.Remove(statusPath)
	command := shellQuote(scriptPath)
	for _, arg := range append([]string{binary}, args...) {
		command += " " + shellQuote(arg)
	}
	client := herdr.NewWithEnv("herdr", env)
	if _, err := client.Run(context.Background(), "pane", "run", paneID, command); err != nil {
		t.Fatalf("run release CLI in User shell: %v", err)
	}
	if !waitForCondition(30*time.Second, func() bool {
		_, err := os.Stat(statusPath)
		return err == nil
	}) {
		t.Fatal("release CLI did not finish in the User shell")
	}
	status, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(status)))
	if err != nil {
		t.Fatalf("invalid release CLI exit code %q: %v", status, err)
	}
	return code, string(output)
}

func newPRLifecycleUserShellEnv(t *testing.T, fixture *prLifecycleFixture) []string {
	t.Helper()
	client := herdr.NewWithEnv("herdr", fixture.env)
	workspace, err := createWorkspace(client, fixture.repo)
	if err != nil {
		t.Fatalf("create isolated User shell workspace: %v", err)
	}
	env := setEnv(fixture.env, "HERDR_ENV", "1")
	env = setEnv(env, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	env = setEnv(env, "HERDR_WORKSPACE_ID", workspace.Workspace.WorkspaceID)
	env = setEnv(env, "HERDR_TAB_ID", workspace.RootPane.TabID)
	env = setEnv(env, "HERDR_SOCKET_PATH", herdr.SocketPath(fixture.env))
	return env
}

func updateFixturePath(t *testing.T, fixture *prLifecycleFixture) string {
	t.Helper()
	binDir := filepath.Join(fixture.root, "update-bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	herdrLink := filepath.Join(binDir, "herdr")
	if err := os.Symlink(herdrBinary, herdrLink); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	return binDir + string(os.PathListSeparator) + "/usr/bin:/bin"
}

func testEnvValue(env []string, key string) string {
	for _, entry := range env {
		name, value, found := strings.Cut(entry, "=")
		if found && name == key {
			return value
		}
	}
	return ""
}

// runReleaseCLI models a plain User shell. The binary is orphaned from the test
// process before it runs, so a Herdr server or agent above the test (as in a
// Worker pane) is not among its ancestors, as in CI.
func runReleaseCLI(t *testing.T, binary, cwd string, env []string, args ...string) (int, string) {
	t.Helper()
	env = taskAuthorGenerationEnv(env)
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "output")
	command := exec.Command("sh", append([]string{"-c", `( "$@" >"$0" 2>&1 </dev/null; echo $? >"$0.status.tmp"; mv "$0.status.tmp" "$0.status" ) &`, outputPath, binary}, args...)...)
	command.Dir, command.Env = cwd, env
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("launch %s %s: %v\n%s", binary, strings.Join(args, " "), err, output)
	}
	statusPath := outputPath + ".status"
	if !waitForCondition(5*time.Minute, func() bool { _, err := os.Stat(statusPath); return err == nil }) {
		t.Fatalf("%s %s did not finish", binary, strings.Join(args, " "))
	}
	status, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(status)))
	if err != nil {
		t.Fatalf("parse exit status %q: %v", status, err)
	}
	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(output)
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
