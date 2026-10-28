package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) recordWorkerSignalWithContinuity(
	ctx context.Context,
	db *store.DB,
	project store.Project,
	task store.Task,
	verb, note string,
	data any,
	noticeKind string,
	verdict *store.ReviewVerdictInput,
	inPlace bool,
	resolutions ...*store.ReviewFixResolutionsInput,
) (store.State, error) {
	var reviewResolutions *store.ReviewFixResolutionsInput
	if len(resolutions) > 0 {
		reviewResolutions = resolutions[0]
	}
	record := func(validation *store.ValidationContinuitySnapshot) (store.State, error) {
		if validation != nil {
			if reviewResolutions != nil {
				return db.RecordWorkerSignalWithValidationAndResolutions(ctx, task, verb, note, data, noticeKind, *validation, reviewResolutions)
			}
			return db.RecordWorkerSignalWithValidation(ctx, task, verb, note, data, noticeKind, *validation)
		}
		if reviewResolutions != nil {
			return db.RecordWorkerSignalWithResolutions(ctx, task, verb, note, data, noticeKind, verdict, reviewResolutions)
		}
		return db.RecordWorkerSignalWithVerdict(ctx, task, verb, note, data, noticeKind, verdict)
	}
	if verb != "done" || task.Type != "change" || inPlace || project.IsWorkspace() {
		return record(nil)
	}
	for attempt := 0; attempt < 3; attempt++ {
		validation, err := s.checkedValidationContinuity(ctx, db, project, task)
		if err != nil {
			return "", err
		}
		state, err := record(&validation)
		if !errors.Is(err, store.ErrValidationHeadsChanged) {
			return state, err
		}
	}
	return "", axi.Failure(
		"head_discontinuity",
		"automatic validation instructions changed repeatedly while recording the done Signal",
		true,
		"Retry `posse signal done` after the validation instructions settle",
	)
}

func (s *Service) checkedValidationContinuity(ctx context.Context, db *store.DB, project store.Project, task store.Task) (store.ValidationContinuitySnapshot, error) {
	validation, err := db.ValidationContinuitySnapshot(ctx, task.ID)
	if err != nil {
		return validation, err
	}
	worktreePath := task.WorktreePath
	if fullPipelineSelected(task, "") {
		candidate, candidateErr := db.LatestValidationCandidate(ctx, task.ID, "")
		if candidateErr != nil {
			return validation, candidateErr
		}
		if candidate.State == store.ValidationCandidateActive {
			worktreePath = candidate.WorktreePath
		}
	}
	actualHead, err := gitOutput(ctx, worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return validation, err
	}
	for _, expectedHead := range validation.Heads {
		descends, err := descendsThroughRewrites(ctx, db, worktreePath, task.ID, expectedHead, actualHead)
		if err != nil {
			return validation, err
		}
		if !descends {
			return validation, recordHeadDiscontinuity(ctx, db, project, task, expectedHead, actualHead)
		}
	}
	return validation, nil
}

func recordHeadDiscontinuity(ctx context.Context, db *store.DB, project store.Project, task store.Task, expectedHead, actualHead string) error {
	summary := fmt.Sprintf("%s: Task head %s does not descend from expected head %s", task.Title, actualHead, expectedHead)
	data := marshalJSON(map[string]any{"expected_base_head": expectedHead, "actual_head": actualHead})
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notices WHERE task_id=? AND kind='head_discontinuity' AND data_json=?)`, task.ID, data).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "head_discontinuity", Summary: summary, DataJSON: data}); err != nil {
			return err
		}
	}
	return axi.Failure(
		"head_discontinuity",
		summary,
		false,
		"The Lead must decide whether to restore the expected history or discard the Task",
	)
}
