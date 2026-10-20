package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

// noMistakesReceiptSchema is the pipeline-owned receipt contract Posse accepts.
// Posse trusts the no-mistakes implementation that produced the receipt; it
// does not attest which runtime model actually reviewed the candidate.
const noMistakesReceiptSchema = "no-mistakes.review-receipt.v1"

type noMistakesReceipt struct {
	Schema     string `json:"schema"`
	Repository string `json:"repository"`
	RunID      string `json:"run_id"`
	HeadSHA    string `json:"head_sha"`
	RunStatus  string `json:"run_status"`
	Review     struct {
		Stage    string `json:"stage"`
		Status   string `json:"status"`
		Reviewer struct {
			Profile string `json:"profile"`
			Model   string `json:"model"`
		} `json:"reviewer"`
		ReportSHA256 string `json:"report_sha256"`
	} `json:"review"`
}

// queryNoMistakesReceipt asks the pipeline owner, never the Worker, for the
// receipt of one exact candidate commit.
var queryNoMistakesReceipt = func(ctx context.Context, root, commit string) (string, error) {
	if _, err := exec.LookPath("no-mistakes"); err != nil {
		return "", errNoMistakesMissing
	}
	return commandStdoutArgs(ctx, root, "no-mistakes", "axi", "receipt", "--head", commit)
}

var errNoMistakesMissing = errors.New("no-mistakes is not installed or not on PATH")

// requireNoMistakesReceipt fails closed unless no-mistakes reports a passing
// review stage of commit in the Project's origin repository by a configured
// reviewer distinct from every author launch. A qualifying receipt is recorded.
func requireNoMistakesReceipt(ctx context.Context, db *store.DB, project store.Project, task store.Task, commit string) error {
	id := taskIDString(task.Seq)
	if commit == "" {
		if task.Branch == "" {
			return axi.Failure("review_receipt_missing", "Task "+id+" has no branch to resolve its review candidate", false)
		}
		head, err := gitOutput(ctx, project.Root, "rev-parse", "--verify", "refs/heads/"+task.Branch+"^{commit}")
		if err != nil {
			return axi.Failure("review_receipt_missing", "could not resolve the review candidate of Task "+id, false, err.Error())
		}
		commit = head
	}
	remote, err := gitOutput(ctx, project.Root, "config", "--get", "remote.origin.url")
	if err != nil {
		return axi.Failure("review_receipt_unavailable", "could not read the Project origin to bind the no-mistakes receipt", true, err.Error())
	}
	host, path, err := remoteRepository(remote)
	if err != nil {
		return axi.Failure("review_receipt_unavailable", "Project origin is not a forge repository; a no-mistakes receipt cannot be bound to it", false, err.Error())
	}
	repository := strings.ToLower(host + "/" + path)
	output, err := queryNoMistakesReceipt(ctx, project.Root, commit)
	if errors.Is(err, errNoMistakesMissing) {
		return axi.Failure("no_mistakes_unavailable", "no-mistakes is not installed or not on PATH; its review receipt for "+shortSHA(commit)+" cannot be queried", false, "Install no-mistakes, or have the User select a different review policy")
	}
	if err != nil {
		return noMistakesQueryFailure(commit, err)
	}
	receipt, err := verifyNoMistakesReceipt(output, repository, commit)
	if err != nil {
		return err
	}
	launches, err := db.TaskLaunchIdentities(ctx, task.ID)
	if err != nil {
		return err
	}
	reviewer := store.TaskLaunchIdentity{Profile: receipt.Review.Reviewer.Profile, ConfiguredModel: receipt.Review.Reviewer.Model, ModelKnown: true}
	if check := compareReviewIdentity(launches, launchHistoryComplete(task, launches), reviewer); !check.Eligible {
		help := []string{"no-mistakes reviewer: " + reviewIdentityDescription(reviewer)}
		for _, launch := range launches {
			help = append(help, "Author "+id+" "+reviewIdentityDescription(launch))
		}
		help = append(help, "Configure the no-mistakes reviewer with a Profile and model that differ from every author launch, then rerun its pipeline")
		return axi.Failure("review_receipt_identity_ineligible", "no-mistakes reviewer of "+shortSHA(commit)+" is not independent: "+check.Reason, false, help...)
	}
	sum := sha256.Sum256([]byte(output))
	return db.RecordReviewReceipt(ctx, store.ReviewReceipt{
		TaskID: task.ID, Repository: receipt.Repository, RunID: receipt.RunID, HeadSHA: receipt.HeadSHA,
		ReviewStatus: receipt.Review.Status, ReviewerProfile: receipt.Review.Reviewer.Profile, ReviewerModel: receipt.Review.Reviewer.Model,
		ReportSHA256: receipt.Review.ReportSHA256, ReceiptSHA256: hex.EncodeToString(sum[:]), ObservedAt: time.Now().UnixMilli(),
	})
}

func noMistakesQueryFailure(commit string, err error) error {
	detail := truncate(strings.TrimSpace(err.Error()), 1200)
	lower := strings.ToLower(detail)
	if strings.Contains(lower, "unknown flag") || strings.Contains(lower, "unknown command") || strings.Contains(lower, "unknown shorthand flag") {
		return axi.Failure("review_receipt_unsupported", "the installed no-mistakes cannot report a review receipt (`no-mistakes axi receipt --head <sha>`)", false, detail, "Upgrade to a no-mistakes release that implements the "+noMistakesReceiptSchema+" receipt, or have the User select a different review policy; CI, forge approval or a quick Review cannot substitute")
	}
	retryable := errors.Is(err, context.DeadlineExceeded)
	return axi.Failure("review_receipt_unavailable", "no-mistakes returned no review receipt for "+shortSHA(commit), retryable, detail, "Run the no-mistakes pipeline for this exact commit, then retry")
}

// verifyNoMistakesReceipt checks the receipt fields that a Posse merge relies
// on. It never fills a missing field from Posse's own records.
func verifyNoMistakesReceipt(output, repository, commit string) (noMistakesReceipt, error) {
	var receipt noMistakesReceipt
	if err := json.Unmarshal([]byte(output), &receipt); err != nil {
		return receipt, axi.Failure("review_receipt_invalid", "no-mistakes returned a receipt that is not valid JSON", false, err.Error())
	}
	if receipt.Schema != noMistakesReceiptSchema {
		return receipt, axi.Failure("review_receipt_invalid", "no-mistakes receipt schema "+quoteOrEmpty(receipt.Schema)+" is not supported; expected "+noMistakesReceiptSchema, false)
	}
	// A failed pipeline for this candidate is reported as failed even when it
	// omits fields that only a completed review produces.
	sameHead := receipt.HeadSHA == "" || strings.EqualFold(receipt.HeadSHA, commit)
	if sameHead && (receipt.RunStatus != "" && receipt.RunStatus != "completed" || receipt.Review.Status != "" && receipt.Review.Status != "passed") {
		return receipt, noMistakesReceiptFailed(receipt, commit)
	}
	var missing []string
	for _, field := range []struct{ name, value string }{
		{"repository", receipt.Repository}, {"run_id", receipt.RunID}, {"head_sha", receipt.HeadSHA}, {"run_status", receipt.RunStatus},
		{"review.stage", receipt.Review.Stage}, {"review.status", receipt.Review.Status},
		{"review.reviewer.profile", receipt.Review.Reviewer.Profile}, {"review.reviewer.model", receipt.Review.Reviewer.Model},
		{"review.report_sha256", receipt.Review.ReportSHA256},
	} {
		if strings.TrimSpace(field.value) == "" {
			missing = append(missing, field.name)
		}
	}
	if len(missing) > 0 {
		return receipt, axi.Failure("review_receipt_incomplete", "no-mistakes receipt is missing required field(s): "+strings.Join(missing, ", "), false, "Posse does not infer missing receipt fields; upgrade no-mistakes or have the User select a different review policy")
	}
	if !hexDigest(receipt.HeadSHA, 40, 64) || !hexDigest(receipt.Review.ReportSHA256, 64) {
		return receipt, axi.Failure("review_receipt_invalid", "no-mistakes receipt head_sha or review.report_sha256 is not a hexadecimal digest", false)
	}
	if !strings.EqualFold(receipt.Repository, repository) {
		return receipt, axi.Failure("review_receipt_mismatch", "no-mistakes receipt is for repository "+receipt.Repository+", not "+repository, false)
	}
	if !strings.EqualFold(receipt.HeadSHA, commit) {
		return receipt, axi.Failure("review_receipt_stale", "no-mistakes receipt run "+receipt.RunID+" reviewed "+shortSHA(receipt.HeadSHA)+", not candidate "+shortSHA(commit), false, "Run the no-mistakes pipeline for the current head; a receipt for another commit never qualifies")
	}
	if receipt.RunStatus != "completed" || receipt.Review.Stage != "review" || receipt.Review.Status != "passed" {
		return receipt, noMistakesReceiptFailed(receipt, commit)
	}
	return receipt, nil
}

func noMistakesReceiptFailed(receipt noMistakesReceipt, commit string) error {
	return axi.Failure("review_receipt_failed", "no-mistakes run "+quoteOrEmpty(receipt.RunID)+" for "+shortSHA(commit)+" has run_status="+quoteOrEmpty(receipt.RunStatus)+", stage "+quoteOrEmpty(receipt.Review.Stage)+" status="+quoteOrEmpty(receipt.Review.Status)+"; a completed run with a passed review stage is required", false, "Address the pipeline findings and rerun it for the candidate")
}

func hexDigest(value string, lengths ...int) bool {
	matched := false
	for _, length := range lengths {
		matched = matched || len(value) == length
	}
	if !matched {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func quoteOrEmpty(value string) string {
	if value == "" {
		return "<missing>"
	}
	return "\"" + value + "\""
}
