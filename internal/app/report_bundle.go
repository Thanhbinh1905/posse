package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thanhbinh1905/posse/internal/store"
)

const reportBundleManifestName = "report-manifest.json"

type reportBundleManifest struct {
	Version int                `json:"version"`
	Task    string             `json:"task"`
	Files   []reportBundleFile `json:"files"`
}

type reportBundleFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func finalizeReportBundle(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task) error {
	root := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq))
	_, encoded, err := buildReportBundleManifest(root, task)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(root, reportBundleManifestName)
	manifestInfo, statErr := os.Lstat(manifestPath)
	if statErr == nil && !manifestInfo.Mode().IsRegular() {
		return fmt.Errorf("report bundle manifest is not a regular file")
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		return statErr
	}
	if task.ReportDigest != "" {
		if string(encoded) != task.ReportManifestJSON || reportDigest(encoded) != task.ReportDigest {
			return fmt.Errorf("report bundle is immutable; the finalized Report or attachments have changed")
		}
		current, readErr := os.ReadFile(manifestPath)
		if readErr == nil && !bytes.Equal(current, encoded) {
			return fmt.Errorf("report bundle manifest conflicts with the finalized Report digest")
		}
		if readErr != nil && !os.IsNotExist(readErr) {
			return readErr
		}
		if os.IsNotExist(readErr) {
			if err := writeFile(manifestPath, encoded); err != nil {
				return err
			}
		}
		return db.RecordReportBundle(ctx, task.ID, task.ReportDigest, task.ReportManifestJSON)
	}
	if err := writeFile(manifestPath, encoded); err != nil {
		return fmt.Errorf("write Report bundle manifest: %w", err)
	}
	return db.RecordReportBundle(ctx, task.ID, reportDigest(encoded), string(encoded))
}

func buildReportBundleManifest(root string, task store.Task) (reportBundleManifest, []byte, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return reportBundleManifest{}, nil, fmt.Errorf("report bundle directory is unavailable: %w", err)
	}
	if !info.IsDir() {
		return reportBundleManifest{}, nil, fmt.Errorf("report bundle path is not a directory")
	}
	manifest := reportBundleManifest{Version: 1, Task: taskIDString(task.Seq), Files: []reportBundleFile{}}
	err = filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == root || entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		if relative == reportBundleManifestName || relative == "brief.md" || relative == "launch.md" || relative == "relaunch.md" || relative == "findings.toon" || relative == "task.toml" {
			return nil
		}
		portable := filepath.ToSlash(relative)
		if !safeReportBundlePath(portable) {
			return fmt.Errorf("unsafe Report attachment path %q", portable)
		}
		fileInfo, err := os.Lstat(filePath)
		if err != nil {
			return err
		}
		if !fileInfo.Mode().IsRegular() {
			return fmt.Errorf("report attachment %q is not a regular file", portable)
		}
		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		manifest.Files = append(manifest.Files, reportBundleFile{Path: portable, SHA256: hex.EncodeToString(hash.Sum(nil))})
		return nil
	})
	if err != nil {
		return reportBundleManifest{}, nil, err
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	foundReport := false
	for _, file := range manifest.Files {
		if file.Path == "report.md" {
			foundReport = true
			break
		}
	}
	if !foundReport {
		return reportBundleManifest{}, nil, fmt.Errorf("report bundle is missing report.md")
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return reportBundleManifest{}, nil, err
	}
	return manifest, encoded, nil
}

func verifyReportBundle(home string, project store.Project, task store.Task) error {
	if task.ReportDigest == "" || task.ReportManifestJSON == "" {
		return fmt.Errorf("report bundle has not been finalized; Teardown the Research Task first")
	}
	encoded := []byte(task.ReportManifestJSON)
	if reportDigest(encoded) != task.ReportDigest {
		return fmt.Errorf("stored Report bundle manifest does not match its digest")
	}
	root := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq))
	manifestPath := filepath.Join(root, reportBundleManifestName)
	info, err := os.Lstat(manifestPath)
	if err != nil {
		return fmt.Errorf("report bundle manifest is unavailable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("report bundle manifest is not a regular file")
	}
	manifestFile, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(manifestFile, encoded) {
		return fmt.Errorf("report bundle manifest changed after Teardown")
	}
	var recorded reportBundleManifest
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&recorded); err != nil {
		return fmt.Errorf("decode Report bundle manifest: %w", err)
	}
	if recorded.Version != 1 || recorded.Task != taskIDString(task.Seq) {
		return fmt.Errorf("report bundle manifest does not identify the current Task")
	}
	for index, file := range recorded.Files {
		if !safeReportBundlePath(file.Path) || len(file.SHA256) != sha256.Size*2 {
			return fmt.Errorf("report bundle manifest contains an invalid attachment entry")
		}
		if _, err := hex.DecodeString(file.SHA256); err != nil {
			return fmt.Errorf("report bundle manifest contains an invalid attachment digest")
		}
		if index > 0 && recorded.Files[index-1].Path >= file.Path {
			return fmt.Errorf("report bundle manifest paths are not unique and sorted")
		}
		attachmentPath := filepath.Join(root, filepath.FromSlash(file.Path))
		attachmentInfo, err := os.Lstat(attachmentPath)
		if os.IsNotExist(err) {
			return fmt.Errorf("report attachment %q is missing", file.Path)
		}
		if err != nil {
			return err
		}
		if !attachmentInfo.Mode().IsRegular() {
			return fmt.Errorf("report attachment %q is not a regular file", file.Path)
		}
	}
	_, current, err := buildReportBundleManifest(root, task)
	if err != nil {
		return fmt.Errorf("verify Report bundle attachments: %w", err)
	}
	if !bytes.Equal(current, encoded) {
		return fmt.Errorf("report attachment bytes changed after Teardown")
	}
	return nil
}

func safeReportBundlePath(value string) bool {
	if value == "" || pathpkg.IsAbs(value) || pathpkg.Clean(value) != value || strings.Contains(value, "\\") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." || component == ".git" {
			return false
		}
	}
	return true
}

func reportDigest(encoded []byte) string {
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
