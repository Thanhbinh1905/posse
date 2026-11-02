package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

type reportReviewTarget struct {
	ReportTaskID int64  `json:"report_task_id"`
	ReportDigest string `json:"report_digest"`
	ManifestJSON string `json:"manifest_json"`
}

func (s *Service) startReportReview(ctx *axi.Context, home string, project store.Project, report store.Task, profile, name string) error {
	if report.ReportKind != "spec" && report.ReportKind != "tickets" {
		return axi.Failure("report_review_target_invalid", "only publication-bound spec or tickets Reports can be reviewed", false)
	}
	if report.State != store.StateTornDown {
		return axi.Failure("report_review_target_incomplete", "Report review requires a completed Research Task sealed by Teardown", false)
	}
	if err := verifyReportBundle(home, project, report); err != nil {
		return axi.Failure("report_bundle_invalid", err.Error(), false)
	}
	if name == "" {
		name = "report-review-of-" + taskIDString(report.Seq)
	}
	if err := validateWorkerName(name); err != nil {
		return invalidWorkerNameFailure(name, err)
	}
	brief := fmt.Sprintf("---\ntype: review\ntitle: Review publication Report %s\ndone_when: the exact Report bundle receives a review verdict\nreview_of: %s\n---\nReview this publication-bound Report and record a verdict for its exact sealed bundle.\n", taskIDString(report.Seq), taskIDString(report.Seq))
	file, err := os.CreateTemp("", "posse-report-review-*.md")
	if err != nil {
		return err
	}
	briefPath := file.Name()
	defer os.Remove(briefPath)
	if _, err := file.WriteString(brief); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	args := []string{"--brief", briefPath, "--name", name}
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	return s.spawnTask(ctx, args, nil)
}

func reportReviewPlanInputs(home string, project store.Project, report store.Task) ([]taskplan.Input, error) {
	if err := verifyReportBundle(home, project, report); err != nil {
		return nil, err
	}
	root := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(report.Seq))
	manifest, err := os.ReadFile(filepath.Join(root, reportBundleManifestName))
	if err != nil {
		return nil, err
	}
	reportBody, err := os.ReadFile(filepath.Join(root, "report.md"))
	if err != nil {
		return nil, err
	}
	var bundle reportBundleManifest
	if err := json.Unmarshal(manifest, &bundle); err != nil {
		return nil, err
	}
	target := reportReviewTarget{ReportTaskID: report.ID, ReportDigest: report.ReportDigest, ManifestJSON: string(manifest)}
	encoded, err := json.Marshal(target)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{
		"review/report.md":            reportBody,
		"review/report-manifest.json": manifest,
		"review/report-target.json":   encoded,
	}
	for _, attachment := range bundle.Files {
		if attachment.Path == "report.md" {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(attachment.Path)))
		if err != nil {
			return nil, err
		}
		files["review/attachments/"+attachment.Path] = contents
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	inputs := make([]taskplan.Input, 0, len(files)+1)
	for _, path := range paths {
		inputs = append(inputs, taskplan.Select("review-round-file", path, files[path]))
	}
	inputs = append(inputs, taskplan.Select("report-review-target", taskIDString(report.Seq), encoded))
	return inputs, nil
}

func reportReviewTargetFromPlan(plan *taskplan.Plan, reportTaskID int64) (*reportReviewTarget, error) {
	if plan == nil || plan.ReviewOf != reportTaskID {
		return nil, nil
	}
	for _, input := range plan.Inputs {
		if input.Role != "report-review-target" {
			continue
		}
		var target reportReviewTarget
		if err := json.Unmarshal(input.Bytes, &target); err != nil {
			return nil, fmt.Errorf("decode frozen Report review target: %w", err)
		}
		if target.ReportTaskID != reportTaskID || target.ReportDigest == "" || target.ManifestJSON == "" || reportDigest([]byte(target.ManifestJSON)) != target.ReportDigest {
			return nil, fmt.Errorf("frozen Report review target is invalid")
		}
		return &target, nil
	}
	return nil, nil
}

func reportReviewInstructions(target reportReviewTarget) string {
	return fmt.Sprintf("\n\nReview target: publication-bound Report Task t%d.\nReport bundle digest (SHA-256 of the exact manifest bytes): %s\nThe sealed Report, manifest and attachments are provided under {{review_dir}}/; attachments are under attachments/. Review these files only; do not edit them. The target binding is frozen in {{review_dir}}/report-target.json.\n", target.ReportTaskID, target.ReportDigest)
}

func isReportReviewTask(ctx context.Context, db *store.DB, task store.Task) (bool, error) {
	if task.Type != "review" || task.ReviewsTaskID == 0 {
		return false, nil
	}
	report, err := db.TaskByID(ctx, task.ProjectID, task.ReviewsTaskID)
	if err != nil {
		return false, err
	}
	return report.Type == "research" && (report.ReportKind == "spec" || report.ReportKind == "tickets"), nil
}

func validateReportReviewTarget(target reportReviewTarget, report store.Task) error {
	if target.ReportTaskID != report.ID || target.ReportDigest != report.ReportDigest || target.ManifestJSON != report.ReportManifestJSON || strings.TrimSpace(target.ManifestJSON) == "" {
		return fmt.Errorf("the publication-bound Report changed after Review admission")
	}
	return nil
}
