package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/thanhbinh1905/posse/internal/atomicfile"
	"github.com/thanhbinh1905/posse/internal/execgroup"
	dbgen "github.com/thanhbinh1905/posse/internal/store/sqlc"
)

type TaskSnapshot struct {
	Version int     `toml:"version"`
	Project Project `toml:"project"`
	Task    Task    `toml:"task"`
	// LaunchIdentities retain the configured author and reviewer identity for
	// every recorded Task launch. Missing numbers are left missing so rebuilds
	// can report legacy or interrupted history as unknown rather than infer it.
	LaunchIdentities []TaskLaunchIdentity `toml:"launch_identities,omitempty"`
	PRBodyMarkers    []PRBodyMarker       `toml:"pr_body_markers,omitempty"`
	// Members and TaskRepos are set for a workspace Project only.
	Members   []ProjectRepo `toml:"members,omitempty"`
	TaskRepos []TaskRepo    `toml:"task_repos,omitempty"`
}

type projectSnapshot struct {
	Version int                  `toml:"version"`
	Project Project              `toml:"project"`
	Members []ProjectRepo        `toml:"members,omitempty"`
	State   projectStateSnapshot `toml:"state"`
}

type projectStateSnapshot struct {
	CapturedAt           int64              `toml:"captured_at"`
	Mounts               []mountSnapshot    `toml:"mounts,omitempty"`
	Decisions            []decisionSnapshot `toml:"decisions,omitempty"`
	Notices              []noticeSnapshot   `toml:"notices,omitempty"`
	DecisionNoticeCursor int64              `toml:"decision_notice_cursor"`
}

type mountSnapshot struct {
	ID         int64  `toml:"id"`
	ProjectID  int64  `toml:"project_id"`
	Number     int    `toml:"number"`
	Path       string `toml:"path"`
	State      string `toml:"state"`
	TaskID     int64  `toml:"task_id"`
	AcquiredAt int64  `toml:"acquired_at"`
	ReleasedAt int64  `toml:"released_at"`
}

type decisionSnapshot struct {
	ID             int64    `toml:"id"`
	ProjectID      int64    `toml:"project_id"`
	TaskID         int64    `toml:"task_id"`
	Origin         string   `toml:"origin"`
	Question       string   `toml:"question"`
	Options        []string `toml:"options"`
	Answer         string   `toml:"answer"`
	UserQuote      string   `toml:"user_quote"`
	CreatedAt      int64    `toml:"created_at"`
	AnsweredAt     int64    `toml:"answered_at"`
	Kind           string   `toml:"kind"`
	TaskLaunches   int      `toml:"task_launches"`
	ObsoleteAt     int64    `toml:"obsolete_at"`
	ObsoleteReason string   `toml:"obsolete_reason"`
}

type noticeSnapshot struct {
	ID          int64  `toml:"id"`
	ProjectID   int64  `toml:"project_id"`
	TaskID      int64  `toml:"task_id"`
	Kind        string `toml:"kind"`
	Summary     string `toml:"summary"`
	DataJSON    string `toml:"data_json"`
	CreatedAt   int64  `toml:"created_at"`
	DeliveredAt int64  `toml:"delivered_at"`
	AckedAt     int64  `toml:"acked_at"`
}

func sameProjectIdentity(a, b Project) bool {
	if a.UUID != "" && b.UUID != "" {
		return a.UUID == b.UUID
	}
	if a.Name != b.Name {
		return false
	}
	if filepath.Clean(a.Root) == filepath.Clean(b.Root) {
		return true
	}
	return a.CreatedAt != 0 && a.CreatedAt == b.CreatedAt
}

func projectIdentityKey(project Project) string {
	if project.UUID != "" {
		return "uuid\x00" + project.UUID
	}
	if project.CreatedAt != 0 {
		return fmt.Sprintf("legacy\x00%s\x00%d", project.Name, project.CreatedAt)
	}
	return "legacy\x00" + project.Name + "\x00" + filepath.Clean(project.Root)
}

func projectIdentityConflict(a, b Project) error {
	return fmt.Errorf("Project identity conflict: ID %d has conflicting snapshots %q at %q (UUID %q) and %q at %q (UUID %q)",
		a.ID, a.Name, a.Root, a.UUID, b.Name, b.Root, b.UUID)
}

func databaseProjectIdentityConflict(snapshot, current Project) error {
	return fmt.Errorf("Project identity conflict: snapshot Project %q at %q (ID %d, UUID %q) conflicts with current database Project %q at %q (ID %d, UUID %q)",
		snapshot.Name, snapshot.Root, snapshot.ID, snapshot.UUID, current.Name, current.Root, current.ID, current.UUID)
}

func IsCorruptDatabaseFile(path string) bool {
	databaseURL := (&url.URL{Scheme: "file", Path: path}).String()
	database, err := sql.Open("sqlite", databaseURL+"?mode=ro&_pragma=busy_timeout(100)")
	if err != nil {
		return false
	}
	defer database.Close()
	var result string
	if err := database.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		message := strings.ToLower(err.Error())
		return strings.Contains(message, "not a database") || strings.Contains(message, "malformed") || strings.Contains(message, "corrupt")
	}
	return result != "ok"
}

func IsTaskWorktreeSnapshot(home, root string) bool {
	paths, err := filepath.Glob(filepath.Join(home, "projects", "*", "tasks", "t*", "task.toml"))
	if err != nil {
		return false
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var snapshot TaskSnapshot
		if _, err := toml.Decode(string(data), &snapshot); err == nil && snapshot.Task.WorktreePath != "" && filepath.Clean(snapshot.Task.WorktreePath) == filepath.Clean(root) {
			return true
		}
	}
	return false
}

func OpenReadOnly(home string) (*DB, error) {
	if home == "" {
		return nil, fmt.Errorf("POSSE_HOME is empty")
	}
	path := filepath.Join(home, "posse.db")
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	fileURL := (&url.URL{Scheme: "file", Path: path}).String()
	database, err := sql.Open("sqlite", fileURL+"?mode=ro&_pragma=busy_timeout(100)")
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := database.Ping(); err != nil {
		_ = database.Close()
		return nil, err
	}
	return &DB{DB: database, Path: path, queries: dbgen.New(database)}, nil
}

func (db *DB) TaskSnapshotPath(project string, sequence int) string {
	return filepath.Join(filepath.Dir(db.Path), "projects", project, "tasks", fmt.Sprintf("t%d", sequence), "task.toml")
}

func (db *DB) ProjectSnapshotPath(project string) string {
	return filepath.Join(filepath.Dir(db.Path), "projects", project, "project.toml")
}

func (db *DB) NoticeDeliverySnapshotPath(project string) string {
	return filepath.Join(filepath.Dir(db.Path), "projects", project, "notice-delivery.toml")
}

func (db *DB) PersistTask(ctx context.Context, taskID int64) error {
	return db.persistTask(ctx, taskID, true)
}

func (db *DB) persistTask(ctx context.Context, taskID int64, persistProject bool) error {
	var projectID int64
	if err := db.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id=?`, taskID).Scan(&projectID); err != nil {
		return err
	}
	task, err := db.TaskByID(ctx, projectID, taskID)
	if err != nil {
		return err
	}
	project, err := db.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	identities, err := db.TaskLaunchIdentities(ctx, taskID)
	if err != nil {
		return err
	}
	markers, err := db.PRBodyMarkersForTask(ctx, taskID)
	if err != nil {
		return err
	}
	snapshotPath := db.TaskSnapshotPath(project.Name, task.Seq)
	if data, readErr := os.ReadFile(snapshotPath); readErr == nil {
		var previous TaskSnapshot
		if _, err := toml.Decode(string(data), &previous); err != nil {
			return fmt.Errorf("decode existing Task snapshot %s: %w", snapshotPath, err)
		}
		known := make(map[string]struct{}, len(markers))
		for _, marker := range markers {
			known[marker.Repo] = struct{}{}
		}
		for _, marker := range previous.PRBodyMarkers {
			if marker.TaskID == taskID {
				if _, exists := known[marker.Repo]; !exists {
					markers = append(markers, marker)
					known[marker.Repo] = struct{}{}
				}
			}
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	sort.Slice(markers, func(i, j int) bool { return markers[i].Repo < markers[j].Repo })
	snapshot := TaskSnapshot{Version: 1, Project: project, Task: task, LaunchIdentities: identities, PRBodyMarkers: markers}
	if project.IsWorkspace() {
		if snapshot.Members, err = db.ProjectRepos(ctx, projectID); err != nil {
			return err
		}
		if snapshot.TaskRepos, err = db.TaskRepos(ctx, taskID); err != nil {
			return err
		}
	}
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(snapshot); err != nil {
		return err
	}
	if err := atomicfile.Write(db.TaskSnapshotPath(project.Name, task.Seq), encoded.Bytes(), 0o600); err != nil {
		return err
	}
	if persistProject {
		return db.PersistProject(ctx, projectID)
	}
	return nil
}

func (db *DB) PersistProject(ctx context.Context, projectID int64) error {
	project, err := db.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	snapshot := projectSnapshot{Version: 1, Project: project}
	if project.IsWorkspace() {
		if snapshot.Members, err = db.ProjectRepos(ctx, projectID); err != nil {
			return err
		}
	}
	if snapshot.State, err = db.projectStateSnapshot(ctx, projectID); err != nil {
		return err
	}
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(snapshot); err != nil {
		return err
	}
	return atomicfile.Write(db.ProjectSnapshotPath(project.Name), encoded.Bytes(), 0o600)
}

func (db *DB) projectStateSnapshot(ctx context.Context, projectID int64) (projectStateSnapshot, error) {
	mounts, err := db.Mounts(ctx, projectID)
	if err != nil {
		return projectStateSnapshot{}, err
	}
	state := projectStateSnapshot{CapturedAt: time.Now().UnixNano()}
	for _, mount := range mounts {
		state.Mounts = append(state.Mounts, mountSnapshot(mount))
	}
	rows, err := db.QueryContext(ctx, decisionSelect+` WHERE project_id=? ORDER BY id`, projectID)
	if err != nil {
		return projectStateSnapshot{}, err
	}
	for rows.Next() {
		decision, err := decisionRow(rows)
		if err != nil {
			_ = rows.Close()
			return projectStateSnapshot{}, err
		}
		state.Decisions = append(state.Decisions, decisionSnapshot(decision))
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return projectStateSnapshot{}, err
	}
	if err := rows.Close(); err != nil {
		return projectStateSnapshot{}, err
	}
	notices, err := db.Notices(ctx, projectID, false)
	if err != nil {
		return projectStateSnapshot{}, err
	}
	for _, notice := range notices {
		state.Notices = append(state.Notices, noticeSnapshot(notice))
	}
	err = db.QueryRowContext(ctx, `SELECT last_notice_id FROM decision_notice_cursors WHERE project_id=?`, projectID).Scan(&state.DecisionNoticeCursor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return projectStateSnapshot{}, err
	}
	return state, nil
}

func (db *DB) BackupDaily(now time.Time) error {
	directory := filepath.Join(filepath.Dir(db.Path), "backup")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	backup := filepath.Join(directory, "posse-"+now.Format("2006-01-02")+".db")
	if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
		lockPath := filepath.Join(directory, ".backup.lock")
		lock, lockErr := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(lockErr, os.ErrExist) {
			if info, statErr := os.Stat(lockPath); statErr == nil && now.Sub(info.ModTime()) > time.Hour {
				if removeErr := os.Remove(lockPath); removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
					lock, lockErr = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
				}
			}
			if errors.Is(lockErr, os.ErrExist) {
				return nil
			}
		}
		if lockErr != nil {
			return lockErr
		}
		_ = lock.Close()
		defer os.Remove(lockPath)
		if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
			quoted := strings.ReplaceAll(backup, "'", "''")
			if _, err := db.ExecContext(context.Background(), "VACUUM INTO '"+quoted+"'"); err != nil {
				_ = os.Remove(backup)
				return fmt.Errorf("create daily database backup: %w", err)
			}
			if err := os.Chmod(backup, 0o600); err != nil {
				return err
			}
		}
	} else if err != nil {
		return err
	}
	entries, err := filepath.Glob(filepath.Join(directory, "posse-*.db"))
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(entries)))
	if len(entries) > 7 {
		for _, stale := range entries[7:] {
			if err := os.Remove(stale); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func (db *DB) BackupBeforeRebuild(ctx context.Context, now time.Time) (string, error) {
	directory := filepath.Join(filepath.Dir(db.Path), "backup")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	backup := filepath.Join(directory, "posse-before-rebuild-"+now.UTC().Format("20060102T150405.000000000")+".db")
	quoted := strings.ReplaceAll(backup, "'", "''")
	if _, err := db.ExecContext(ctx, "VACUUM INTO '"+quoted+"'"); err != nil {
		_ = os.Remove(backup)
		return "", fmt.Errorf("create pre-rebuild database backup: %w", err)
	}
	if err := os.Chmod(backup, 0o600); err != nil {
		return "", err
	}
	return backup, nil
}

func (db *DB) RebuildFromSnapshots(ctx context.Context, home string) (int, error) {
	if _, err := db.BackupBeforeRebuild(ctx, time.Now()); err != nil {
		return 0, err
	}
	paths, err := filepath.Glob(filepath.Join(home, "projects", "*", "tasks", "t*", "task.toml"))
	if err != nil {
		return 0, err
	}
	projectPaths, err := filepath.Glob(filepath.Join(home, "projects", "*", "project.toml"))
	if err != nil {
		return 0, err
	}
	snapshots := make([]TaskSnapshot, 0, len(paths))
	projects := map[int64]Project{}
	members := map[int64][]ProjectRepo{}
	type deliverySnapshotCandidate struct {
		path     string
		snapshot NoticeDeliverySnapshot
	}
	deliveryCandidates := map[string][]deliverySnapshotCandidate{}
	projectStates := map[int64]projectStateSnapshot{}
	projectSnapshotAt := map[int64]int64{}
	projectIDsByIdentity := map[string]int64{}
	// Legacy Workspaces without project.toml recover Members from their newest Task snapshot.
	memberSnapshotAt := map[int64]int64{}
	memberSnapshotSeq := map[int64]int{}
	for _, path := range projectPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		var snapshot projectSnapshot
		if _, err := toml.Decode(string(data), &snapshot); err != nil {
			return 0, fmt.Errorf("decode %s: %w", path, err)
		}
		if snapshot.Version != 1 || snapshot.Project.ID == 0 || snapshot.Project.Name == "" || snapshot.State.CapturedAt == 0 {
			return 0, fmt.Errorf("invalid Project snapshot %s", path)
		}
		if filepath.Clean(path) != filepath.Clean(db.ProjectSnapshotPath(snapshot.Project.Name)) {
			return 0, fmt.Errorf("Project snapshot path does not match its identity: %s", path)
		}
		identity := projectIdentityKey(snapshot.Project)
		if existing, ok := projects[snapshot.Project.ID]; ok && !sameProjectIdentity(existing, snapshot.Project) {
			return 0, projectIdentityConflict(existing, snapshot.Project)
		}
		if existingID, ok := projectIDsByIdentity[identity]; ok && existingID != snapshot.Project.ID {
			return 0, fmt.Errorf("Project identity conflict: snapshot Project %q at %q has IDs %d and %d", snapshot.Project.Name, snapshot.Project.Root, existingID, snapshot.Project.ID)
		}
		projectIDsByIdentity[identity] = snapshot.Project.ID
		if snapshot.State.CapturedAt > projectSnapshotAt[snapshot.Project.ID] {
			projects[snapshot.Project.ID] = snapshot.Project
			members[snapshot.Project.ID] = snapshot.Members
			projectStates[snapshot.Project.ID] = snapshot.State
			projectSnapshotAt[snapshot.Project.ID] = snapshot.State.CapturedAt
		}
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		var snapshot TaskSnapshot
		if _, err := toml.Decode(string(data), &snapshot); err != nil {
			return 0, fmt.Errorf("decode %s: %w", path, err)
		}
		if snapshot.Version != 1 || snapshot.Project.ID == 0 || snapshot.Project.Name == "" || snapshot.Task.ID == 0 || snapshot.Task.Seq < 1 || snapshot.Task.ProjectID != snapshot.Project.ID {
			return 0, fmt.Errorf("invalid Task snapshot %s", path)
		}
		if filepath.Clean(path) != filepath.Clean(db.TaskSnapshotPath(snapshot.Project.Name, snapshot.Task.Seq)) {
			return 0, fmt.Errorf("Task snapshot path does not match its identity: %s", path)
		}
		identity := projectIdentityKey(snapshot.Project)
		if existing, ok := projects[snapshot.Project.ID]; ok && !sameProjectIdentity(existing, snapshot.Project) {
			return 0, projectIdentityConflict(existing, snapshot.Project)
		}
		if existingID, ok := projectIDsByIdentity[identity]; ok && existingID != snapshot.Project.ID {
			return 0, fmt.Errorf("Project identity conflict: snapshot Project %q at %q has IDs %d and %d", snapshot.Project.Name, snapshot.Project.Root, existingID, snapshot.Project.ID)
		}
		projectIDsByIdentity[identity] = snapshot.Project.ID
		if projectSnapshotAt[snapshot.Project.ID] == 0 {
			projects[snapshot.Project.ID] = snapshot.Project
		}
		if projectSnapshotAt[snapshot.Project.ID] == 0 && len(snapshot.Members) > 0 &&
			(snapshot.Task.UpdatedAt > memberSnapshotAt[snapshot.Project.ID] ||
				snapshot.Task.UpdatedAt == memberSnapshotAt[snapshot.Project.ID] && snapshot.Task.Seq > memberSnapshotSeq[snapshot.Project.ID]) {
			members[snapshot.Project.ID] = snapshot.Members
			memberSnapshotAt[snapshot.Project.ID] = snapshot.Task.UpdatedAt
			memberSnapshotSeq[snapshot.Project.ID] = snapshot.Task.Seq
		}
		snapshots = append(snapshots, snapshot)
	}
	deliveryPaths, err := filepath.Glob(filepath.Join(home, "projects", "*", "notice-delivery.toml"))
	if err != nil {
		return 0, err
	}
	for _, path := range deliveryPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		var snapshot NoticeDeliverySnapshot
		if _, err := toml.Decode(string(data), &snapshot); err != nil {
			return 0, fmt.Errorf("decode %s: %w", path, err)
		}
		if snapshot.Version != 1 || snapshot.Project.ID == 0 || snapshot.Project.Name == "" || filepath.Base(path) != "notice-delivery.toml" {
			return 0, fmt.Errorf("invalid Notice delivery snapshot %s", path)
		}
		deliveryCandidates[snapshot.Project.UUID] = append(deliveryCandidates[snapshot.Project.UUID], deliverySnapshotCandidate{path: path, snapshot: snapshot})
	}
	maxID := int64(0)
	known := make(map[string]bool, len(snapshots))
	for _, snapshot := range snapshots {
		known[fmt.Sprintf("%d:%d", snapshot.Project.ID, snapshot.Task.Seq)] = true
		if snapshot.Task.ID > maxID {
			maxID = snapshot.Task.ID
		}
	}
	// A freshly initialized DB may have reused a snapshot ID for another Project.
	currentRows, err := db.QueryContext(ctx, `SELECT id FROM projects`)
	if err != nil {
		return 0, err
	}
	var currentProjectIDs []int64
	for currentRows.Next() {
		var id int64
		if err := currentRows.Scan(&id); err != nil {
			_ = currentRows.Close()
			return 0, err
		}
		currentProjectIDs = append(currentProjectIDs, id)
	}
	if err := currentRows.Err(); err != nil {
		_ = currentRows.Close()
		return 0, err
	}
	if err := currentRows.Close(); err != nil {
		return 0, err
	}
	for _, projectID := range currentProjectIDs {
		current, err := db.ProjectByID(ctx, projectID)
		if err != nil {
			return 0, err
		}
		snapshot, exists := projects[current.ID]
		if exists && !sameProjectIdentity(snapshot, current) {
			return 0, databaseProjectIdentityConflict(snapshot, current)
		}
		if snapshotID, found := projectIDsByIdentity[projectIdentityKey(current)]; found && snapshotID != current.ID {
			return 0, fmt.Errorf("Project identity conflict: snapshot Project %q at %q has ID %d, but the current database has ID %d", current.Name, current.Root, snapshotID, current.ID)
		}
		if !exists {
			continue
		}
		projects[current.ID] = current
		if current.IsWorkspace() && projectSnapshotAt[current.ID] == 0 && len(members[current.ID]) == 0 {
			members[current.ID], err = db.ProjectRepos(ctx, current.ID)
			if err != nil {
				return 0, err
			}
		}
		state, err := db.projectStateSnapshot(ctx, current.ID)
		if err != nil {
			return 0, err
		}
		projectStates[current.ID] = state
	}
	for projectID, project := range projects {
		if project.UUID == "" {
			project.UUID, err = newProjectUUID()
			if err != nil {
				return 0, err
			}
			projects[projectID] = project
		}
	}
	for index := range snapshots {
		if project, ok := projects[snapshots[index].Project.ID]; ok {
			snapshots[index].Project = project
		}
	}
	projectIDsByUUID := make(map[string]int64, len(projects))
	for projectID, project := range projects {
		if existingID, exists := projectIDsByUUID[project.UUID]; project.UUID != "" && exists && existingID != projectID {
			return 0, fmt.Errorf("Project UUID %q identifies both Project IDs %d and %d", project.UUID, existingID, projectID)
		}
		if project.UUID != "" {
			projectIDsByUUID[project.UUID] = projectID
		}
	}
	deliverySnapshots := make(map[int64]NoticeDeliverySnapshot, len(deliveryCandidates))
	for projectUUID, candidates := range deliveryCandidates {
		projectID, exists := projectIDsByUUID[projectUUID]
		if !exists {
			return 0, fmt.Errorf("Notice delivery snapshot Project UUID %q does not match any Project", projectUUID)
		}
		project := projects[projectID]
		selected := -1
		for index, candidate := range candidates {
			if filepath.Base(filepath.Dir(candidate.path)) == project.Name {
				selected = index
				break
			}
		}
		if selected < 0 {
			selected = 0
			for index := 1; index < len(candidates); index++ {
				if latestDeliveryUpdate(candidates[index].snapshot) > latestDeliveryUpdate(candidates[selected].snapshot) {
					selected = index
				}
			}
		}
		snapshot := candidates[selected].snapshot
		if snapshot.Project.UUID == "" || snapshot.Project.UUID != project.UUID {
			return 0, fmt.Errorf("Notice delivery snapshot Project UUID %q does not match recovered Project UUID %q", snapshot.Project.UUID, project.UUID)
		}
		if snapshot.Project.ID != project.ID {
			return 0, fmt.Errorf("Notice delivery snapshot Project UUID %q has ID %d, current Project ID is %d", projectUUID, snapshot.Project.ID, project.ID)
		}
		deliverySnapshots[projectID] = snapshot
	}
	for _, project := range projects {
		recovered, err := recoverBranchSnapshots(ctx, project, known, maxID)
		if err != nil {
			return 0, err
		}
		for _, snapshot := range recovered {
			maxID = snapshot.Task.ID
			known[fmt.Sprintf("%d:%d", snapshot.Project.ID, snapshot.Task.Seq)] = true
			snapshots = append(snapshots, snapshot)
		}
	}
	for _, snapshot := range snapshots {
		if snapshot.Task.MountID == 0 {
			continue
		}
		state := projectStates[snapshot.Project.ID]
		found := false
		for _, mount := range state.Mounts {
			if mount.ID == snapshot.Task.MountID {
				found = true
				break
			}
		}
		if found {
			continue
		}
		number := snapshot.Task.Seq
		if value, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(snapshot.Task.WorktreePath), "mount-")); err == nil && value > 0 {
			number = value
		}
		state.Mounts = append(state.Mounts, mountSnapshot{
			ID: snapshot.Task.MountID, ProjectID: snapshot.Project.ID, Number: number,
			Path: snapshot.Task.WorktreePath, State: "held", TaskID: snapshot.Task.ID,
		})
		projectStates[snapshot.Project.ID] = state
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Task.ID < snapshots[j].Task.ID })
	if err := db.rebuild(ctx, snapshots, projects, members, projectStates, deliverySnapshots); err != nil {
		return 0, err
	}
	for _, snapshot := range snapshots {
		if err := db.PersistTask(ctx, snapshot.Task.ID); err != nil {
			return 0, err
		}
	}
	return len(snapshots), nil
}

func recoverBranchSnapshots(ctx context.Context, project Project, known map[string]bool, maxID int64) ([]TaskSnapshot, error) {
	branches, err := execgroup.CommandContext(ctx, "git", "-C", project.Root, "branch", "--list", "--format=%(refname:short)", "posse/t*").CombinedOutput()
	if err != nil {
		return nil, nil
	}
	worktrees := map[string]string{}
	output, err := execgroup.CommandContext(ctx, "git", "-C", project.Root, "worktree", "list", "--porcelain").CombinedOutput()
	if err == nil {
		path := ""
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(line, "worktree ") {
				path = strings.TrimPrefix(line, "worktree ")
			} else if strings.HasPrefix(line, "branch refs/heads/") && path != "" {
				worktrees[strings.TrimPrefix(line, "branch refs/heads/")] = path
			}
		}
	}
	var recovered []TaskSnapshot
	for _, branch := range strings.Fields(string(branches)) {
		if !strings.HasPrefix(branch, "posse/t") {
			continue
		}
		sequence, parseErr := strconv.Atoi(strings.TrimPrefix(branch, "posse/t"))
		if parseErr != nil || sequence < 1 || known[fmt.Sprintf("%d:%d", project.ID, sequence)] {
			continue
		}
		path := worktrees[branch]
		maxID++
		task := Task{ID: maxID, ProjectID: project.ID, Seq: sequence, Type: "recovered", Title: "Recovered branch " + branch, State: StateLost, LandingMode: "local", AutonomyReview: "ask", AutonomyLand: "ask", Branch: branch, BaseRef: project.DefaultBranch, WorktreePath: path, CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli()}
		recovered = append(recovered, TaskSnapshot{Version: 1, Project: project, Task: task})
	}
	return recovered, nil
}

func latestDeliveryUpdate(snapshot NoticeDeliverySnapshot) int64 {
	latest := snapshot.CapturedAt
	for _, delivery := range snapshot.Deliveries {
		updatedAt := delivery.UpdatedAt * int64(time.Millisecond)
		if updatedAt > latest {
			latest = updatedAt
		}
	}
	return latest
}

func (db *DB) rebuild(ctx context.Context, snapshots []TaskSnapshot, projects map[int64]Project, members map[int64][]ProjectRepo, projectStates map[int64]projectStateSnapshot, deliverySnapshots map[int64]NoticeDeliverySnapshot) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys=ON`); err != nil {
		return err
	}
	for _, table := range []string{"notice_notifications", "approvals", "events", "messages", "notice_delivery_receipts", "notices", "decisions", "decision_notice_cursors", "signals", "transitions", "intents", "mounts", "project_runtime", "lead_start_claims", "task_launch_identities", "pr_body_markers", "task_repos", "project_repos", "repo_watch_state", "pr_observations", "project_watch_state"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE tasks SET reviews_task_id=NULL"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM tasks"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM projects"); err != nil {
		return err
	}
	for _, project := range projects {
		kind := project.Kind
		if kind == "" {
			kind = ProjectKindRepo
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO projects(id,name,root,default_branch,kind,herdr_workspace_id,lead_pane_id,lead_label,lead_absent_since,status,created_at,last_activity_at,lead_launches,down_at,project_uuid) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			project.ID, project.Name, project.Root, project.DefaultBranch, kind, project.HerdrWorkspaceID, project.LeadPaneID, project.LeadLabel, project.LeadAbsentSince, project.Status, project.CreatedAt, project.LastActivityAt, project.LeadLaunches, project.DownAt, project.UUID); err != nil {
			return err
		}
		for _, member := range members[project.ID] {
			if _, err := tx.ExecContext(ctx, `INSERT INTO project_repos(project_id,name,path,default_branch,status,created_at,updated_at,origin_host) VALUES(?,?,?,?,?,?,?,?)`, project.ID, member.Name, member.Path, member.DefaultBranch, member.Status, project.CreatedAt, project.LastActivityAt, member.OriginHost); err != nil {
				return err
			}
		}
	}
	for _, snapshot := range snapshots {
		task := snapshot.Task
		var reviewed any
		if task.ReviewsTaskID != 0 {
			reviewed = task.ReviewsTaskID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(id,project_id,seq,type,reviews_task_id,title,short_name,state,profile,dispatch_rule,landing_mode,autonomy_review,autonomy_land,branch,base_ref,worktree_path,herdr_workspace_id,pane_id,pane_label,agent_name,agent_session,pr_url,landed_ref,last_output_hash,last_worktree_hash,last_progress_at,agent_absent_since,idle_since,launches,gated_sha,created_at,updated_at,mount_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			task.ID, task.ProjectID, task.Seq, task.Type, reviewed, task.Title, task.ShortName, task.State, task.Profile, task.DispatchRule, task.LandingMode, task.AutonomyReview, task.AutonomyLand, task.Branch, task.BaseRef, task.WorktreePath, task.HerdrWorkspaceID, task.PaneID, task.PaneLabel, task.AgentName, task.AgentSession, task.PRURL, task.LandedRef, task.LastOutputHash, task.LastWorktreeHash, task.LastProgressAt, task.AgentAbsentSince, task.IdleSince, task.Launches, task.GatedSHA, task.CreatedAt, task.UpdatedAt, nullableID(task.MountID)); err != nil {
			return err
		}
		for _, marker := range snapshot.PRBodyMarkers {
			if marker.TaskID != 0 && marker.TaskID != task.ID {
				return fmt.Errorf("PR body marker in snapshot for t%d belongs to Task %d", task.Seq, marker.TaskID)
			}
			if marker.Token == "" {
				return fmt.Errorf("empty PR body marker token in snapshot for t%d", task.Seq)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO pr_body_markers(task_id,repo,pr_url,marker_token,updated_at) VALUES(?,?,?,?,?)`, task.ID, marker.Repo, marker.PRURL, marker.Token, marker.UpdatedAt); err != nil {
				return err
			}
		}
		for _, identity := range snapshot.LaunchIdentities {
			if identity.TaskID != 0 && identity.TaskID != task.ID {
				return fmt.Errorf("Task launch identity in snapshot for t%d belongs to Task %d", task.Seq, identity.TaskID)
			}
			if identity.LaunchNumber < 1 || identity.LaunchNumber > task.Launches {
				return fmt.Errorf("invalid launch number %d in snapshot for t%d", identity.LaunchNumber, task.Seq)
			}
			modelKnown := identity.ModelKnown && strings.TrimSpace(identity.ConfiguredModel) != ""
			if _, err := tx.ExecContext(ctx, `INSERT INTO task_launch_identities(task_id,launch_number,profile_name,configured_model,model_known) VALUES(?,?,?,?,?)`, task.ID, identity.LaunchNumber, identity.Profile, identity.ConfiguredModel, modelKnown); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO transitions(task_id,from_state,to_state,source,note,at) VALUES(?,?,?,?,?,?)`, task.ID, "", task.State, "cli", "Rebuilt from atomic Task snapshot", task.UpdatedAt); err != nil {
			return err
		}
		for _, repo := range snapshot.TaskRepos {
			if _, err := tx.ExecContext(ctx, `INSERT INTO task_repos(task_id,repo,worktree_path,base_ref,landing_mode,state,gated_sha,pr_url,landed_ref,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, task.ID, repo.Repo, repo.WorktreePath, repo.BaseRef, repo.LandingMode, repo.State, repo.GatedSHA, repo.PRURL, repo.LandedRef, task.UpdatedAt); err != nil {
				return err
			}
		}
	}
	for projectID, state := range projectStates {
		for _, mount := range state.Mounts {
			if _, err := tx.ExecContext(ctx, `INSERT INTO mounts(id,project_id,n,path,state,task_id,acquired_at,released_at) VALUES(?,?,?,?,?,?,?,?)`,
				mount.ID, mount.ProjectID, mount.Number, mount.Path, mount.State, nullableID(mount.TaskID), mount.AcquiredAt, mount.ReleasedAt); err != nil {
				return err
			}
		}
		for _, decision := range state.Decisions {
			options, err := json.Marshal(decision.Options)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO decisions(id,project_id,task_id,origin,question,options_json,answer,user_quote,created_at,answered_at,kind,task_launches,obsolete_at,obsolete_reason) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				decision.ID, decision.ProjectID, decision.TaskID, decision.Origin, decision.Question, string(options), decision.Answer, decision.UserQuote,
				decision.CreatedAt, decision.AnsweredAt, decision.Kind, decision.TaskLaunches, decision.ObsoleteAt, decision.ObsoleteReason); err != nil {
				return err
			}
		}
		for _, notice := range state.Notices {
			if _, err := tx.ExecContext(ctx, `INSERT INTO notices(id,project_id,task_id,kind,summary,data_json,created_at,delivered_at,acked_at) VALUES(?,?,?,?,?,?,?,?,?)`,
				notice.ID, notice.ProjectID, nullableID(notice.TaskID), notice.Kind, notice.Summary, notice.DataJSON, notice.CreatedAt,
				nullableTime(notice.DeliveredAt), nullableTime(notice.AckedAt)); err != nil {
				return err
			}
		}
		if state.CapturedAt > 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO decision_notice_cursors(project_id,last_notice_id) VALUES(?,?)`, projectID, state.DecisionNoticeCursor); err != nil {
				return err
			}
		}
	}
	for projectID, snapshot := range deliverySnapshots {
		project := projects[projectID]
		if err := db.RestoreNoticeDeliverySnapshot(ctx, tx, snapshot, project, projectStates[projectID].CapturedAt); err != nil {
			return fmt.Errorf("restore Notice delivery snapshot for Project %d: %w", projectID, err)
		}
	}
	return tx.Commit()
}

func nullableTime(value int64) sql.NullInt64 {
	if value == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: value, Valid: true}
}
