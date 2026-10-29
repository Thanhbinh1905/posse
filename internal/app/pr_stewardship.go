package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/runtime"
	"github.com/thanhbinh1905/posse/internal/store"
)

// The Lead stewards every PR/MR. Stewardship covers only the forge operations
// below: inspection, replies, refreshing Posse-owned metadata and rerunning an
// existing failed CI run on the current head. It never grants merge, branch,
// workflow or secret authority; those keep their own commands and approvals.

// stewardRerunLimit bounds steward reruns of one CI run on one PR head.
const stewardRerunLimit = 2

const stewardReplyMaxBytes = 32 * 1024

type stewardTarget struct {
	project   store.Project
	cfg       config.Config
	task      store.Task
	member    string
	prURL     string
	delivery  store.Delivery
	forge     repositoryForge
	actor     string
	actorPane string
}

func (target stewardTarget) taskLabel() string {
	label := taskIDString(target.task.Seq)
	if target.member != "" {
		label += " --repo " + target.member
	}
	return label
}

func (target stewardTarget) operation(name, head, subject, baseline string) store.DeliveryOperation {
	return store.DeliveryOperation{DeliveryID: target.delivery.ID, Operation: name, Actor: target.actor, ActorPane: target.actorPane, HeadSHA: head, Subject: subject, Baseline: baseline}
}

func (s *Service) stewardCommand(ctx *axi.Context, name string, args []string, specs map[string]flagSpec, run func(*store.DB, stewardTarget, parsedArgs) error) error {
	if specs == nil {
		specs = map[string]flagSpec{}
	}
	specs["repo"] = flagSpec{}
	parsed, err := parseArgs("pr "+name, args, specs)
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("pr " + name + " requires one Task id")
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if err := s.requireLead(ctx.Context, db, project); err != nil {
		return err
	}
	target, err := s.resolveStewardTarget(ctx.Context, db, project, parsed.Positionals[0], parsed.Flags["repo"])
	if err != nil {
		return err
	}
	return run(db, target, parsed)
}

func (s *Service) resolveStewardTarget(ctx context.Context, db *store.DB, project store.Project, identifier, member string) (stewardTarget, error) {
	task, err := s.currentTask(ctx, db, project, identifier)
	if err != nil {
		return stewardTarget{}, err
	}
	if task.Type != "change" {
		return stewardTarget{}, axi.Failure("pr_delivery_missing", "only a Change Task has a PR/MR to steward", false)
	}
	prURL := task.PRURL
	if project.IsWorkspace() {
		if member == "" {
			return stewardTarget{}, axi.Failure("pr_delivery_missing", "a workspace Task needs --repo <member> to name its PR/MR", false)
		}
		repos, err := db.TaskRepos(ctx, task.ID)
		if err != nil {
			return stewardTarget{}, err
		}
		prURL = ""
		for _, repo := range repos {
			if repo.Repo == member {
				prURL = repo.PRURL
			}
		}
	} else if member != "" {
		return stewardTarget{}, axi.Failure("pr_delivery_missing", "--repo applies only to workspace Projects", false)
	}
	if prURL == "" {
		return stewardTarget{}, axi.Failure("pr_delivery_missing", "Task "+taskIDString(task.Seq)+" has no recorded PR/MR", false, "Wait for the Worker to run `posse publish`")
	}
	cfg, err := s.prepareProject(ctx, db, project)
	if err != nil {
		return stewardTarget{}, err
	}
	root := project.Root
	if member != "" {
		target, err := projectTarget(ctx, db, project, member)
		if err != nil {
			return stewardTarget{}, err
		}
		root = target.Root
	}
	forge, err := forgeForRepository(ctx, root, cfg, member)
	if err != nil {
		return stewardTarget{}, err
	}
	// A PR URL recorded without publish predates delivery records.
	delivery, err := db.EnsureDelivery(ctx, task.ID, member, prURL, "legacy")
	if err != nil {
		return stewardTarget{}, err
	}
	actor := s.configCaller(ctx, db)
	role := "user"
	if actor.role == "lead" {
		role = "lead"
	}
	return stewardTarget{project: project, cfg: cfg, task: task, member: member, prURL: prURL, delivery: delivery, forge: forge, actor: role, actorPane: actor.paneID}, nil
}

// observeDelivery refreshes the Project's PR watch and returns the PR's
// observation. current is false when the watch did not confirm it from the
// forge during this call, so cached state is never presented as current.
func (s *Service) observeDelivery(ctx context.Context, db *store.DB, target stewardTarget) (store.PRObservation, bool, error) {
	started := time.Now().UnixMilli()
	deadline := time.Now().Add(runtime.ReconcileBudget)
	for attempt := 0; ; attempt++ {
		pollCtx, cancel := context.WithDeadline(ctx, deadline)
		_ = s.pollProjectPullRequests(pollCtx, db, target.project, target.cfg, true)
		cancel()
		observation, err := latestDeliveryObservation(ctx, db, target)
		if err != nil {
			return store.PRObservation{}, false, err
		}
		if observation.ID != 0 {
			confirmed, err := db.PRObservationConfirmedAt(ctx, observation.ID)
			if err != nil {
				return store.PRObservation{}, false, err
			}
			if confirmed >= started {
				return observation, true, nil
			}
		}
		// Another poll may hold the Project claim; give it a moment to finish.
		if attempt >= 2 || time.Now().Add(300*time.Millisecond).After(deadline) {
			return observation, false, nil
		}
		select {
		case <-ctx.Done():
			return observation, false, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// latestDeliveryObservation returns the delivery's latest observation, or a
// zero observation when none was recorded for its PR URL.
func latestDeliveryObservation(ctx context.Context, db *store.DB, target stewardTarget) (store.PRObservation, error) {
	var observation store.PRObservation
	var err error
	if target.member == "" {
		observation, err = db.LatestPRObservation(ctx, target.task.ID)
	} else {
		observation, err = db.LatestMemberPRObservation(ctx, target.task.ID, target.member)
	}
	if store.IsNotFound(err) || err == nil && observation.PRURL != target.prURL {
		return store.PRObservation{}, nil
	}
	return observation, err
}

func (s *Service) watchFailureReason(ctx context.Context, db *store.DB, target stewardTarget) string {
	failures, err := db.PRTaskWatchFailuresForTask(ctx, target.task.ID)
	if err != nil {
		return ""
	}
	for _, failure := range failures {
		if failure.Repo == target.member && failure.PRURL == target.prURL && failure.LastError != "" {
			return failure.LastError
		}
	}
	return ""
}

func (s *Service) forgeUnavailable(ctx context.Context, db *store.DB, target stewardTarget, observation store.PRObservation) error {
	message := "could not read the current PR/MR state from the forge"
	reasons := make([]string, 0, 2)
	if reason, err := db.PRPollLastError(ctx, target.project.ID); err == nil && reason != "" {
		reasons = append(reasons, reason)
	}
	if reason := s.watchFailureReason(ctx, db, target); reason != "" {
		reasons = append(reasons, reason)
	}
	if len(reasons) > 0 {
		message += ": " + strings.Join(reasons, "; ")
	}
	if observation.ID != 0 {
		message += fmt.Sprintf("; the last observation is head %s (%s) from %s", observation.HeadSHA, observation.State, time.UnixMilli(observation.ObservedAt).Local().Format(time.RFC3339))
	}
	return axi.Failure("forge_unavailable", message, true, "Retry `posse pr inspect "+target.taskLabel()+"` when the forge is reachable; no forge effect was attempted")
}

// currentOpenDelivery is the precondition of every effect: a current
// observation of an open PR/MR.
func (s *Service) currentOpenDelivery(ctx context.Context, db *store.DB, target stewardTarget) (store.PRObservation, error) {
	if target.task.State == store.StateMerged || target.task.State == store.StateTornDown {
		return store.PRObservation{}, axi.Failure("pr_delivery_closed", "Task "+taskIDString(target.task.Seq)+" is "+string(target.task.State)+"; its PR/MR is no longer stewarded", false)
	}
	observation, current, err := s.observeDelivery(ctx, db, target)
	if err != nil {
		return store.PRObservation{}, err
	}
	if !current {
		return store.PRObservation{}, s.forgeUnavailable(ctx, db, target, observation)
	}
	if observation.State != "OPEN" {
		return store.PRObservation{}, axi.Failure("pr_delivery_closed", "PR/MR "+target.prURL+" is "+strings.ToLower(observation.State), false)
	}
	return observation, nil
}

func (s *Service) prInspect(ctx *axi.Context, args []string) error {
	return s.stewardCommand(ctx, "inspect", args, nil, func(db *store.DB, target stewardTarget, _ parsedArgs) error {
		if target.task.State == store.StateMerged || target.task.State == store.StateTornDown {
			// The watch no longer polls a finished Task; show its final record.
			observation, err := latestDeliveryObservation(ctx.Context, db, target)
			if err != nil {
				return err
			}
			return s.printDelivery(ctx, db, target, observation)
		}
		observation, current, err := s.observeDelivery(ctx.Context, db, target)
		if err != nil {
			return err
		}
		outcome, detail := "succeeded", ""
		if !current {
			outcome, detail = "failed", "forge observation unavailable"
			if reason := s.watchFailureReason(ctx.Context, db, target); reason != "" {
				detail += ": " + reason
			}
		}
		operation := target.operation("inspect", observation.HeadSHA, "", "")
		operation.Outcome, operation.Detail = outcome, detail
		if _, err := db.RecordDeliveryOperation(ctx.Context, operation); err != nil {
			return err
		}
		if !current {
			return s.forgeUnavailable(ctx.Context, db, target, observation)
		}
		return s.printDelivery(ctx, db, target, observation)
	})
}

func (s *Service) printDelivery(ctx *axi.Context, db *store.DB, target stewardTarget, observation store.PRObservation) error {
	checks := decodeChecks(observation.Checks)
	failures := checks.Failures
	if target.forge.Kind == "gitlab" && len(failures) > 0 {
		if expanded, err := expandGitLabFailureChecks(ctx.Context, target.forge, failures); err == nil {
			failures = expanded
		}
	}
	failureRows := make([]any, 0, len(failures))
	for _, failure := range failures {
		failureRows = append(failureRows, axi.Object{{Key: "name", Value: failure.Name}, {Key: "run", Value: failure.RunID}, {Key: "job", Value: failure.JobID}, {Key: "cancelled", Value: failure.Cancelled}, {Key: "url", Value: failure.URL}})
	}
	identities, err := db.TaskLaunchIdentities(ctx.Context, target.task.ID)
	if err != nil {
		return err
	}
	authors := make([]any, 0, len(identities))
	for _, identity := range identities {
		model := identity.ConfiguredModel
		if !identity.ModelKnown {
			model = "unknown"
		}
		authors = append(authors, axi.Object{{Key: "task", Value: taskIDString(target.task.Seq)}, {Key: "launch", Value: identity.LaunchNumber}, {Key: "profile", Value: defaultValue(identity.Profile, "unknown")}, {Key: "model", Value: model}})
	}
	if len(authors) == 0 {
		authors = append(authors, axi.Object{{Key: "task", Value: taskIDString(target.task.Seq)}, {Key: "launch", Value: 0}, {Key: "profile", Value: "unknown"}, {Key: "model", Value: "unknown"}})
	}
	operations, err := db.DeliveryOperations(ctx.Context, target.delivery.ID, 10)
	if err != nil {
		return err
	}
	operationRows := make([]any, 0, len(operations))
	for _, operation := range operations {
		operationRows = append(operationRows, axi.Object{{Key: "id", Value: operation.ID}, {Key: "operation", Value: operation.Operation}, {Key: "actor", Value: operation.Actor}, {Key: "head", Value: shortSHA(operation.HeadSHA)}, {Key: "subject", Value: operation.Subject}, {Key: "outcome", Value: operation.Outcome}, {Key: "detail", Value: operation.Detail}, {Key: "at", Value: operation.UpdatedAt}})
	}
	var confirmed int64
	if observation.ID != 0 {
		if confirmed, err = db.PRObservationConfirmedAt(ctx.Context, observation.ID); err != nil {
			return err
		}
	}
	help := []any{}
	if observation.State == "OPEN" && len(failures) > 0 {
		help = append(help, "For an infrastructure or flaky failure, run `posse pr rerun "+target.taskLabel()+" "+rerunFlag(target.forge)+" <id>` once; send code failures to the Worker with `posse send`")
	}
	help = append(help, "Run `posse pr reply "+target.taskLabel()+" --body <text>` to answer a reviewer on the PR/MR")
	return ctx.Print(axi.Object{
		{Key: "delivery", Value: axi.Row{{Key: "task", Value: taskIDString(target.task.Seq)}, {Key: "repo", Value: target.member}, {Key: "url", Value: target.prURL}, {Key: "steward", Value: target.delivery.Steward}, {Key: "origin", Value: target.delivery.Origin}}},
		{Key: "authors", Value: authors},
		{Key: "observation", Value: axi.Row{{Key: "head", Value: observation.HeadSHA}, {Key: "state", Value: observation.State}, {Key: "checks", Value: checks.State}, {Key: "review", Value: observation.Review}, {Key: "mergeable", Value: observation.Mergeable}, {Key: "confirmed_at", Value: confirmed}}},
		{Key: "failures", Value: failureRows},
		{Key: "operations", Value: operationRows},
		{Key: "help", Value: help},
	})
}

func rerunFlag(forge repositoryForge) string {
	if forge.Kind == "gitlab" {
		return "--job"
	}
	return "--run"
}

func stewardOperationMarker(id int64) string {
	return "<!-- posse:steward-operation:" + strconv.FormatInt(id, 10) + " -->"
}

func (s *Service) prReply(ctx *axi.Context, args []string) error {
	return s.stewardCommand(ctx, "reply", args, map[string]flagSpec{"body": {}}, func(db *store.DB, target stewardTarget, parsed parsedArgs) error {
		body := strings.TrimSpace(parsed.Flags["body"])
		if body == "" {
			return axi.Usage("pr reply requires --body <text>")
		}
		if len(body) > stewardReplyMaxBytes {
			return axi.Usage(fmt.Sprintf("pr reply --body is limited to %d bytes", stewardReplyMaxBytes))
		}
		observation, err := s.currentOpenDelivery(ctx.Context, db, target)
		if err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(body))
		subject := "body:" + hex.EncodeToString(digest[:8])
		// A reply is keyed by its text across heads: the same text is posted once.
		if applied, err := s.settleUnresolvedOperation(ctx.Context, db, target, "reply", "", subject, func(previous store.DeliveryOperation) (bool, error) {
			return forgeCommentHasMarker(ctx.Context, target, stewardOperationMarker(previous.ID))
		}); err != nil {
			return err
		} else if applied != nil {
			return printOperation(ctx, target, *applied, "observed", "The earlier reply is on the PR/MR; it was not posted again")
		}
		operation, err := db.BeginDeliveryOperation(ctx.Context, target.operation("reply", observation.HeadSHA, subject, ""), "", 1)
		if err != nil {
			return stewardBeginFailure(err, target, "reply")
		}
		if err := postForgeComment(ctx.Context, target, body+"\n\n"+stewardOperationMarker(operation.ID)); err != nil {
			return uncertainOperation(ctx.Context, db, target, operation, err)
		}
		if err := db.ResolveDeliveryOperation(ctx.Context, operation.ID, "succeeded", ""); err != nil {
			return err
		}
		operation.Outcome = "succeeded"
		return printOperation(ctx, target, operation, "posted", "A reply is not a review pass or a User answer")
	})
}

func (s *Service) prRefresh(ctx *axi.Context, args []string) error {
	return s.stewardCommand(ctx, "refresh", args, nil, func(db *store.DB, target stewardTarget, _ parsedArgs) error {
		observation, err := s.currentOpenDelivery(ctx.Context, db, target)
		if err != nil {
			return err
		}
		title, body, err := s.readForgeMetadata(ctx.Context, target)
		if err != nil {
			return axi.Failure("forge_unavailable", "could not read the PR/MR title and body: "+err.Error(), true, "Retry `posse pr refresh "+target.taskLabel()+"`; no forge effect was attempted")
		}
		wantTitle, wantBody, digest, err := s.desiredForgeMetadata(ctx.Context, db, target, body)
		if err != nil {
			return err
		}
		// Edits set exact values, so a repeat cannot duplicate an effect; an
		// unknown earlier outcome is still settled from the metadata just read.
		if _, err := s.settleUnresolvedOperation(ctx.Context, db, target, "refresh", observation.HeadSHA, "", func(store.DeliveryOperation) (bool, error) {
			return title == wantTitle && body == wantBody, nil
		}); err != nil {
			return err
		}
		if title == wantTitle && body == wantBody {
			operation, err := db.RecordDeliveryOperation(ctx.Context, store.DeliveryOperation{DeliveryID: target.delivery.ID, Operation: "refresh", Actor: target.actor, ActorPane: target.actorPane, HeadSHA: observation.HeadSHA, Outcome: "succeeded", Detail: "already current"})
			if err != nil {
				return err
			}
			if err := recordValidationDigest(ctx.Context, db, target, digest); err != nil {
				return err
			}
			return printOperation(ctx, target, operation, "current", "Posse-owned PR/MR metadata already matches")
		}
		operation, err := db.BeginDeliveryOperation(ctx.Context, target.operation("refresh", observation.HeadSHA, "", ""), observation.HeadSHA, 0)
		if err != nil {
			return stewardBeginFailure(err, target, "refresh")
		}
		if title != wantTitle {
			if err := writeForgeTitle(ctx.Context, target.forge, target.prURL, wantTitle); err != nil {
				return uncertainOperation(ctx.Context, db, target, operation, err)
			}
		}
		if body != wantBody {
			if err := writeForgeBody(ctx.Context, target.forge, target.prURL, wantBody); err != nil {
				return uncertainOperation(ctx.Context, db, target, operation, err)
			}
		}
		if err := recordValidationDigest(ctx.Context, db, target, digest); err != nil {
			return err
		}
		if err := db.ResolveDeliveryOperation(ctx.Context, operation.ID, "succeeded", ""); err != nil {
			return err
		}
		operation.Outcome = "succeeded"
		return printOperation(ctx, target, operation, "refreshed", "Only the title and Posse's Validation block were rewritten; other PR/MR text is unchanged")
	})
}

func (s *Service) prRerun(ctx *axi.Context, args []string) error {
	return s.stewardCommand(ctx, "rerun", args, map[string]flagSpec{"run": {}, "job": {}}, func(db *store.DB, target stewardTarget, parsed parsedArgs) error {
		flag := strings.TrimPrefix(rerunFlag(target.forge), "--")
		id := strings.TrimSpace(parsed.Flags[flag])
		if other := map[string]string{"run": "job", "job": "run"}[flag]; parsed.Flags[other] != "" {
			return axi.Usage("pr rerun on " + target.forge.Kind + " takes --" + flag + " <id>, not --" + other)
		}
		if _, err := strconv.ParseInt(id, 10, 64); err != nil || strings.HasPrefix(id, "-") || id == "0" {
			return axi.Usage("pr rerun requires --" + flag + " <numeric id> of a failed check on the current head")
		}
		observation, err := s.currentOpenDelivery(ctx.Context, db, target)
		if err != nil {
			return err
		}
		failures := decodeChecks(observation.Checks).Failures
		if target.forge.Kind == "gitlab" {
			if failures, err = expandGitLabFailureChecks(ctx.Context, target.forge, failures); err != nil {
				return err
			}
		}
		var failed *failedCheck
		for index := range failures {
			if flag == "run" && failures[index].RunID == id || flag == "job" && failures[index].JobID == id {
				failed = &failures[index]
				break
			}
		}
		subject := flag + ":" + id
		if failed == nil {
			// Settling an earlier rerun needs no forge effect, so do it even
			// when the run is no longer failing.
			if applied, err := s.settleUnresolvedOperation(ctx.Context, db, target, "rerun", observation.HeadSHA, subject, func(previous store.DeliveryOperation) (bool, error) {
				return forgeRerunApplied(ctx.Context, target, id, previous.Baseline)
			}); err == nil && applied != nil {
				return printOperation(ctx, target, *applied, "observed", "The earlier rerun started; it was not requested again")
			}
			return axi.Failure("pr_rerun_refused", "--"+flag+" "+id+" is not a failed check on the current head "+observation.HeadSHA, false, "Run `posse pr inspect "+target.taskLabel()+"` to list failed checks; stewardship reruns only existing failed CI on the current head")
		}
		if applied, err := s.settleUnresolvedOperation(ctx.Context, db, target, "rerun", observation.HeadSHA, subject, func(previous store.DeliveryOperation) (bool, error) {
			return forgeRerunApplied(ctx.Context, target, id, previous.Baseline)
		}); err != nil {
			return err
		} else if applied != nil {
			return printOperation(ctx, target, *applied, "observed", "The earlier rerun started; it was not requested again")
		}
		baseline, err := forgeRerunBaseline(ctx.Context, target, id, observation.HeadSHA)
		if err != nil {
			return err
		}
		operation, err := db.BeginDeliveryOperation(ctx.Context, target.operation("rerun", observation.HeadSHA, subject, baseline), observation.HeadSHA, stewardRerunLimit)
		if err != nil {
			return stewardBeginFailure(err, target, "rerun")
		}
		if err := requestStewardRerun(ctx.Context, target, id, failed.RunID); err != nil {
			return uncertainOperation(ctx.Context, db, target, operation, err)
		}
		if err := db.ResolveDeliveryOperation(ctx.Context, operation.ID, "succeeded", failed.Name); err != nil {
			return err
		}
		operation.Outcome, operation.Detail = "succeeded", failed.Name
		return printOperation(ctx, target, operation, "requested", "Posse reports the rerun result through the PR watch; end your turn")
	})
}

// settleUnresolvedOperation resolves an earlier operation whose outcome is
// unknown before anything is attempted again. It returns the operation when
// the forge shows it took effect. A running or unobservable operation refuses
// the new attempt, so an ambiguous effect is never duplicated.
func (s *Service) settleUnresolvedOperation(ctx context.Context, db *store.DB, target stewardTarget, name, head, subject string, applied func(store.DeliveryOperation) (bool, error)) (*store.DeliveryOperation, error) {
	previous, err := db.UnresolvedDeliveryOperation(ctx, target.delivery.ID, name, head, subject)
	if store.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if previous.Running(time.Now()) {
		return nil, axi.Failure("pr_operation_in_progress", fmt.Sprintf("steward operation %d (%s) is still running", previous.ID, name), true, "Retry after it finishes; Posse will not start the same forge effect twice")
	}
	took, err := applied(previous)
	if err != nil {
		return nil, axi.Failure("forge_outcome_unknown", fmt.Sprintf("steward operation %d (%s) has an unknown outcome and the forge could not be read: %v", previous.ID, name, err), true, "Retry the same command when the forge is reachable; Posse checks the earlier attempt before acting again")
	}
	outcome, detail := "failed", "observed absent on the forge"
	if took {
		outcome, detail = "succeeded", "observed on the forge"
	}
	if err := db.ResolveDeliveryOperation(ctx, previous.ID, outcome, detail); err != nil && !errors.Is(err, store.ErrStateRace) {
		return nil, err
	}
	if !took {
		return nil, nil
	}
	previous.Outcome, previous.Detail = outcome, detail
	return &previous, nil
}

func uncertainOperation(ctx context.Context, db *store.DB, target stewardTarget, operation store.DeliveryOperation, cause error) error {
	detail := boundedLogTail(cause.Error(), 5)
	if err := db.ResolveDeliveryOperation(ctx, operation.ID, "uncertain", detail); err != nil {
		return err
	}
	return axi.Failure("forge_outcome_unknown", fmt.Sprintf("the forge did not confirm steward operation %d (%s): %s", operation.ID, operation.Operation, detail), true, "Run the same command again; Posse checks whether the earlier attempt took effect before acting again")
}

func stewardBeginFailure(err error, target stewardTarget, name string) error {
	switch {
	case errors.Is(err, store.ErrDeliveryOperationLimit) && name == "rerun":
		return axi.Failure("pr_rerun_limit", fmt.Sprintf("this CI run was already rerun %d times on the current head", stewardRerunLimit), false, "Send the failure to the Worker with `posse send "+taskIDString(target.task.Seq)+" <instruction>` or ask the User")
	case errors.Is(err, store.ErrDeliveryOperationLimit):
		return axi.Failure("pr_reply_duplicate", "this exact reply was already posted on the PR/MR", false, "Change the reply text if a new reply is needed")
	case errors.Is(err, store.ErrDeliveryOperationUnresolved):
		return axi.Failure("pr_operation_in_progress", "an earlier "+name+" settled concurrently", true, "Retry the same command")
	}
	return err
}

func printOperation(ctx *axi.Context, target stewardTarget, operation store.DeliveryOperation, result, note string) error {
	return ctx.Print(axi.Object{
		{Key: "operation", Value: axi.Row{{Key: "id", Value: operation.ID}, {Key: "task", Value: taskIDString(target.task.Seq)}, {Key: "repo", Value: target.member}, {Key: "operation", Value: operation.Operation}, {Key: "actor", Value: operation.Actor}, {Key: "head", Value: operation.HeadSHA}, {Key: "subject", Value: operation.Subject}, {Key: "outcome", Value: operation.Outcome}, {Key: "result", Value: result}}},
		{Key: "help", Value: []any{note}},
	})
}

func postForgeComment(ctx context.Context, target stewardTarget, body string) error {
	if target.forge.Kind == "gitlab" {
		number, err := forgeReference(target.prURL, target.forge)
		if err != nil {
			return err
		}
		output, err := commandOutputArgs(ctx, target.forge.Root, "glab", "api", "--hostname", target.forge.Host, "--method", "POST", "--raw-field", "body="+body, "projects/"+url.PathEscape(target.forge.Path)+"/merge_requests/"+strconv.Itoa(number)+"/notes")
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
		}
		return nil
	}
	output, err := commandOutputArgs(ctx, target.forge.Root, "gh", "pr", "comment", target.prURL, "--body", body)
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
	}
	return nil
}

func forgeCommentHasMarker(ctx context.Context, target stewardTarget, marker string) (bool, error) {
	var bodies []string
	if target.forge.Kind == "gitlab" {
		number, err := forgeReference(target.prURL, target.forge)
		if err != nil {
			return false, err
		}
		output, err := commandOutputArgs(ctx, target.forge.Root, "glab", "api", "--hostname", target.forge.Host, "--paginate", "projects/"+url.PathEscape(target.forge.Path)+"/merge_requests/"+strconv.Itoa(number)+"/notes?per_page=100")
		if err != nil {
			return false, fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
		}
		var notes []struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal([]byte(output), &notes); err != nil {
			return false, fmt.Errorf("glab returned invalid merge request notes: %w", err)
		}
		for _, note := range notes {
			bodies = append(bodies, note.Body)
		}
	} else {
		output, err := commandOutputArgs(ctx, target.forge.Root, "gh", "pr", "view", target.prURL, "--json", "comments")
		if err != nil {
			return false, fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
		}
		var pull struct {
			Comments []struct {
				Body string `json:"body"`
			} `json:"comments"`
		}
		if err := json.Unmarshal([]byte(output), &pull); err != nil {
			return false, fmt.Errorf("gh returned invalid pull request comments: %w", err)
		}
		for _, comment := range pull.Comments {
			bodies = append(bodies, comment.Body)
		}
	}
	for _, body := range bodies {
		if strings.Contains(body, marker) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) readForgeMetadata(ctx context.Context, target stewardTarget) (string, string, error) {
	if target.forge.Kind == "gitlab" {
		number, err := forgeReference(target.prURL, target.forge)
		if err != nil {
			return "", "", err
		}
		mr, err := gitlabMR(ctx, target.forge, number)
		if err != nil {
			return "", "", err
		}
		if mr.WebURL != target.prURL {
			return "", "", errors.New("GitLab returned a different merge request")
		}
		return mr.Title, mr.Description, nil
	}
	output, err := commandOutputArgs(ctx, target.forge.Root, "gh", "pr", "view", target.prURL, "--json", "title,body")
	if err != nil {
		return "", "", fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
	}
	var pull struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal([]byte(output), &pull); err != nil {
		return "", "", fmt.Errorf("gh returned invalid pull request metadata: %w", err)
	}
	return pull.Title, pull.Body, nil
}

// desiredForgeMetadata is the Posse-owned PR/MR metadata: the saved or Brief
// title and the current Validation block inside Posse's tokenized span.
func (s *Service) desiredForgeMetadata(ctx context.Context, db *store.DB, target stewardTarget, body string) (string, string, string, error) {
	title := target.task.PRTitle
	if title == "" {
		home, err := s.homePath()
		if err != nil {
			return "", "", "", err
		}
		brief, err := readTaskBrief(home, target.project, target.task)
		if err != nil {
			return "", "", "", err
		}
		title = brief.Title
	}
	marker, err := db.GetPRBodyMarker(ctx, target.task.ID, target.member)
	if store.IsNotFound(err) || err == nil && marker.PRURL != target.prURL {
		return title, body, "", nil
	}
	if err != nil {
		return "", "", "", err
	}
	summary, err := validationSummaryFor(ctx, db, target.task)
	if err != nil {
		return "", "", "", err
	}
	next, ok := replaceValidationBlock(body, marker.Token, summary.markdown())
	if !ok {
		return title, body, "", nil
	}
	return title, next, summary.digest(), nil
}

func recordValidationDigest(ctx context.Context, db *store.DB, target stewardTarget, digest string) error {
	if digest == "" {
		return nil
	}
	_, err := db.ExecContext(ctx, `UPDATE pr_body_markers SET validation_digest=? WHERE task_id=? AND repo=? AND pr_url=?`, digest, target.task.ID, target.member, target.prURL)
	return err
}

func writeForgeTitle(ctx context.Context, forge repositoryForge, prURL, title string) error {
	if forge.Kind == "gitlab" {
		number, err := forgeReference(prURL, forge)
		if err != nil {
			return err
		}
		_, err = commandOutputArgs(ctx, forge.Root, "glab", "api", "--hostname", forge.Host, "--method", "PUT", "--raw-field", "title="+title, "projects/"+url.PathEscape(forge.Path)+"/merge_requests/"+strconv.Itoa(number))
		return err
	}
	_, err := commandOutputArgs(ctx, forge.Root, "gh", "pr", "edit", prURL, "--title", title)
	return err
}

type githubRunState struct {
	Attempt int    `json:"attempt"`
	Status  string `json:"status"`
	HeadSHA string `json:"headSha"`
}

func githubRun(ctx context.Context, target stewardTarget, runID string) (githubRunState, error) {
	ref, err := pullRequestReferenceFromURL(target.prURL)
	if err != nil {
		return githubRunState{}, err
	}
	output, err := commandOutputArgs(ctx, target.forge.Root, "gh", "run", "view", runID, "--repo", githubRepositorySelector(ref), "--json", "attempt,status,headSha")
	if err != nil {
		return githubRunState{}, fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
	}
	var run githubRunState
	if err := json.Unmarshal([]byte(output), &run); err != nil || run.Attempt < 1 {
		return githubRunState{}, fmt.Errorf("gh returned invalid workflow run metadata")
	}
	return run, nil
}

type gitlabJobState struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Pipeline struct {
		ID  int64  `json:"id"`
		SHA string `json:"sha"`
	} `json:"pipeline"`
}

func gitlabJob(ctx context.Context, target stewardTarget, jobID string) (gitlabJobState, error) {
	output, err := gitlabAPI(ctx, target.forge, "jobs/"+jobID)
	if err != nil {
		return gitlabJobState{}, fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
	}
	var job gitlabJobState
	if err := json.Unmarshal([]byte(output), &job); err != nil || job.ID < 1 || job.Pipeline.ID < 1 {
		return gitlabJobState{}, fmt.Errorf("glab returned invalid job metadata")
	}
	return job, nil
}

// forgeRerunBaseline checks that the run belongs to the observed head and has
// settled, and returns what a later observation compares against.
func forgeRerunBaseline(ctx context.Context, target stewardTarget, id, head string) (string, error) {
	unavailable := func(err error) error {
		return axi.Failure("forge_unavailable", "could not read CI run "+id+": "+err.Error(), true, "Retry `posse pr rerun "+target.taskLabel()+" "+rerunFlag(target.forge)+" "+id+"`; no rerun was requested")
	}
	if target.forge.Kind == "gitlab" {
		job, err := gitlabJob(ctx, target, id)
		if err != nil {
			return "", unavailable(err)
		}
		if job.Pipeline.SHA != "" && job.Pipeline.SHA != head {
			return "", axi.Failure("pr_rerun_refused", "job "+id+" ran on "+job.Pipeline.SHA+", not the current head "+head, false)
		}
		if !oneOfString(strings.ToLower(job.Status), "failed", "canceled", "cancelled") {
			return "", axi.Failure("pr_rerun_refused", "job "+id+" is "+job.Status+", not failed", false, "Wait for the PR watch to report the result")
		}
		return strconv.FormatInt(job.Pipeline.ID, 10), nil
	}
	run, err := githubRun(ctx, target, id)
	if err != nil {
		return "", unavailable(err)
	}
	if run.HeadSHA != "" && run.HeadSHA != head {
		return "", axi.Failure("pr_rerun_refused", "run "+id+" ran on "+run.HeadSHA+", not the current head "+head, false)
	}
	if !strings.EqualFold(run.Status, "completed") {
		return "", axi.Failure("pr_rerun_refused", "run "+id+" is "+strings.ToLower(run.Status)+", not completed", false, "Wait for the PR watch to report the result")
	}
	return strconv.Itoa(run.Attempt), nil
}

// forgeRerunApplied reports whether a rerun recorded against baseline took
// effect: GitHub started a later run attempt, or GitLab created a newer job of
// the same name in the pipeline.
func forgeRerunApplied(ctx context.Context, target stewardTarget, id, baseline string) (bool, error) {
	if target.forge.Kind == "gitlab" {
		job, err := gitlabJob(ctx, target, id)
		if err != nil {
			return false, err
		}
		pipeline := baseline
		if pipeline == "" {
			pipeline = strconv.FormatInt(job.Pipeline.ID, 10)
		}
		output, err := gitlabAPI(ctx, target.forge, "pipelines/"+pipeline+"/jobs?include_retried=true&per_page=100")
		if err != nil {
			return false, fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
		}
		var jobs []gitlabPipelineJob
		if err := json.Unmarshal([]byte(output), &jobs); err != nil {
			return false, fmt.Errorf("glab returned invalid pipeline jobs: %w", err)
		}
		for _, candidate := range jobs {
			if candidate.Name == job.Name && candidate.ID > job.ID {
				return true, nil
			}
		}
		return false, nil
	}
	run, err := githubRun(ctx, target, id)
	if err != nil {
		return false, err
	}
	attempt, err := strconv.Atoi(baseline)
	if err != nil {
		return false, fmt.Errorf("recorded rerun baseline %q is invalid", baseline)
	}
	return run.Attempt > attempt || !strings.EqualFold(run.Status, "completed"), nil
}

func requestStewardRerun(ctx context.Context, target stewardTarget, id, runID string) error {
	if target.forge.Kind == "gitlab" {
		return requestCIRerun(ctx, target.forge.Root, target.prURL, target.forge, ciRerunGroup{runID: runID, checks: []failedCheck{{JobID: id}}})
	}
	return requestCIRerun(ctx, target.forge.Root, target.prURL, target.forge, ciRerunGroup{runID: id})
}
