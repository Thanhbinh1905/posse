package app

import (
	"bytes"
	"context"
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

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/execgroup"
	"github.com/thanhbinh1905/posse/internal/store"
)

type pullRequestReference struct {
	Host   string
	Owner  string
	Repo   string
	Number int
}

type ghPullRequest struct {
	URL            string `json:"url"`
	State          string `json:"state"`
	HeadRefOID     string `json:"headRefOid"`
	Mergeable      string `json:"mergeable"`
	ReviewDecision string `json:"reviewDecision"`
	MergeCommit    *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	StatusCheckRollup *ghStatusCheckRollup `json:"statusCheckRollup"`
}

type ghStatusCheckRollup struct {
	State    string `json:"state"`
	Contexts struct {
		Nodes []map[string]any `json:"nodes"`
	} `json:"contexts"`
}

type ghQueryResponse struct {
	Data struct {
		Repositories map[string]json.RawMessage `json:"-"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
		Path    []any  `json:"path"`
	} `json:"errors"`
}

func (response *ghQueryResponse) UnmarshalJSON(data []byte) error {
	var decoded struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
			Path    []any  `json:"path"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	response.Data.Repositories = decoded.Data
	response.Errors = decoded.Errors
	return nil
}

type prWatchTarget struct {
	Task       store.Task
	Repository string
	Pull       string
}

type failedCheck struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type checkSnapshot struct {
	State    string        `json:"state"`
	Failures []failedCheck `json:"failures"`
}

const (
	openPullRequestHeadAttempts = 8
	openPullRequestHeadDelay    = 250 * time.Millisecond
)

func createPROpenedNotice(ctx context.Context, db *store.DB, project store.Project, task store.Task, prURL, head, summary string) error {
	_, err := db.CreatePROpenedNoticeOnce(ctx, project.ID, task.ID, summary, prURL, head)
	return err
}

func (s *Service) landPullRequest(out *axi.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, merge bool, userQuote string) (returnErr error) {
	ctx := out.Context
	if task.PRURL != "" {
		previous, err := db.LatestPRObservation(ctx, task.ID)
		if err != nil && !store.IsNotFound(err) {
			return err
		}
		if err == nil && previous.PRURL == task.PRURL && previous.State == "CLOSED" {
			return axi.Failure("pr_closed", "pull request closed without merging; resolve its Decision before retrying Land", false)
		}
	}
	var openIntent store.Intent
	openIntentActive := false
	defer func() {
		if openIntentActive {
			if finishErr := db.FinishIntent(ctx, openIntent.ID, openIntent.ProcessID); returnErr == nil && finishErr != nil {
				returnErr = finishErr
			}
		}
	}()
	if merge {
		if task.State != store.StateDone && task.State != store.StateLanding {
			return axi.Failure("land_refused", "Task must be done or landing before Land", false)
		}
		if task.AutonomyLand != "auto" && userQuote == "" {
			return axi.Failure("land_approval_required", "pull request merge needs User approval", false, "Pass `--user-approved \"<User's words>\"` after the User approves")
		}
	}
	if task.State == store.StateDone {
		if task.LandingMode == "no-mistakes" {
			if task.PRURL == "" {
				return axi.Failure("land_refused", "no-mistakes delivery has no pull request URL", false, "Signal completion with `posse holler done <summary> --pr <url>`")
			}
			if err := s.enterNoMistakesLanding(ctx, db, project, task); err != nil {
				return err
			}
			current, err := db.TaskByID(ctx, project.ID, task.ID)
			if err != nil {
				return err
			}
			task = current
		} else {
			// Legacy completions without a Worker-reported PR may reopen a
			// closed PR at Land. Worker-reported URLs must never be silently
			// replaced by a PR that the Worker did not validate.
			if task.LandingMode == "pr" && task.PRURL != "" {
				previous, previousErr := db.LatestPRObservation(ctx, task.ID)
				if previousErr != nil && !store.IsNotFound(previousErr) {
					return previousErr
				}
				if previousErr == nil && previous.PRURL == task.PRURL && previous.State == "CLOSED" {
					signals, err := db.TaskSignals(ctx, task.ID, 20)
					if err != nil {
						return err
					}
					for _, signal := range signals {
						if signal.Verb != "done" {
							continue
						}
						var data map[string]string
						_ = json.Unmarshal([]byte(signal.DataJSON), &data)
						if data["pr_url"] == "" {
							if err := db.UpdateTaskLanding(ctx, task.ID, "", ""); err != nil {
								return err
							}
							task.PRURL = ""
						}
						break
					}
				}
			}
			if task.WorktreePath == "" || task.Branch == "" {
				return axi.Failure("land_refused", "Task has no branch or Mount", false)
			}
			status, err := gitOutput(ctx, task.WorktreePath, "status", "--porcelain")
			if err != nil {
				return err
			}
			if status != "" {
				return axi.Failure("land_refused", "Task worktree is not clean", false, "Commit or discard the remaining changes, then signal done again")
			}
			gatedSHA, err := gitOutput(ctx, task.WorktreePath, "rev-parse", "refs/heads/"+task.Branch)
			if err != nil {
				return err
			}
			workerPR := task.PRURL != ""
			if workerPR {
				forge, err := forgeForRepository(ctx, project.Root, cfg, "")
				if err != nil {
					return err
				}
				if err := validateWorkerPullRequest(ctx, project, task, forge, task.PRURL, gatedSHA); err != nil {
					return err
				}
			}
			if err := db.SetTaskGatedSHA(ctx, task.ID, gatedSHA); err != nil {
				return err
			}
			task.GatedSHA = gatedSHA
			if err := runGates(ctx, cfg, task); err != nil {
				_, noticeErr := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "gate_failed", Summary: task.Title + ": " + truncate(err.Error(), 240), DataJSON: `{}`})
				if noticeErr != nil {
					return noticeErr
				}
				_ = s.deliverNotices(ctx, db, project)
				return axi.Failure("gate_failed", err.Error(), false, "Fix the gate failure on the Task branch, then run `posse send "+taskIDString(task.Seq)+" <instruction>`")
			}
			currentSHA, err := gitOutput(ctx, task.WorktreePath, "rev-parse", "refs/heads/"+task.Branch)
			if err != nil {
				return err
			}
			if currentSHA != gatedSHA {
				if err := db.ClearTaskGatedSHA(ctx, task.ID); err != nil {
					return err
				}
				return axi.Failure("branch_moved", "Task branch moved while the Gate was running", false, "Run `posse land "+taskIDString(task.Seq)+"` again to re-gate")
			}
			forge, err := forgeForRepository(ctx, project.Root, cfg, "")
			if err != nil {
				return err
			}
			if err := validateForgeMergeMethod(forge, cfg); err != nil {
				return err
			}
			if workerPR {
				if err := validateWorkerPullRequest(ctx, project, task, forge, task.PRURL, gatedSHA); err != nil {
					return err
				}
			} else {
				// Compatibility with Ship Tasks completed before Worker publishing existed.
				branchRef := "refs/heads/" + task.Branch
				if _, err := gitOutput(ctx, project.Root, "push", "origin", branchRef+":"+branchRef); err != nil {
					return axi.Failure("pr_push_failed", "could not push the gated Task branch", true, err.Error())
				}
			}
			openedURL := ""
			if task.PRURL == "" {
				openIntent, err = s.startTaskIntent(ctx, db, project.ID, task.ID, "land --open-pr")
				if err != nil {
					return axi.Failure("intent_active", "Task already has an unfinished command", true, err.Error())
				}
				openIntentActive = true
				prURL, _, openErr := s.findOrCreatePullRequest(ctx, db, project, task, openIntent, forge, "", "", "", "", "", false)
				if openErr != nil {
					return openErr
				}
				task.PRURL = prURL
				openedURL = prURL
				if _, err := forgeReference(prURL, forge); err != nil {
					return axi.Failure("pr_create_failed", "forge returned an invalid pull request URL", false, err.Error())
				}
			}
			if err := db.UpdateTaskLanding(ctx, task.ID, task.PRURL, ""); err != nil {
				return err
			}
			if err := db.Transition(ctx, task.ID, store.StateDone, store.StateLanding, "cli", "Pull request is open for review"); err != nil {
				return err
			}
			if openedURL != "" {
				if err := createPROpenedNotice(ctx, db, project, task, openedURL, task.GatedSHA, task.Title+": pull request opened"); err != nil {
					return err
				}
			}
			if openIntentActive {
				if err := db.FinishIntent(ctx, openIntent.ID, openIntent.ProcessID); err != nil {
					return err
				}
				openIntentActive = false
			}
			task.State = store.StateLanding
		}
	}
	if task.State != store.StateLanding {
		return axi.Failure("land_refused", "Task must be done or landing before Land", false, "Run `posse show "+taskIDString(task.Seq)+"` to inspect its state")
	}
	if !merge {
		if err := s.deliverNotices(ctx, db, project); err != nil && !isHerdrUnavailable(err) {
			return err
		}
		return ctxPrintLand(out, task, "Run `posse lookout` to watch this pull request")
	}
	return s.mergePullRequest(out, db, project, cfg, task, userQuote)
}

func (s *Service) findOrCreatePullRequest(ctx context.Context, db *store.DB, project store.Project, task store.Task, intent store.Intent, forge repositoryForge, member, summary, verification, proof, risk string, refreshLegacy bool) (string, bool, error) {
	if forge.Kind == "gitlab" {
		return s.findOrCreateGitLabMR(ctx, db, project, task, intent, forge, member, summary, verification, proof, risk, refreshLegacy)
	}
	args := []string{"pr", "list", "--state", "open", "--base", project.DefaultBranch, "--head", task.Branch, "--json", "url,headRefName,headRefOid", "--limit", "5"}
	var urlValue, lastHead string
	foundOpenPR := false
	for attempt := 0; attempt < openPullRequestHeadAttempts; attempt++ {
		output, err := runOutputStep(ctx, db, intent, "pr.lookup", project.Root, "gh", args...)
		if err != nil {
			return "", false, axi.Failure("pr_list_failed", "could not find an existing open pull request for the Task branch", true, truncate(err.Error(), 1200))
		}
		candidateURL, candidateHead, found, err := openPullRequestForBranch(output, task.Branch)
		if err != nil {
			return "", false, axi.Failure("pr_list_failed", "gh pr list returned invalid pull request data", true, err.Error())
		}
		if !found {
			if !foundOpenPR {
				break
			}
		} else {
			foundOpenPR = true
			urlValue, lastHead = candidateURL, candidateHead
			if candidateHead == task.GatedSHA {
				if err := validatePullRequestOrigin(ctx, project.Root, urlValue); err != nil {
					return "", false, err
				}
				if err := validateWorkerPullRequest(ctx, project, task, forge, urlValue, task.GatedSHA); err != nil {
					return "", false, err
				}
				// Land may adopt an existing PR but has no new Rider metadata to publish.
				if summary != "" {
					if err := s.refreshExistingPullRequest(ctx, db, intent, project, task, forge, urlValue, member, summary, verification, proof, risk, refreshLegacy); err != nil {
						return "", false, err
					}
				}
				return urlValue, false, nil
			}
		}
		if attempt+1 < openPullRequestHeadAttempts {
			timer := time.NewTimer(openPullRequestHeadDelay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return "", false, ctx.Err()
			}
		}
	}
	if foundOpenPR {
		return "", false, pullRequestHeadMismatch(lastHead, task.GatedSHA)
	}
	token, err := db.EnsurePRBodyMarker(ctx, task.ID, member, "")
	if err != nil {
		return "", false, err
	}
	title, body, err := prDetails(ctx, db, project, task, s.homePath, member, summary, verification, proof, risk)
	if err != nil {
		return "", false, err
	}
	created, err := runOutputStep(ctx, db, intent, "pr.create", project.Root, "gh", "pr", "create", "--base", project.DefaultBranch, "--head", task.Branch, "--title", title, "--body", managedPublishBody(body, token))
	if err != nil {
		return "", false, axi.Failure("pr_create_failed", "could not open the pull request", true, truncate(err.Error(), 1200))
	}
	urlValue, err = pullRequestURLFromOutput(created)
	if err != nil {
		return "", false, axi.Failure("pr_create_failed", "gh pr create did not return a pull request URL", true, strings.TrimSpace(created))
	}
	if err := validatePullRequestOrigin(ctx, project.Root, urlValue); err != nil {
		return "", false, err
	}
	if err := db.BindPRBodyMarker(ctx, task.ID, member, urlValue, token); err != nil {
		return "", false, err
	}
	return urlValue, true, nil
}

func (s *Service) refreshExistingPullRequest(ctx context.Context, db *store.DB, intent store.Intent, project store.Project, task store.Task, forge repositoryForge, prURL, member, summary, verification, proof, risk string, refreshLegacy bool) error {
	title, nextBody, err := prDetails(ctx, db, project, task, s.homePath, member, summary, verification, proof, risk)
	if err != nil {
		return err
	}
	var existingTitle, existingBody string
	if forge.Kind == "gitlab" {
		number, err := forgeReference(prURL, forge)
		if err != nil {
			return err
		}
		endpoint := "projects/" + url.PathEscape(forge.Path) + "/merge_requests/" + strconv.Itoa(number)
		output, err := runOutputStep(ctx, db, intent, "pr.refresh.read", forge.Root, "glab", "api", "--hostname", forge.Host, endpoint)
		if err != nil {
			return axi.Failure("pr_refresh_failed", "could not read the existing merge request metadata", true, err.Error())
		}
		var mr gitlabMergeRequest
		if err := json.Unmarshal([]byte(output), &mr); err != nil || mr.WebURL != prURL {
			return axi.Failure("pr_refresh_failed", "GitLab returned invalid merge request metadata", true, output)
		}
		existingTitle, existingBody = mr.Title, mr.Description
	} else {
		output, err := runOutputStep(ctx, db, intent, "pr.refresh.read", forge.Root, "gh", "pr", "view", prURL, "--json", "title,body")
		if err != nil {
			return axi.Failure("pr_refresh_failed", "could not read the existing pull request metadata", true, err.Error())
		}
		var pull struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		if err := json.Unmarshal([]byte(output), &pull); err != nil {
			return axi.Failure("pr_refresh_failed", "gh returned invalid pull request metadata", true, err.Error())
		}
		existingTitle, existingBody = pull.Title, pull.Body
	}
	_, markerErr := db.GetPRBodyMarker(ctx, task.ID, member)
	if store.IsNotFound(markerErr) && hasUnownedTokenizedPublishMarkerPair(existingBody) {
		return missingPRBodyMarkerError()
	}
	if markerErr != nil && !store.IsNotFound(markerErr) {
		return markerErr
	}
	token, err := db.EnsurePRBodyMarker(ctx, task.ID, member, prURL)
	if err != nil {
		return err
	}
	nextBody, err = refreshPublishBody(existingBody, nextBody, token, refreshLegacy)
	if err != nil {
		return err
	}
	if existingTitle == title && existingBody == nextBody {
		return nil
	}
	if forge.Kind == "gitlab" {
		number, _ := forgeReference(prURL, forge)
		endpoint := "projects/" + url.PathEscape(forge.Path) + "/merge_requests/" + strconv.Itoa(number)
		_, err = runOutputStep(ctx, db, intent, "pr.refresh.write", forge.Root, "glab", "api", "--hostname", forge.Host, "--method", "PUT", "--raw-field", "title="+title, "--raw-field", "description="+nextBody, endpoint)
	} else {
		_, err = runOutputStep(ctx, db, intent, "pr.refresh.write", forge.Root, "gh", "pr", "edit", prURL, "--title", title, "--body", nextBody)
	}
	if err != nil {
		return axi.Failure("pr_refresh_failed", "could not refresh Posse-owned pull request metadata", true, err.Error())
	}
	return nil
}

func openPullRequestForBranch(output, branch string) (string, string, bool, error) {
	var pullRequests []struct {
		URL         string `json:"url"`
		HeadRefName string `json:"headRefName"`
		HeadRefOID  string `json:"headRefOid"`
	}
	if err := json.Unmarshal([]byte(output), &pullRequests); err != nil {
		return "", "", false, err
	}
	for _, pull := range pullRequests {
		if pull.HeadRefName != branch {
			continue
		}
		if _, err := pullRequestReferenceFromURL(pull.URL); err != nil {
			return "", "", false, err
		}
		return pull.URL, pull.HeadRefOID, true, nil
	}
	return "", "", false, nil
}

func pullRequestHeadMismatch(actual, expected string) error {
	return axi.Failure("branch_moved", fmt.Sprintf("open pull request for the Task branch has head %s; expected %s", actual, expected), false, "Re-run `posse land` to verify the Task branch tip")
}

func (s *Service) enterNoMistakesLanding(ctx context.Context, db *store.DB, project store.Project, task store.Task) error {
	if err := validatePullRequestOrigin(ctx, project.Root, task.PRURL); err != nil {
		return err
	}
	if task.Branch == "" || task.WorktreePath == "" {
		return axi.Failure("land_refused", "Task has no branch or Mount", false)
	}
	gatedSHA, err := gitOutput(ctx, task.WorktreePath, "rev-parse", "refs/heads/"+task.Branch)
	if err != nil {
		return err
	}
	if err := db.SetTaskGatedSHA(ctx, task.ID, gatedSHA); err != nil {
		return err
	}
	if err := db.UpdateTaskLanding(ctx, task.ID, task.PRURL, ""); err != nil {
		return err
	}
	if err := db.Transition(ctx, task.ID, store.StateDone, store.StateLanding, "cli", "no-mistakes delivered the pull request"); err != nil {
		return err
	}
	return createPROpenedNotice(ctx, db, project, task, task.PRURL, gatedSHA, task.Title+": no-mistakes pull request is ready for review")
}

func (s *Service) mergePullRequest(out *axi.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, userQuote string) (returnErr error) {
	ctx := out.Context
	if task.GatedSHA == "" || task.PRURL == "" {
		task, _ = db.TaskByID(ctx, project.ID, task.ID)
	}
	if task.GatedSHA == "" || task.PRURL == "" {
		return axi.Failure("land_refused", "Task has no recorded pull request or gated commit", false)
	}
	forge, err := forgeForRepository(ctx, project.Root, cfg, "")
	if err != nil {
		return err
	}
	if err := validateForgeMergeMethod(forge, cfg); err != nil {
		return err
	}
	if _, err := forgeReference(task.PRURL, forge); err != nil {
		return axi.Failure("pr_url_invalid", err.Error(), false)
	}
	output := ""
	if forge.Kind == "gitlab" {
		number, _ := forgeReference(task.PRURL, forge)
		mr, viewErr := gitlabMR(ctx, forge, number)
		err = viewErr
		output = marshalJSON(struct {
			HeadRefOID string `json:"headRefOid"`
		}{mr.SHA})
	} else {
		output, err = commandOutputArgs(ctx, project.Root, "gh", "pr", "view", task.PRURL, "--json", "headRefOid")
	}
	if err != nil {
		return axi.Failure("pr_view_failed", "could not verify the pull request head", true, strings.TrimSpace(output))
	}
	var head struct {
		HeadRefOID string `json:"headRefOid"`
	}
	if err := json.Unmarshal([]byte(output), &head); err != nil || head.HeadRefOID == "" {
		return axi.Failure("pr_view_failed", "gh pr view returned no headRefOid", true, strings.TrimSpace(output))
	}
	if head.HeadRefOID != task.GatedSHA {
		if err := db.Transition(ctx, task.ID, store.StateLanding, store.StateDone, "cli", "Pull request head moved after the Gate passed"); err != nil {
			return err
		}
		if err := db.ClearTaskGatedSHA(ctx, task.ID); err != nil {
			return err
		}
		return axi.Failure("branch_moved", "pull request head moved after the Gate passed", false, "Run `posse land "+taskIDString(task.Seq)+"` again to re-gate")
	}
	observation, observationErr := db.LatestPRObservation(ctx, task.ID)
	if observationErr != nil && !store.IsNotFound(observationErr) {
		return observationErr
	}
	var observationChecks checkSnapshot
	checksErr := json.Unmarshal([]byte(observation.Checks), &observationChecks)
	if observationErr != nil || checksErr != nil || observation.PRURL != task.PRURL || observation.HeadSHA != task.GatedSHA || observationChecks.State != "SUCCESS" || observation.Review == "CHANGES_REQUESTED" || observation.Review == "REVIEW_REQUIRED" || observation.Mergeable != "MERGEABLE" {
		return axi.Failure("checks_not_green", "pull request does not have a green observation for the gated head", false, "Wait for `land_ready` before retrying `posse land "+taskIDString(task.Seq)+" --merge`")
	}
	if userQuote != "" {
		if err := db.RecordApproval(ctx, task.ID, "merge", userQuote); err != nil {
			return err
		}
	}
	intent, err := s.startTaskIntent(ctx, db, project.ID, task.ID, "land --merge")
	if err != nil {
		return axi.Failure("intent_active", "Task already has an unfinished command", true, err.Error())
	}
	defer func() {
		if finishErr := db.FinishIntent(ctx, intent.ID, intent.ProcessID); returnErr == nil && finishErr != nil {
			returnErr = finishErr
		}
	}()
	method := defaultValue(cfg.Defaults.MergeMethod, "squash")
	if forge.Kind == "gitlab" {
		number, _ := forgeReference(task.PRURL, forge)
		args := []string{"mr", "merge", strconv.Itoa(number), "--repo", "https://" + forge.Host + "/" + forge.Path, "--sha", task.GatedSHA, "--yes", "--auto-merge=false"}
		if method == "squash" {
			args = append(args, "--squash")
		}
		if _, err := runOutputStep(ctx, db, intent, "pr.merge", forge.Root, "glab", args...); err != nil {
			return axi.Failure("pr_merge_failed", "GitLab did not merge the merge request", true, err.Error())
		}
	} else if _, err := runOutputStep(ctx, db, intent, "pr.merge", project.Root, "gh", "pr", "merge", task.PRURL, "--"+method, "--match-head-commit", task.GatedSHA); err != nil {
		return axi.Failure("pr_merge_failed", "GitHub did not merge the pull request", true, err.Error())
	}
	return ctxPrintLand(out, task, "Run `posse` or `posse lookout` to observe the merge")
}

func runOutputStep(ctx context.Context, db *store.DB, intent store.Intent, step, cwd, name string, args ...string) (string, error) {
	if err := db.UpdateIntentStep(ctx, intent.ID, intent.ProcessID, "in_progress:"+step); err != nil {
		return "", err
	}
	crashIntentAt(intent.Command, "before", step)
	output, err := commandOutputArgs(ctx, cwd, name, args...)
	if err != nil {
		return output, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(output))
	}
	if err := db.UpdateIntentStep(ctx, intent.ID, intent.ProcessID, "done:"+step); err != nil {
		return output, err
	}
	crashIntentAt(intent.Command, "after", step)
	return output, nil
}

func ctxPrintLand(ctx *axi.Context, task store.Task, help string) error {
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "landing"}, {Key: "pr_url", Value: task.PRURL}, {Key: "gated_sha", Value: task.GatedSHA}, {Key: "help", Value: []any{help}}})
}

func prDetails(ctx context.Context, db *store.DB, project store.Project, task store.Task, homeFor func() (string, error), member, summary, verification, proof, risk string) (string, string, error) {
	if strings.TrimSpace(summary) == "" {
		signals, err := db.TaskSignals(ctx, task.ID, 20)
		if err != nil {
			return "", "", err
		}
		for _, signal := range signals {
			if signal.Verb == "done" {
				summary = signal.Note
				break
			}
		}
	}
	if strings.TrimSpace(summary) == "" {
		return "", "", axi.Failure("pr_summary_missing", "Rider completion summary is missing", false)
	}
	home, err := homeFor()
	if err != nil {
		return "", "", err
	}
	briefPath := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), "brief.md")
	contents, err := osReadFile(briefPath)
	if err != nil {
		return "", "", axi.Failure("brief_missing", "could not read the Task Brief for the pull request body", false, err.Error())
	}
	brief, err := dispatch.ParseHistoricalBriefText(string(contents))
	if err != nil {
		return "", "", axi.Failure("brief_invalid", "could not parse the Task Brief for the pull request body", false, err.Error())
	}
	changes, err := gitOutput(ctx, task.WorktreePath, "log", "--reverse", "--format=- %s", task.BaseRef+"..HEAD")
	if err != nil {
		return "", "", axi.Failure("pr_changes_failed", "could not read Task commit subjects for the pull request body", false, err.Error())
	}
	changes = strings.TrimSpace(changes)
	if changes == "" {
		return "", "", axi.Failure("pr_changes_failed", "Task branch has no commit subjects for the pull request body", false)
	}
	verification = verificationChecklist(verification)
	proof = strings.TrimSpace(proof)
	if proof == "" {
		proof = "_No proof supplied._"
	}
	risk = strings.TrimSpace(risk)
	if risk == "" {
		risk = "Risk: not stated\nRollback: revert this PR"
	}
	issueLinks := strings.Join(issueLinkLines(brief, member), "\n")
	issueSection := "## Issue Link"
	if issueLinks != "" {
		issueSection += "\n\n" + issueLinks
	}
	body := strings.Join([]string{
		"## Summary\n\n" + strings.TrimSpace(summary),
		issueSection,
		"## Changes\n\n" + changes,
		"## Verification\n\n" + verification,
		"## Proof\n\n" + proof,
		"## Risk And Rollback\n\n" + risk,
		"## Documentation\n\n" + documentationChecklist,
	}, "\n\n") + "\n"
	return brief.Title, body, nil
}

func verificationChecklist(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "- [ ] Not run; reason: not supplied"
	}
	items := make([]string, 0)
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "- [ ] ") || strings.HasPrefix(line, "- [x] ") || strings.HasPrefix(line, "- [X] ") {
			items = append(items, line)
		} else {
			items = append(items, "- [x] "+line)
		}
	}
	if len(items) == 0 {
		return "- [ ] Not run; reason: not supplied"
	}
	return strings.Join(items, "\n")
}

const documentationChecklist = `- [ ] Documentation updated for this change
- [ ] CLAUDE.md/AGENTS.md updated if needed`

var osReadFile = func(path string) ([]byte, error) { return os.ReadFile(path) }

func pullRequestURLFromOutput(output string) (string, error) {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		candidate := strings.TrimSpace(line)
		if _, err := pullRequestReferenceFromURL(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("no GitHub pull request URL was found")
}

func pullRequestReferenceFromURL(value string) (pullRequestReference, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return pullRequestReference{}, errors.New("expected an HTTPS pull request URL")
	}
	if before, after, ok := strings.Cut(strings.Trim(parsed.Path, "/"), "/-/merge_requests/"); ok {
		number, err := strconv.Atoi(after)
		if err != nil || number < 1 || before == "" || strings.Contains(before, "..") {
			return pullRequestReference{}, errors.New("invalid GitLab merge request URL")
		}
		parts := strings.Split(before, "/")
		if len(parts) < 2 {
			return pullRequestReference{}, errors.New("invalid GitLab repository path")
		}
		return pullRequestReference{Host: strings.ToLower(parsed.Hostname()), Owner: strings.Join(parts[:len(parts)-1], "/"), Repo: parts[len(parts)-1], Number: number}, nil
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return pullRequestReference{}, errors.New("expected a URL ending in /<owner>/<repo>/pull/<number>")
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number < 1 || parts[0] == "" || parts[1] == "" {
		return pullRequestReference{}, errors.New("pull request URL has an invalid owner, repository or number")
	}
	return pullRequestReference{Host: strings.ToLower(parsed.Hostname()), Owner: parts[0], Repo: parts[1], Number: number}, nil
}

func validatePullRequestOrigin(ctx context.Context, root, value string) error {
	pull, err := pullRequestReferenceFromURL(value)
	if err != nil {
		return axi.Failure("pr_url_invalid", "pull request URL is invalid", false, err.Error())
	}
	remote, err := gitOutput(ctx, root, "config", "--get", "remote.origin.url")
	if err != nil {
		return axi.Failure("pr_origin_unavailable", "could not read the Project origin URL", true, err.Error())
	}
	host, path, err := remoteRepository(remote)
	if err != nil {
		return axi.Failure("pr_origin_invalid", "Project origin is not a forge repository URL", false, err.Error())
	}
	parts := strings.Split(path, "/")
	origin := pullRequestReference{Host: host, Owner: strings.Join(parts[:len(parts)-1], "/"), Repo: parts[len(parts)-1]}
	if !strings.EqualFold(pull.Host, origin.Host) || !strings.EqualFold(pull.Owner, origin.Owner) || !strings.EqualFold(pull.Repo, origin.Repo) {
		return axi.Failure("pr_repository_mismatch", "pull request does not belong to the Project origin repository", false, fmt.Sprintf("Use a pull request for %s/%s on %s", origin.Owner, origin.Repo, origin.Host))
	}
	return nil
}

func (s *Service) pollProjectPullRequests(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, force bool) error {
	return s.pollProjectPullRequestsWithLandedCallback(ctx, db, project, cfg, force, nil)
}

func (s *Service) pollProjectPullRequestsWithLandedCallback(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, force bool, onWorkspaceTaskLanded func()) error {
	if project.IsWorkspace() {
		return s.pollWorkspacePullRequestsAndNotify(ctx, db, project, cfg, force, onWorkspaceTaskLanded)
	}
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return err
	}
	watched := make([]store.Task, 0)
	for _, task := range tasks {
		if task.PRURL == "" {
			continue
		}
		if task.State != store.StateLanded && task.State != store.StateTornDown && task.State != store.StateReported {
			previous, previousErr := db.LatestPRObservation(ctx, task.ID)
			if previousErr != nil && !store.IsNotFound(previousErr) {
				return previousErr
			}
			if previousErr == nil && previous.PRURL == task.PRURL && previous.State == "CLOSED" {
				if err := raiseClosedPRDecision(ctx, db, project, task, task.PRURL); err != nil {
					return err
				}
			}
			watched = append(watched, task)
		}
	}
	now := time.Now()
	interval := parseDurationOr(cfg.Defaults.PRPoll, 2*time.Minute)
	claim, err := db.ClaimPRPoll(ctx, project.ID, now, interval, force)
	if err != nil {
		return err
	}
	if claim == "" {
		return nil
	}
	defer func() { _ = db.ReleasePRPoll(context.Background(), project.ID, claim) }()
	if len(watched) == 0 {
		_, err := db.RecordPRPoll(ctx, project.ID, now.UnixMilli(), "")
		return err
	}
	targets := make([]prWatchTarget, 0, len(watched))
	failedTasks := make(map[int64]error)
	forge, forgeErr := forgeForRepository(ctx, project.Root, cfg, "")
	globalFailure := ""
	if forgeErr != nil {
		globalFailure = truncate(forgeErr.Error(), 300)
	}
	gitlabTargets := make([]store.Task, 0)
	for _, task := range watched {
		refErr := forgeErr
		if refErr == nil {
			_, refErr = forgeReference(task.PRURL, forge)
		}
		if refErr != nil {
			failedTasks[task.ID] = refErr
			continue
		}
		if forge.Kind == "gitlab" {
			gitlabTargets = append(gitlabTargets, task)
		} else {
			targets = append(targets, prWatchTarget{Task: task, Pull: "pr" + strconv.Itoa(task.Seq)})
		}
	}
	query, targets := pullRequestGraphQLQuery(targets)
	pulls := make(map[int64]*ghPullRequest, len(targets))
	var response ghQueryResponse
	if query != "" {
		output, stderr, commandErr := graphQLCommandOutput(ctx, project.Root, query)
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			failure := "invalid GraphQL response: " + err.Error()
			if commandErr != nil {
				failure = truncate(strings.TrimSpace(stderr+" "+commandErr.Error()), 300)
			}
			if _, recordErr := db.RecordPRPoll(ctx, project.ID, now.UnixMilli(), failure); recordErr != nil {
				return recordErr
			}
			for _, task := range watched {
				if recordErr := recordPRTaskWatchFailure(ctx, db, project, task, errors.New(failure), now); recordErr != nil {
					return recordErr
				}
			}
			return nil
		}
		if commandErr != nil && (len(response.Errors) == 0 || errors.Is(commandErr, context.Canceled) || errors.Is(commandErr, context.DeadlineExceeded)) {
			globalFailure = truncate(strings.TrimSpace(stderr+" "+commandErr.Error()), 300)
		}
		if response.Data.Repositories == nil {
			globalFailure = "GraphQL response omitted data"
		}
		for _, failure := range response.Errors {
			attributed := false
			if len(failure.Path) > 0 {
				repository, _ := failure.Path[0].(string)
				pull := ""
				if len(failure.Path) > 1 {
					pull, _ = failure.Path[1].(string)
				}
				for _, target := range targets {
					if target.Repository == repository && (pull == "" || target.Pull == pull) {
						failedTasks[target.Task.ID] = errors.New(failure.Message)
						attributed = true
					}
				}
			}
			if !attributed {
				globalFailure = truncate(strings.TrimSpace(globalFailure+" "+failure.Message), 300)
			}
		}
		for _, target := range targets {
			if failedTasks[target.Task.ID] != nil {
				continue
			}
			pull, pullErr := response.pullRequest(target)
			if pullErr != nil {
				failedTasks[target.Task.ID] = pullErr
				continue
			}
			pulls[target.Task.ID] = pull
		}
		if _, err := db.RecordPRPoll(ctx, project.ID, now.UnixMilli(), globalFailure); err != nil {
			return err
		}
	} else if _, err := db.RecordPRPoll(ctx, project.ID, now.UnixMilli(), globalFailure); err != nil {
		return err
	}
	gitlabObservations := make(map[int64]store.PRObservation)
	gitlabFailures := make(map[int64][]failedCheck)
	for _, task := range gitlabTargets {
		observation, failures, pollErr := gitlabObservation(ctx, forge, task, now.UnixMilli())
		if pollErr != nil {
			failedTasks[task.ID] = pollErr
			continue
		}
		gitlabObservations[task.ID] = observation
		gitlabFailures[task.ID] = failures
	}
	type pendingObservation struct {
		task      store.Task
		current   store.PRObservation
		failures  []failedCheck
		previous  store.PRObservation
		hasBefore bool
	}
	observations := make([]pendingObservation, 0, len(targets))
	for _, task := range watched {
		if taskErr := failedTasks[task.ID]; taskErr != nil {
			if strings.Contains(taskErr.Error(), "Could not resolve to a PullRequest") {
				if err := raiseInvalidPRDecision(ctx, db, project, task); err != nil {
					return err
				}
				continue
			}
			if err := recordPRTaskWatchFailure(ctx, db, project, task, taskErr, now); err != nil {
				return err
			}
			continue
		}
		pull := pulls[task.ID]
		if pull == nil && forge.Kind != "gitlab" {
			continue
		}
		previous, previousErr := db.LatestPRObservation(ctx, task.ID)
		hasPrevious := previousErr == nil && previous.PRURL == task.PRURL
		if previousErr != nil && !store.IsNotFound(previousErr) {
			return previousErr
		}
		var observation store.PRObservation
		var failures []failedCheck
		if forge.Kind == "gitlab" {
			observation, failures = gitlabObservations[task.ID], gitlabFailures[task.ID]
		} else {
			if strings.EqualFold(pull.Mergeable, "UNKNOWN") && hasPrevious {
				pull.Mergeable = previous.Mergeable
			}
			observation, failures, err = makePRObservation(project.ID, task, pull, now)
		}
		if observation.Mergeable == "UNKNOWN" && hasPrevious {
			observation.Mergeable = previous.Mergeable
		}
		if err != nil {
			if recordErr := recordPRTaskWatchFailure(ctx, db, project, task, err, now); recordErr != nil {
				return recordErr
			}
			continue
		}
		if observation.State == "MERGED" {
			verified, verifyErr := db.WasVerifiedPRHead(ctx, task.ID, task.PRURL, observation.HeadSHA)
			if verifyErr != nil {
				return verifyErr
			}
			// A head still on the Mount's Task branch is also the Rider's own.
			if !verified && task.WorktreePath != "" && task.Branch != "" {
				branch, branchErr := gitOutput(ctx, task.WorktreePath, "symbolic-ref", "--quiet", "--short", "HEAD")
				if branchErr == nil && branch == task.Branch {
					_, verifyErr = gitOutput(ctx, task.WorktreePath, "merge-base", "--is-ancestor", observation.HeadSHA, "HEAD")
					verified = verifyErr == nil
				}
			}
			if !verified {
				if err := recordPRTaskWatchFailure(ctx, db, project, task, fmt.Errorf("merged head %s is neither on the Mount Task branch nor recorded by publish", observation.HeadSHA), now); err != nil {
					return err
				}
				continue
			}
			if (task.State != store.StateLanding || task.GatedSHA != observation.HeadSHA) && validateWorkerPullRequestWithRemote(ctx, project, task, forge, task.PRURL, observation.HeadSHA, false) != nil {
				err := fmt.Errorf("merged pull request identity could not be verified for %s", task.PRURL)
				if recordErr := recordPRTaskWatchFailure(ctx, db, project, task, err, now); recordErr != nil {
					return recordErr
				}
				continue
			}
		}
		observations = append(observations, pendingObservation{task: task, current: observation, failures: failures, previous: previous, hasBefore: hasPrevious})
	}
	for _, observed := range observations {
		effect := prObservationEffect(project, observed.task, observed.current, observed.failures, observed.previous, observed.hasBefore)
		if observed.current.State == "MERGED" && observed.hasBefore && observed.previous.PRURL == observed.current.PRURL && observed.previous.State == "MERGED" {
			var exists bool
			if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notices WHERE task_id=? AND kind='pr_merged' AND data_json=?)`, observed.task.ID, marshalJSON(map[string]any{"url": observed.current.PRURL, "head_sha": observed.current.HeadSHA, "merge_commit": observed.current.MergeCommit})).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				effect.Notices = append(effect.Notices, store.Notice{ProjectID: project.ID, TaskID: observed.task.ID, Kind: "pr_merged", Summary: observed.task.Title + ": pull request merged", DataJSON: marshalJSON(map[string]any{"url": observed.current.PRURL, "head_sha": observed.current.HeadSHA, "merge_commit": observed.current.MergeCommit}), CreatedAt: observed.current.ObservedAt})
			}
		}
		recorded, err := db.RecordPRObservation(ctx, observed.current, effect, observed.task)
		if err != nil {
			return err
		}
		if recorded && observed.current.State == "CLOSED" {
			if err := raiseClosedPRDecision(ctx, db, project, observed.task, observed.current.PRURL); err != nil {
				return err
			}
		}
		if recorded && observed.current.State == "MERGED" {
			if _, err := s.syncProjectRoot(ctx, db, project, cfg, true); err != nil {
				if noticeErr := recordPRTaskWatchFailure(ctx, db, project, observed.task, err, now); noticeErr != nil {
					return noticeErr
				}
			}
		}
	}
	return nil
}

// gh may exit nonzero for a single GraphQL error while still printing usable
// data for the other aliases. Keep stderr separate so it cannot corrupt JSON.
func graphQLCommandOutput(ctx context.Context, root, query string) (string, string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, externalCommandTimeout)
	defer cancel()
	command := execgroup.CommandContext(commandCtx, "gh", "api", "graphql", "-f", "query="+query)
	command.Dir = root
	command.Env = externalCommandEnvironment("gh")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if commandCtx.Err() != nil {
		if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("gh timed out after %s: %w", externalCommandTimeout, context.DeadlineExceeded)
		} else {
			err = commandCtx.Err()
		}
	}
	return stdout.String(), stderr.String(), err
}

func pullRequestGraphQLQuery(targets []prWatchTarget) (string, []prWatchTarget) {
	if len(targets) == 0 {
		return "", nil
	}
	groups := make([]string, 0)
	groupAliases := make(map[string]string)
	groupFields := make(map[string][]string)
	for index := range targets {
		ref, err := pullRequestReferenceFromURL(targets[index].Task.PRURL)
		if err != nil {
			continue
		}
		key := strings.ToLower(ref.Host + "/" + ref.Owner + "/" + ref.Repo)
		repository, found := groupAliases[key]
		if !found {
			repository = "repo" + strconv.Itoa(len(groupAliases)+1)
			groupAliases[key] = repository
			groups = append(groups, repository)
		}
		targets[index].Repository = repository
		groupFields[repository] = append(groupFields[repository], fmt.Sprintf(`%s: pullRequest(number: %d) {
  url state headRefOid mergeable reviewDecision mergeCommit { oid }
  statusCheckRollup {
    state
    contexts(first: 100) {
      nodes {
        __typename
        ... on CheckRun { name status conclusion detailsUrl }
        ... on StatusContext { context state targetUrl }
      }
    }
  }
	}`, targets[index].Pull, ref.Number))
	}
	rootFields := make([]string, 0, len(groups))
	for _, repository := range groups {
		first := -1
		for index := range targets {
			if targets[index].Repository == repository {
				first = index
				break
			}
		}
		ref, _ := pullRequestReferenceFromURL(targets[first].Task.PRURL)
		rootFields = append(rootFields, fmt.Sprintf(`%s: repository(owner: %s, name: %s) {
%s
}`, repository, strconv.Quote(ref.Owner), strconv.Quote(ref.Repo), strings.Join(groupFields[repository], "\n")))
	}
	return "query {\n" + strings.Join(rootFields, "\n") + "\n}", targets
}

func (response *ghQueryResponse) pullRequest(target prWatchTarget) (*ghPullRequest, error) {
	data, ok := response.Data.Repositories[target.Repository]
	if !ok || len(data) == 0 || string(data) == "null" {
		return nil, fmt.Errorf("GraphQL response omitted repository alias %s", target.Repository)
	}
	var repository map[string]*ghPullRequest
	if err := json.Unmarshal(data, &repository); err != nil {
		return nil, fmt.Errorf("invalid GraphQL repository alias %s: %w", target.Repository, err)
	}
	pull := repository[target.Pull]
	if pull == nil {
		return nil, fmt.Errorf("GraphQL response omitted pull request %s", target.Task.PRURL)
	}
	return pull, nil
}

func recordWorkspacePRTaskWatchFailure(ctx context.Context, db *store.DB, project store.Project, task store.Task, repo, prURL string, cause error, now time.Time) error {
	summary := taskDisplayName(task) + " pull request watch failed for " + repo + ": " + truncate(cause.Error(), 240)
	data := marshalJSON(map[string]any{"repo": repo, "pr_url": prURL, "error": cause.Error()})
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notices WHERE project_id=? AND task_id=? AND kind='pr_watch_failing' AND data_json=?)`, project.ID, task.ID, data).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "pr_watch_failing", Summary: summary, DataJSON: data, CreatedAt: now.UnixMilli()})
	return err
}

func recordPRTaskWatchFailure(ctx context.Context, db *store.DB, project store.Project, task store.Task, cause error, now time.Time) error {
	summary := taskDisplayName(task) + " pull request watch failed: " + truncate(cause.Error(), 240)
	data := marshalJSON(map[string]any{"pr_url": task.PRURL, "error": cause.Error()})
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notices WHERE project_id=? AND task_id=? AND kind='pr_watch_failing' AND data_json=?)`, project.ID, task.ID, data).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "pr_watch_failing", Summary: summary, DataJSON: data, CreatedAt: now.UnixMilli()})
	return err
}

func makePRObservation(projectID int64, task store.Task, pull *ghPullRequest, now time.Time) (store.PRObservation, []failedCheck, error) {
	if pull.URL == "" || pull.HeadRefOID == "" || pull.State == "" {
		return store.PRObservation{}, nil, errors.New("GitHub response omitted pull request state or head SHA")
	}
	if pull.URL != task.PRURL {
		return store.PRObservation{}, nil, errors.New("GitHub response URL does not match the watched pull request")
	}
	checks := checkSnapshot{State: strings.ToUpper(pull.StatusCheckRollupState())}
	if checks.State == "" || checks.State == "EXPECTED" || checks.State == "PENDING" || checks.State == "QUEUED" || checks.State == "IN_PROGRESS" || checks.State == "REQUESTED" {
		checks.State = "PENDING"
	}
	for _, node := range pull.checkNodes() {
		typename, _ := node["__typename"].(string)
		name, _ := node["name"].(string)
		if name == "" {
			name, _ = node["context"].(string)
		}
		status, _ := node["state"].(string)
		if status == "" {
			status, _ = node["conclusion"].(string)
		}
		url, _ := node["detailsUrl"].(string)
		if url == "" {
			url, _ = node["targetUrl"].(string)
		}
		failed := false
		if typename == "CheckRun" {
			conclusion, _ := node["conclusion"].(string)
			failed = oneOfString(strings.ToUpper(conclusion), "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE")
		} else {
			failed = oneOfString(strings.ToUpper(status), "ERROR", "FAILURE")
		}
		if failed {
			checks.Failures = append(checks.Failures, failedCheck{Name: name, URL: url})
		}
	}
	if checks.State == "FAILURE" && len(checks.Failures) == 0 {
		checks.Failures = append(checks.Failures, failedCheck{Name: "required check rollup", URL: ""})
	}
	sort.Slice(checks.Failures, func(i, j int) bool {
		if checks.Failures[i].Name == checks.Failures[j].Name {
			return checks.Failures[i].URL < checks.Failures[j].URL
		}
		return checks.Failures[i].Name < checks.Failures[j].Name
	})
	encodedChecks, err := json.Marshal(checks)
	if err != nil {
		return store.PRObservation{}, nil, err
	}
	mergeCommit := ""
	if pull.MergeCommit != nil {
		mergeCommit = pull.MergeCommit.OID
	}
	if strings.EqualFold(pull.State, "MERGED") && mergeCommit == "" {
		return store.PRObservation{}, nil, errors.New("GitHub response omitted the pull request merge commit")
	}
	return store.PRObservation{
		ProjectID: projectID, TaskID: task.ID, PRURL: task.PRURL,
		HeadSHA: pull.HeadRefOID, State: strings.ToUpper(pull.State), Checks: string(encodedChecks),
		Review: strings.ToUpper(pull.ReviewDecision), Mergeable: strings.ToUpper(pull.Mergeable),
		MergeCommit: mergeCommit, ObservedAt: now.UnixMilli(),
	}, checks.Failures, nil
}

func (pull *ghPullRequest) StatusCheckRollupState() string {
	if pull.StatusCheckRollup == nil {
		return "PENDING"
	}
	return pull.StatusCheckRollup.State
}

func (pull *ghPullRequest) checkNodes() []map[string]any {
	if pull.StatusCheckRollup == nil {
		return nil
	}
	return pull.StatusCheckRollup.Contexts.Nodes
}

func prObservationEffect(project store.Project, task store.Task, current store.PRObservation, failures []failedCheck, previous store.PRObservation, hasPrevious bool) store.PRObservationEffect {
	effect := store.PRObservationEffect{}
	newHead := !hasPrevious || previous.HeadSHA != current.HeadSHA
	previousChecks := decodeChecks(previous.Checks)
	checks := decodeChecks(current.Checks)
	addNotice := func(kind, summary string, data any) {
		effect.Notices = append(effect.Notices, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: kind, Summary: summary, DataJSON: marshalJSON(data), CreatedAt: current.ObservedAt})
	}
	switch current.State {
	case "MERGED":
		if !hasPrevious || previous.State != "MERGED" {
			addNotice("pr_merged", task.Title+": pull request merged", map[string]any{"url": current.PRURL, "head_sha": current.HeadSHA, "merge_commit": current.MergeCommit})
		}
		effect.TransitionTo = store.StateLanded
		effect.TransitionNote = "Pull request merged at " + current.MergeCommit
		effect.LandedRef = current.MergeCommit
	case "CLOSED":
		if !hasPrevious || previous.State != "CLOSED" {
			addNotice("pr_closed", task.Title+": pull request closed without merging", map[string]any{"url": current.PRURL, "head_sha": current.HeadSHA})
		}
	case "OPEN":
		if len(failures) > 0 && (newHead || len(previousChecks.Failures) == 0 || !sameFailures(previousChecks.Failures, failures)) {
			addNotice("pr_checks_failed", task.Title+": required checks failed: "+failedCheckNames(failures), map[string]any{"url": current.PRURL, "head_sha": current.HeadSHA, "checks": failures})
		}
		if current.Review == "CHANGES_REQUESTED" && (newHead || previous.Review != "CHANGES_REQUESTED") {
			addNotice("pr_changes_requested", task.Title+": review requests changes", map[string]any{"url": current.PRURL, "head_sha": current.HeadSHA, "review": current.Review})
		}
		if current.Mergeable == "CONFLICTING" && (newHead || previous.Mergeable != "CONFLICTING") {
			addNotice("pr_conflict", task.Title+": pull request conflicts with the Project default branch", map[string]any{"url": current.PRURL, "head_sha": current.HeadSHA})
		}
		ready := checks.State == "SUCCESS" && current.Review != "CHANGES_REQUESTED" && current.Review != "REVIEW_REQUIRED" && current.Mergeable == "MERGEABLE"
		previousReady := hasPrevious && previous.State == "OPEN" && previous.HeadSHA == current.HeadSHA && previousChecks.State == "SUCCESS" && previous.Review != "CHANGES_REQUESTED" && previous.Review != "REVIEW_REQUIRED" && previous.Mergeable == "MERGEABLE"
		if ready && !previousReady {
			addNotice("land_ready", task.Title+": pull request checks and reviews are ready to merge", map[string]any{"url": current.PRURL, "head_sha": current.HeadSHA})
		}
	}
	return effect
}

func sameFailures(left, right []failedCheck) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func decodeChecks(value string) checkSnapshot {
	var checks checkSnapshot
	_ = json.Unmarshal([]byte(value), &checks)
	return checks
}

func failedCheckNames(failures []failedCheck) string {
	names := make([]string, 0, len(failures))
	for _, failure := range failures {
		name := failure.Name
		if failure.URL != "" {
			name += " (" + failure.URL + ")"
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

func pullRequestDisplay(ctx context.Context, db *store.DB, task store.Task) (map[string]any, string, error) {
	if task.PRURL == "" {
		return nil, "", nil
	}
	observation, err := db.LatestPRObservation(ctx, task.ID)
	if errors.Is(err, store.ErrNotFound) {
		return map[string]any{"url": task.PRURL, "state": "OPEN", "checks": "pending", "review": "unknown", "mergeable": "unknown", "updated": ""}, "OPEN", nil
	}
	if err != nil {
		return nil, "", err
	}
	checks := decodeChecks(observation.Checks)
	checkLabel := strings.ToLower(checks.State)
	if len(checks.Failures) > 0 {
		checkLabel = "failed: " + failedCheckNames(checks.Failures)
	}
	updated := time.UnixMilli(observation.ObservedAt).Local().Format(time.RFC3339)
	return map[string]any{
		"url": observation.PRURL, "state": observation.State, "checks": checkLabel,
		"review":    defaultValue(strings.ToLower(observation.Review), "unknown"),
		"mergeable": defaultValue(strings.ToLower(observation.Mergeable), "unknown"), "updated": updated,
	}, observation.State, nil
}

func parseDurationOr(value string, fallback time.Duration) time.Duration {
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
