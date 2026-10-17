//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func assertE2EReportBundle(t *testing.T, root string, task store.Task) {
	t.Helper()
	manifestPath := filepath.Join(root, "report-manifest.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read sealed Report manifest: %v", err)
	}
	manifestHash := sha256.Sum256(manifestBytes)
	if got := hex.EncodeToString(manifestHash[:]); got != task.ReportDigest {
		t.Fatalf("Task Report digest = %q, manifest digest = %q", task.ReportDigest, got)
	}
	if string(manifestBytes) != task.ReportManifestJSON {
		t.Fatal("Task snapshot manifest bytes differ from report-manifest.json")
	}
	var manifest struct {
		Version int    `json:"version"`
		Task    string `json:"task"`
		Files   []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("decode Report manifest: %v", err)
	}
	if manifest.Version != 1 || manifest.Task != "t1" {
		t.Fatalf("manifest identity = version %d task %q", manifest.Version, manifest.Task)
	}
	reportFound := false
	for _, file := range manifest.Files {
		if file.Path == "report.md" {
			reportFound = true
		}
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file.Path)))
		if err != nil {
			t.Fatalf("read manifested attachment %q: %v", file.Path, err)
		}
		hash := sha256.Sum256(contents)
		if got := hex.EncodeToString(hash[:]); got != file.SHA256 {
			t.Fatalf("attachment %q digest = %q, manifest says %q", file.Path, got, file.SHA256)
		}
	}
	if !reportFound {
		t.Fatal("Report manifest omitted report.md")
	}
	snapshot, err := os.ReadFile(filepath.Join(root, "task.toml"))
	if err != nil {
		t.Fatalf("read Task snapshot: %v", err)
	}
	if !strings.Contains(string(snapshot), `report_digest = "`+task.ReportDigest+`"`) || !strings.Contains(string(snapshot), `report_kind = "spec"`) {
		t.Fatalf("Task snapshot omitted Report metadata:\n%s", snapshot)
	}
}
