package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/execution"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

const inPlaceManifestVersion = 1

type inPlaceFile struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
	Target string `json:"target,omitempty"`
}

type inPlaceSnapshot struct {
	Version int           `json:"version"`
	Root    string        `json:"root"`
	Scopes  []string      `json:"scopes"`
	Files   []inPlaceFile `json:"files"`
	Digest  string        `json:"digest"`
}

type inPlaceChange struct {
	Path       string `json:"path"`
	Status     string `json:"status"`
	BeforeSHA  string `json:"before_sha256,omitempty"`
	AfterSHA   string `json:"after_sha256,omitempty"`
	BeforeMode uint32 `json:"before_mode,omitempty"`
	AfterMode  uint32 `json:"after_mode,omitempty"`
}

type inPlaceOutput struct {
	Version        int             `json:"version"`
	Root           string          `json:"root"`
	BaselineSHA256 string          `json:"baseline_sha256"`
	CurrentSHA256  string          `json:"current_sha256"`
	OutputSHA256   string          `json:"output_sha256"`
	ScopeViolation string          `json:"scope_violation,omitempty"`
	ChangedFiles   []inPlaceChange `json:"changed_files"`
	CapturedAt     string          `json:"captured_at"`
}

type inPlaceCheck struct {
	Command      string `json:"command"`
	OutputSHA256 string `json:"output_sha256"`
}

func inPlaceSourceKey(taskID int64, path string) string {
	return fmt.Sprintf("in-place:%d:%s", taskID, filepath.Clean(path))
}

func canonicalInPlaceScopes(root string, scopes []string) ([]string, error) {
	root = filepath.Clean(root)
	canonicalPaths := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if scope == "" || !filepath.IsLocal(scope) || filepath.Clean(scope) != scope {
			return nil, axi.Failure("in_place_scope_invalid", "selected path is not a clean relative path: "+scope, false)
		}
		path := filepath.Join(root, filepath.FromSlash(scope))
		canonical, err := execution.CanonicalPath(path)
		if err != nil {
			return nil, err
		}
		if scope == "." {
			canonical = root
		}
		if !pathWithin(root, canonical) {
			return nil, axi.Failure("in_place_scope_invalid", "selected path resolves outside the registered Project: "+scope, false)
		}
		relative, err := filepath.Rel(root, canonical)
		if err != nil {
			return nil, err
		}
		for _, component := range strings.Split(filepath.ToSlash(relative), "/") {
			if component == ".git" {
				return nil, axi.Failure("in_place_scope_invalid", "selected path resolves to Git metadata: "+scope, false)
			}
		}
		for _, previous := range canonicalPaths {
			if pathWithin(previous, canonical) || pathWithin(canonical, previous) {
				return nil, axi.Failure("in_place_scope_invalid", "selected paths overlap after resolving symlinks", false)
			}
		}
		canonicalPaths = append(canonicalPaths, canonical)
	}
	return canonicalPaths, nil
}

func manifestInPlaceScopes(root string, scopes []string) ([]string, error) {
	root = filepath.Clean(root)
	canonicalPaths := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if scope == "" || !filepath.IsLocal(scope) || filepath.Clean(filepath.FromSlash(scope)) != filepath.FromSlash(scope) {
			return nil, fmt.Errorf("in-place baseline contains an invalid scope %q", scope)
		}
		canonical := filepath.Clean(filepath.Join(root, filepath.FromSlash(scope)))
		if !pathWithin(root, canonical) {
			return nil, fmt.Errorf("in-place baseline scope escapes its Project root: %q", scope)
		}
		for _, component := range strings.Split(filepath.ToSlash(scope), "/") {
			if component == ".git" {
				return nil, fmt.Errorf("in-place baseline scope includes Git metadata")
			}
		}
		for _, previous := range canonicalPaths {
			if pathWithin(previous, canonical) || pathWithin(canonical, previous) {
				return nil, fmt.Errorf("in-place baseline contains overlapping scopes")
			}
		}
		canonicalPaths = append(canonicalPaths, canonical)
	}
	if len(canonicalPaths) == 0 {
		return nil, fmt.Errorf("in-place baseline has no path scopes")
	}
	return canonicalPaths, nil
}

func relativeInPlaceScopes(root string, canonicalPaths []string) ([]string, error) {
	scopes := make([]string, 0, len(canonicalPaths))
	for _, canonical := range canonicalPaths {
		relative, err := filepath.Rel(root, canonical)
		if err != nil || !pathWithin(root, canonical) {
			return nil, fmt.Errorf("in-place scope does not belong to its Project root")
		}
		scopes = append(scopes, filepath.ToSlash(relative))
	}
	return scopes, nil
}

func validateInPlacePlanScopes(root string, planned, frozen []string) error {
	canonical, err := canonicalInPlaceScopes(root, planned)
	if err != nil {
		return err
	}
	relative, err := relativeInPlaceScopes(root, canonical)
	if err != nil {
		return err
	}
	if strings.Join(relative, "\x00") != strings.Join(frozen, "\x00") {
		return axi.Failure("in_place_scope_changed", "approved path scope no longer resolves to its admitted location", false,
			"Restore the original path or preserve the interrupted Task before changing its scope")
	}
	return nil
}

func ensureInPlaceLeases(ctx context.Context, db *store.DB, project store.Project, task store.Task, plan taskplan.Plan) error {
	canonicalPaths, err := canonicalInPlaceScopes(project.Root, plan.Execution.Paths)
	if err != nil {
		return err
	}
	return ensureInPlaceLeasesAt(ctx, db, project, task, canonicalPaths)
}

func ensureInPlaceLeasesAt(ctx context.Context, db *store.DB, project store.Project, task store.Task, canonicalPaths []string) error {
	manager := execution.NewManager(db)
	if err := manager.EnsureMountLeases(ctx); err != nil {
		return resourceLeaseError(err)
	}
	acquired := make([]store.ResourceLease, 0, len(canonicalPaths))
	rollback := func() {
		for _, lease := range acquired {
			_ = manager.Release(context.Background(), lease)
		}
	}
	for _, canonical := range canonicalPaths {
		if !pathWithin(project.Root, canonical) {
			rollback()
			return axi.Failure("in_place_scope_invalid", "frozen path scope resolves outside the registered Project", false)
		}
		source := inPlaceSourceKey(task.ID, canonical)
		lease, err := db.ResourceLeaseBySource(ctx, source)
		if err == nil {
			if lease.OwnerProjectID != project.ID || lease.OwnerTaskID != task.ID || lease.ResourceType != store.ResourcePath || lease.ScopePath != canonical {
				rollback()
				return axi.Failure("in_place_lease_invalid", "stored in-place path lease does not match this Task", false)
			}
			continue
		}
		if !store.IsNotFound(err) {
			rollback()
			return err
		}
		lease, err = manager.AcquirePath(ctx, canonical, store.ResourceLeaseRequest{
			OwnerProjectID: project.ID, OwnerTaskID: task.ID,
			Owner: fmt.Sprintf("in-place Task t%d", task.Seq), SourceKey: source,
		})
		if err != nil {
			rollback()
			return resourceLeaseError(err)
		}
		acquired = append(acquired, lease)
	}
	return nil
}

func captureInPlaceBaseline(ctx context.Context, db *store.DB, project store.Project, task store.Task, plan taskplan.Plan) error {
	root, err := execution.CanonicalPath(project.Root)
	if err != nil {
		return err
	}
	if root != filepath.Clean(project.Root) {
		return axi.Failure("in_place_root_changed", "registered Project root no longer resolves to its canonical path", false)
	}
	baselinePath := inPlaceBaselinePath(plan)
	if _, statErr := os.Stat(baselinePath); statErr == nil {
		baseline, err := loadInPlaceSnapshot(baselinePath)
		if err != nil {
			return err
		}
		if err := validateInPlaceRoot(root, baseline.Root); err != nil {
			return err
		}
		if err := validateInPlacePlanScopes(root, plan.Execution.Paths, baseline.Scopes); err != nil {
			return err
		}
		canonical, err := manifestInPlaceScopes(root, baseline.Scopes)
		if err != nil {
			return err
		}
		if err := ensureInPlaceLeasesAt(ctx, db, project, task, canonical); err != nil {
			return err
		}
		return db.UpdateTaskMountPath(ctx, task.ID, 0, root)
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	canonicalScopes, err := canonicalInPlaceScopes(root, plan.Execution.Paths)
	if err != nil {
		return err
	}
	if err := ensureInPlaceLeasesAt(ctx, db, project, task, canonicalScopes); err != nil {
		return err
	}
	if err := db.UpdateTaskMountPath(ctx, task.ID, 0, root); err != nil {
		return err
	}
	snapshot, err := scanInPlaceFiles(ctx, root)
	if err != nil {
		return err
	}
	snapshot.Scopes, err = relativeInPlaceScopes(root, canonicalScopes)
	if err != nil {
		return err
	}
	snapshot.Digest = inPlaceSnapshotDigest(snapshot.Files, snapshot.Scopes)
	if err := validateInPlaceSymlinks(root, snapshot.Scopes, snapshot.Files); err != nil {
		return err
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return writeFile(baselinePath, encoded)
}

func inPlaceBaselinePath(plan taskplan.Plan) string {
	return filepath.Join(plan.Paths.Output, "in-place-baseline.json")
}

func inPlaceOutputPath(plan taskplan.Plan) string {
	return filepath.Join(plan.Paths.Output, "in-place-output.json")
}

func archiveInPlaceOutput(plan taskplan.Plan, launch int) error {
	current := inPlaceOutputPath(plan)
	if _, err := os.Stat(current); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	archive := filepath.Join(plan.Paths.Output, fmt.Sprintf("in-place-output-launch-%d.json", launch))
	for suffix := 2; ; suffix++ {
		if _, err := os.Lstat(archive); os.IsNotExist(err) {
			break
		} else if err != nil {
			return err
		}
		archive = filepath.Join(plan.Paths.Output, fmt.Sprintf("in-place-output-launch-%d-%d.json", launch, suffix))
	}
	return os.Rename(current, archive)
}

func captureLostInPlaceEvidence(ctx context.Context, db *store.DB, project store.Project) error {
	if !project.IsDirectory() {
		return nil
	}
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.State != store.StateLost || task.Launches == 0 {
			continue
		}
		plan, err := task.AdmittedPlan()
		if err != nil {
			return admittedPlanFailure(err)
		}
		if plan == nil || plan.Execution.Mode != "in-place" {
			continue
		}
		currentPath := inPlaceOutputPath(*plan)
		if data, err := os.ReadFile(currentPath); err == nil {
			var previous inPlaceOutput
			if err := json.Unmarshal(data, &previous); err != nil {
				return fmt.Errorf("decode interrupted in-place output evidence: %w", err)
			}
			baseline, err := loadInPlaceSnapshot(inPlaceBaselinePath(*plan))
			if err != nil {
				return err
			}
			if err := validateInPlaceRoot(task.WorktreePath, baseline.Root); err != nil {
				return err
			}
			current, err := scanInPlaceFiles(ctx, task.WorktreePath)
			if err != nil {
				return err
			}
			current.Digest = inPlaceSnapshotDigest(current.Files, baseline.Scopes)
			if previous.CurrentSHA256 == current.Digest {
				if previous.ScopeViolation != "" {
					if err := recordInPlaceScopeNotice(ctx, db, project, task, previous); err != nil {
						return err
					}
				}
				continue
			}
			if err := archiveInPlaceOutput(*plan, task.Launches); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		output, captureErr := captureInPlaceOutput(ctx, *plan, task.WorktreePath)
		if captureErr != nil {
			var failure *axi.Error
			if !errors.As(captureErr, &failure) || failure.Code != "in_place_scope_violation" {
				return captureErr
			}
			if err := recordInPlaceScopeNotice(ctx, db, project, task, output); err != nil {
				return err
			}
		}
	}
	return nil
}

func loadInPlaceSnapshot(path string) (inPlaceSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return inPlaceSnapshot{}, err
	}
	var snapshot inPlaceSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return inPlaceSnapshot{}, fmt.Errorf("decode in-place baseline: %w", err)
	}
	if snapshot.Version != inPlaceManifestVersion || snapshot.Root == "" || snapshot.Digest != inPlaceSnapshotDigest(snapshot.Files, snapshot.Scopes) {
		return inPlaceSnapshot{}, fmt.Errorf("in-place baseline is invalid or has been modified")
	}
	if _, err := manifestInPlaceScopes(snapshot.Root, snapshot.Scopes); err != nil {
		return inPlaceSnapshot{}, err
	}
	return snapshot, nil
}

func scanInPlaceFiles(ctx context.Context, root string) (inPlaceSnapshot, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return inPlaceSnapshot{}, err
	}
	if !rootInfo.IsDir() {
		return inPlaceSnapshot{}, axi.Failure("in_place_root_changed", "registered Project root is no longer a directory", false)
	}
	files := make([]inPlaceFile, 0)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if isGitMetadataPath(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		file := inPlaceFile{Path: rel, Mode: uint32(info.Mode().Perm())}
		if info.IsDir() {
			file.Type = "directory"
		} else if info.Mode()&os.ModeSymlink != 0 {
			file.Type = "symlink"
			file.Target, err = os.Readlink(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256([]byte(file.Target))
			file.SHA256 = hex.EncodeToString(sum[:])
		} else if info.Mode().IsRegular() {
			file.Type = "file"
			contents, err := os.Open(path)
			if err != nil {
				return err
			}
			digest := sha256.New()
			_, copyErr := io.Copy(digest, contents)
			closeErr := contents.Close()
			if copyErr != nil || closeErr != nil {
				return errors.Join(copyErr, closeErr)
			}
			file.SHA256 = hex.EncodeToString(digest.Sum(nil))
		} else {
			return fmt.Errorf("unsupported file type in registered Project: %s", rel)
		}
		files = append(files, file)
		return nil
	})
	if err != nil {
		return inPlaceSnapshot{}, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return inPlaceSnapshot{Version: inPlaceManifestVersion, Root: filepath.Clean(root), Files: files, Digest: inPlaceSnapshotDigest(files, nil)}, nil
}

func inPlaceSnapshotDigest(files []inPlaceFile, scopes []string) string {
	data, _ := json.Marshal(struct {
		Scopes []string      `json:"scopes"`
		Files  []inPlaceFile `json:"files"`
	}{Scopes: scopes, Files: files})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func captureInPlaceOutput(ctx context.Context, plan taskplan.Plan, root string) (inPlaceOutput, error) {
	baseline, err := loadInPlaceSnapshot(inPlaceBaselinePath(plan))
	if err != nil {
		return inPlaceOutput{}, err
	}
	if err := validateInPlaceRoot(root, baseline.Root); err != nil {
		return inPlaceOutput{}, err
	}
	current, err := scanInPlaceFiles(ctx, root)
	if err != nil {
		return inPlaceOutput{}, err
	}
	current.Scopes = append([]string(nil), baseline.Scopes...)
	current.Digest = inPlaceSnapshotDigest(current.Files, current.Scopes)
	before := make(map[string]inPlaceFile, len(baseline.Files))
	after := make(map[string]inPlaceFile, len(current.Files))
	for _, file := range baseline.Files {
		before[file.Path] = file
	}
	for _, file := range current.Files {
		after[file.Path] = file
	}
	all := make(map[string]bool, len(before)+len(after))
	for path := range before {
		all[path] = true
	}
	for path := range after {
		all[path] = true
	}
	paths := make([]string, 0, len(all))
	for path := range all {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	changes := make([]inPlaceChange, 0)
	for _, path := range paths {
		old, oldOK := before[path]
		next, nextOK := after[path]
		if oldOK && nextOK && old.Type == next.Type && old.Mode == next.Mode && old.SHA256 == next.SHA256 && old.Target == next.Target {
			continue
		}
		change := inPlaceChange{Path: path}
		switch {
		case !oldOK:
			change.Status = "added"
		case !nextOK:
			change.Status = "deleted"
		default:
			change.Status = "modified"
		}
		if oldOK {
			change.BeforeSHA, change.BeforeMode = old.SHA256, old.Mode
		}
		if nextOK {
			change.AfterSHA, change.AfterMode = next.SHA256, next.Mode
		}
		changes = append(changes, change)
	}
	scopeErr := validateInPlaceChanges(root, baseline.Scopes, changes)
	if symlinkErr := validateInPlaceSymlinks(root, baseline.Scopes, current.Files); scopeErr == nil {
		scopeErr = symlinkErr
	}
	payload, err := json.Marshal(changes)
	if err != nil {
		return inPlaceOutput{}, err
	}
	digest := sha256.Sum256(payload)
	output := inPlaceOutput{Version: inPlaceManifestVersion, Root: filepath.Clean(root), BaselineSHA256: baseline.Digest, CurrentSHA256: current.Digest,
		OutputSHA256: hex.EncodeToString(digest[:]), ChangedFiles: changes, CapturedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if scopeErr != nil {
		output.ScopeViolation = scopeErr.Error()
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return inPlaceOutput{}, err
	}
	if err := writeFile(inPlaceOutputPath(plan), encoded); err != nil {
		return inPlaceOutput{}, err
	}
	return output, scopeErr
}

func recordInPlaceScopeNotice(ctx context.Context, db *store.DB, project store.Project, task store.Task, output inPlaceOutput) error {
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		return err
	}
	for _, notice := range notices {
		if notice.TaskID != task.ID || notice.Kind != "in_place_scope_violation" {
			continue
		}
		var previous inPlaceOutput
		if json.Unmarshal([]byte(notice.DataJSON), &previous) == nil && previous.OutputSHA256 == output.OutputSHA256 {
			return nil
		}
	}
	_, err = db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "in_place_scope_violation", Summary: task.Title + ": " + output.ScopeViolation, DataJSON: marshalJSON(output)})
	return err
}

func validateInPlaceRoot(root, expected string) error {
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return axi.Failure("in_place_root_changed", "registered Project root is no longer a directory", false)
	}
	canonical, err := execution.CanonicalPath(root)
	if err != nil {
		return err
	}
	if canonical != root || canonical != filepath.Clean(expected) {
		return axi.Failure("in_place_root_changed", "Task source root no longer matches its admitted baseline", false)
	}
	return nil
}

func validateInPlaceSymlinks(root string, scopes []string, files []inPlaceFile) error {
	resolvedScopes, err := manifestInPlaceScopes(root, scopes)
	if err != nil {
		return err
	}
	for _, file := range files {
		if file.Type != "symlink" {
			continue
		}
		linkPath := filepath.Join(root, filepath.FromSlash(file.Path))
		for _, scope := range resolvedScopes {
			if !pathWithin(scope, linkPath) {
				continue
			}
			target := file.Target
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(linkPath), target)
			}
			resolvedTarget, err := execution.CanonicalPath(target)
			if err != nil {
				return err
			}
			allowed := false
			for _, allowedScope := range resolvedScopes {
				if pathWithin(allowedScope, resolvedTarget) {
					allowed = true
					break
				}
			}
			if !allowed {
				return axi.Failure("in_place_scope_violation", "approved path contains a symlink outside its leased scopes: "+file.Path, false,
					"Keep symlink targets inside the selected paths. The symlink and any source changes remain in place for inspection")
			}
		}
	}
	return nil
}

func validateInPlaceChanges(root string, scopes []string, changes []inPlaceChange) error {
	if _, err := manifestInPlaceScopes(root, scopes); err != nil {
		return err
	}
	for _, change := range changes {
		covered := false
		for _, scope := range scopes {
			if scope == "." || change.Path == scope || strings.HasPrefix(change.Path, strings.TrimSuffix(scope, "/")+"/") {
				covered = true
				break
			}
		}
		if !covered {
			return axi.Failure("in_place_scope_violation", "Task changed a path outside its approved scope: "+change.Path, false,
				"The source change was left in place for inspection. Ask the Rider to restore or explain it, then retry `posse holler done`")
		}
	}
	return nil
}

func signalFileInTaskScratch(home string, project store.Project, task store.Task, selected string) bool {
	if selected == "" {
		return false
	}
	scratch, err := taskScratchPath(home, project, task)
	if err != nil {
		return false
	}
	path := selected
	if !filepath.IsAbs(path) {
		cwd, err := os.Getwd()
		if err != nil {
			return false
		}
		path = filepath.Join(cwd, path)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return pathWithin(scratch, resolved)
}

func runInPlaceChecks(ctx context.Context, plan taskplan.Plan, root string) ([]inPlaceCheck, error) {
	results := make([]inPlaceCheck, 0, len(plan.Gate))
	for _, check := range plan.Gate {
		output, err := commandOutput(ctx, root, check)
		if err != nil {
			return nil, axi.Failure("in_place_check_failed", fmt.Sprintf("approved check %q failed: %s", check, truncate(strings.TrimSpace(output), 1200)), false,
				"Fix the check failure in the approved path scope, then retry `posse holler done`")
		}
		sum := sha256.Sum256([]byte(output))
		results = append(results, inPlaceCheck{Command: check, OutputSHA256: hex.EncodeToString(sum[:])})
	}
	return results, nil
}

func releaseInPlaceLeases(ctx context.Context, db *store.DB, project store.Project, task store.Task) error {
	leases, err := db.ResourceLeases(ctx, project.ID)
	if err != nil {
		return err
	}
	manager := execution.NewManager(db)
	prefix := fmt.Sprintf("in-place:%d:", task.ID)
	for _, lease := range leases {
		if lease.OwnerTaskID == task.ID && strings.HasPrefix(lease.SourceKey, prefix) {
			if err := manager.Reconcile(ctx, lease, "Rider process and panes are stopped; in-place output was preserved without rollback"); err != nil {
				return err
			}
		}
	}
	return nil
}
