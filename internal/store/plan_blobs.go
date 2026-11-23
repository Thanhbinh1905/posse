package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thanhbinh1905/posse/internal/taskplan"
)

const planInputBlobThreshold = 64 << 10

type taskPlanCacheKey struct {
	taskID   int64
	revision int
}

type cachedTaskPlan struct {
	jsonDigest [32]byte
	plan       *taskplan.Plan
	size       int64
}

const taskPlanCacheByteLimit = 128 << 20

func storedPlanRevision(data string) int {
	var plan struct {
		Revision int `json:"revision"`
	}
	_ = json.Unmarshal([]byte(data), &plan)
	return plan.Revision
}

func storedWorkflowRunID(data string) string {
	var plan struct {
		Workflow *struct {
			RunID string `json:"run_id"`
		} `json:"workflow"`
	}
	if json.Unmarshal([]byte(data), &plan) != nil || plan.Workflow == nil {
		return ""
	}
	return plan.Workflow.RunID
}

func (db *DB) externalizePlanJSON(data string) (string, error) {
	var stored taskplan.Plan
	if err := json.Unmarshal([]byte(data), &stored); err != nil {
		return "", err
	}
	existingBlobs := make([]bool, len(stored.Inputs))
	hasExistingBlobs := false
	needsExternalization := false
	for i := range stored.Inputs {
		if stored.Inputs[i].Path == "" {
			needsExternalization = needsExternalization || len(stored.Inputs[i].Bytes) > planInputBlobThreshold
			continue
		}
		existingBlobs[i] = true
		hasExistingBlobs = true
		inputBytes, err := db.readPlanBlob(stored.Inputs[i])
		if err != nil {
			return "", err
		}
		stored.Inputs[i].Bytes = inputBytes
		stored.Inputs[i].Path = ""
	}
	var plan taskplan.Plan
	var err error
	if hasExistingBlobs {
		hydratedJSON, marshalErr := json.Marshal(stored)
		if marshalErr != nil {
			return "", marshalErr
		}
		plan, err = taskplan.Decode(string(hydratedJSON))
	} else {
		plan, err = taskplan.Decode(data)
	}
	if err != nil {
		return "", err
	}
	if !needsExternalization {
		return data, nil
	}
	plan.AdmittedSHA256 = ""
	digest := plan.SHA256()
	for i := range plan.Inputs {
		input := &plan.Inputs[i]
		if input.Role == "brief" || len(input.Bytes) <= planInputBlobThreshold && !existingBlobs[i] {
			continue
		}
		path, err := db.writePlanBlob(input.SHA256, input.Bytes)
		if err != nil {
			return "", err
		}
		input.Path = path
		input.Bytes = nil
	}
	plan.AdmittedSHA256 = digest
	planInputsJSON, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	return string(planInputsJSON), nil
}

func (db *DB) writePlanBlob(digest string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != digest {
		return "", fmt.Errorf("Task plan input digest mismatch")
	}
	relative := filepath.ToSlash(filepath.Join("blobs", "sha256", digest[:2], digest))
	path := filepath.Join(filepath.Dir(db.Path), filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create Task plan blob directory: %w", err)
	}
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) != string(data) {
			return "", fmt.Errorf("Task plan blob %s has different content", digest)
		}
		return relative, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".blob-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Link(temporaryPath, path); err != nil && !os.IsExist(err) {
		return "", err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return "", err
	}
	return relative, nil
}

func (db *DB) readPlanBlob(input taskplan.Input) ([]byte, error) {
	if input.Path == "" || filepath.IsAbs(input.Path) || filepath.Clean(filepath.FromSlash(input.Path)) != filepath.FromSlash(input.Path) {
		return nil, fmt.Errorf("invalid Task plan input path")
	}
	path := filepath.Join(filepath.Dir(db.Path), filepath.FromSlash(input.Path))
	relative, err := filepath.Rel(filepath.Dir(db.Path), path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("Task plan input path escapes POSSE_HOME")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Task plan blob %s: %w", input.SHA256, err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != input.SHA256 {
		return nil, fmt.Errorf("Task plan blob %s failed digest verification", input.SHA256)
	}
	return data, nil
}

func clonePlan(plan *taskplan.Plan) *taskplan.Plan {
	copy := *plan
	copy.Inputs = append([]taskplan.Input(nil), plan.Inputs...)
	for i := range copy.Inputs {
		copy.Inputs[i].Bytes = append([]byte(nil), plan.Inputs[i].Bytes...)
	}
	copy.Dependencies = append(copy.Dependencies[:0:0], plan.Dependencies...)
	copy.Grants = append(copy.Grants[:0:0], plan.Grants...)
	copy.Preferences = append(copy.Preferences[:0:0], plan.Preferences...)
	copy.MemberAdmissions = append(copy.MemberAdmissions[:0:0], plan.MemberAdmissions...)
	copy.Execution.Members = append(copy.Execution.Members[:0:0], plan.Execution.Members...)
	for i := range copy.Execution.Members {
		if selected := copy.Execution.Members[i].DeleteBranchOnMerge; selected != nil {
			value := *selected
			copy.Execution.Members[i].DeleteBranchOnMerge = &value
		}
	}
	copy.Execution.Paths = append(copy.Execution.Paths[:0:0], plan.Execution.Paths...)
	copy.Launch.Args = append(copy.Launch.Args[:0:0], plan.Launch.Args...)
	copy.Launch.Profile.Args = append(copy.Launch.Profile.Args[:0:0], plan.Launch.Profile.Args...)
	copy.Pool.Setup = append(copy.Pool.Setup[:0:0], plan.Pool.Setup...)
	if plan.UserChoice != nil {
		choice := *plan.UserChoice
		choice.Fields = append([]string(nil), plan.UserChoice.Fields...)
		copy.UserChoice = &choice
	}
	if plan.ValidationLoops != nil {
		loops := *plan.ValidationLoops
		copy.ValidationLoops = &loops
	}
	if plan.Workflow != nil {
		workflow := *plan.Workflow
		workflow.Profile.Stages = append(workflow.Profile.Stages[:0:0], plan.Workflow.Profile.Stages...)
		copy.Workflow = &workflow
	}
	if plan.DeleteBranchOnMerge != nil {
		value := *plan.DeleteBranchOnMerge
		copy.DeleteBranchOnMerge = &value
	}
	return &copy
}

func taskPlanMemorySize(plan *taskplan.Plan) int64 {
	size := int64(1 << 10)
	for _, input := range plan.Inputs {
		size += int64(len(input.Bytes))
	}
	return size
}

func (db *DB) cacheTaskPlan(key taskPlanCacheKey, digest [32]byte, plan *taskplan.Plan) {
	if db.planCache == nil {
		db.planCache = make(map[taskPlanCacheKey]cachedTaskPlan)
	}
	size := taskPlanMemorySize(plan)
	for cachedKey, cached := range db.planCache {
		if cachedKey.taskID == key.taskID && cachedKey.revision != key.revision {
			db.planCacheBytes -= cached.size
			delete(db.planCache, cachedKey)
		}
	}
	if cached := db.planCache[key]; cached.plan != nil {
		db.planCacheBytes -= cached.size
		delete(db.planCache, key)
	}
	if size > taskPlanCacheByteLimit {
		return
	}
	for db.planCacheBytes+size > taskPlanCacheByteLimit {
		for cachedKey, cached := range db.planCache {
			db.planCacheBytes -= cached.size
			delete(db.planCache, cachedKey)
			break
		}
	}
	db.planCache[key] = cachedTaskPlan{jsonDigest: digest, plan: plan, size: size}
	db.planCacheBytes += size
}

func (db *DB) loadAdmittedPlan(taskID int64, data string, revisions ...int) (*taskplan.Plan, error) {
	revision := 0
	if len(revisions) > 0 {
		revision = revisions[0]
	}
	if revision < 1 {
		revision = storedPlanRevision(data)
	}
	if data == "" && revision < 1 {
		if err := db.QueryRow(`SELECT revision,plan_json FROM task_plan_revisions WHERE task_id=? ORDER BY revision DESC LIMIT 1`, taskID).Scan(&revision, &data); err != nil {
			if err == sql.ErrNoRows {
				return nil, nil
			}
			return nil, fmt.Errorf("load latest Task plan revision: %w", err)
		}
	}
	key := taskPlanCacheKey{taskID: taskID, revision: revision}
	if data == "" {
		db.planCacheMu.Lock()
		if cached := db.planCache[key]; cached.plan != nil {
			db.planCacheMu.Unlock()
			return clonePlan(cached.plan), nil
		}
		db.planCacheMu.Unlock()
		if err := db.QueryRow(`SELECT plan_json FROM task_plan_revisions WHERE task_id=? AND revision=?`, taskID, revision).Scan(&data); err != nil {
			if err == sql.ErrNoRows {
				return nil, nil
			}
			return nil, fmt.Errorf("load Task plan revision %d: %w", revision, err)
		}
	}
	jsonDigest := sha256.Sum256([]byte(data))
	db.planCacheMu.Lock()
	if cached := db.planCache[key]; cached.plan != nil && cached.jsonDigest == jsonDigest {
		db.planCacheMu.Unlock()
		return clonePlan(cached.plan), nil
	}
	db.planCacheMu.Unlock()
	var stored taskplan.Plan
	if err := json.Unmarshal([]byte(data), &stored); err != nil {
		return nil, fmt.Errorf("decode execution plan: %w", err)
	}
	admittedDigest := stored.AdmittedSHA256
	stored.AdmittedSHA256 = ""
	for i := range stored.Inputs {
		if stored.Inputs[i].Path == "" {
			continue
		}
		inputBytes, err := db.readPlanBlob(stored.Inputs[i])
		if err != nil {
			return nil, err
		}
		stored.Inputs[i].Bytes = inputBytes
		stored.Inputs[i].Path = ""
	}
	hydratedJSON, err := json.Marshal(stored)
	if err != nil {
		return nil, err
	}
	plan, err := taskplan.Decode(string(hydratedJSON))
	if err != nil {
		return nil, err
	}
	if admittedDigest != "" && plan.Version == taskplan.Version && plan.SHA256() != admittedDigest {
		return nil, fmt.Errorf("Task plan digest does not match its admitted digest")
	}
	plan.AdmittedSHA256 = admittedDigest
	db.planCacheMu.Lock()
	if cached := db.planCache[key]; cached.plan != nil && cached.jsonDigest == jsonDigest {
		plan = *cached.plan
	} else {
		db.cacheTaskPlan(key, jsonDigest, &plan)
	}
	db.planCacheMu.Unlock()
	return clonePlan(&plan), nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
