package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

type reviewRoundRepository struct {
	name         string
	root         string
	worktree     string
	targetBranch string
	head         string
	workspace    bool
}

type reviewRoundScope struct {
	Binding      store.ReviewRound
	DeltaPatch   []byte
	Cumulative   []byte
	Conflicts    []string
	Guidance     map[string][]byte
	TargetBranch string
}

type generatedReviewRound struct {
	Admission store.ReviewRoundAdmission
	Targets   []store.ReviewTarget
	BriefData []byte
	BriefPath string
	Files     map[string][]byte
}

type reviewRoundJSON struct {
	Round           int                    `json:"round"`
	MaxRounds       int                    `json:"max_rounds"`
	RemainingRounds int                    `json:"remaining_rounds"`
	Repositories    []reviewRoundJSONEntry `json:"repositories"`
}

type reviewRoundJSONEntry struct {
	Repository      string   `json:"repository"`
	TargetBranch    string   `json:"target_branch"`
	ScopeKind       string   `json:"scope_kind"`
	ScopeBaseSHA    string   `json:"scope_base_sha"`
	HeadSHA         string   `json:"head_sha"`
	TargetCommitSHA string   `json:"target_commit_sha"`
	BaselineTree    string   `json:"baseline_tree"`
	DeltaSHA256     string   `json:"delta_sha256"`
	ConflictedPaths []string `json:"conflicted_paths"`
}

func (s *Service) reviewStart(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("review start", args, map[string]flagSpec{"name": {}, "profile": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("review start requires one Change Task id")
	}
	db, home, err := s.openDB()
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
	change, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	return s.startReviewRound(ctx, db, home, project, change, parsed.Flags["profile"], parsed.Flags["name"])
}

func (s *Service) startReviewRound(ctx *axi.Context, db *store.DB, home string, project store.Project, change store.Task, profile, name string) error {
	if project.IsDirectory() {
		return axi.Failure("review_round_unsupported", "directory Tasks cannot be reviewed as Git rounds", false)
	}
	if change.Type != "change" {
		return axi.Failure("review_round_target_invalid", "Review Rounds require a Change Task", false)
	}
	plan, err := change.AdmittedPlan()
	if err != nil {
		return admittedPlanFailure(err)
	}
	candidateReview := plan != nil && (plan.Review == "full-pipeline" || workspaceHasFullPipeline(plan))
	if candidateReview {
		if change.State != store.StateWorking && change.State != store.StateStalled && change.State != store.StateDone && change.State != store.StateMerging {
			return axi.Failure("review_target_incomplete", "Change Task must be active before its private candidate can be reviewed", false)
		}
		if err := s.prepareReviewCandidateCheckouts(ctx.Context, db, home, project, change, plan); err != nil {
			return err
		}
	} else if err := reviewableChange(change); err != nil {
		return err
	}
	plan, err = change.AdmittedPlan()
	if err != nil {
		return admittedPlanFailure(err)
	}
	if plan == nil || plan.ValidationLoops == nil || !plan.ValidationLoops.Review.Enabled {
		return axi.Failure("review_loop_disabled", "Change Task "+taskIDString(change.Seq)+" has no enabled, frozen Review Loop", false)
	}
	maxRounds := plan.ValidationLoops.MaxReviewRounds()
	if maxRounds < 1 || maxRounds > 3 {
		return axi.Failure("plan_invalid", "Change Task has an invalid frozen Review Round cap", false)
	}
	generated, err := buildReviewRound(ctx.Context, db, project, change, *plan, maxRounds, home)
	if err != nil {
		return err
	}
	if name == "" {
		name = fmt.Sprintf("review-round-%d", generated.Admission.Round)
	}
	if err := validateWorkerName(name); err != nil {
		return invalidWorkerNameFailure(name, err)
	}
	args := []string{"--name", name}
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	err = s.spawnTask(ctx, args, generated)
	var failure *axi.Error
	if errors.As(err, &failure) && failure.Code == "review_round_cap" {
		usage, usageErr := db.ReviewCapUsage(ctx.Context, change.ID)
		if usageErr != nil {
			return usageErr
		}
		used := reviewCapUsed(change, usage)
		head, headErr := reviewCapHead(ctx.Context, db, project, change)
		if headErr != nil {
			return headErr
		}
		if noticeErr := s.raiseReviewCapNotice(ctx.Context, db, project, change, head, used, maxRounds); noticeErr != nil {
			return noticeErr
		}
		return axi.Failure("review_round_cap", fmt.Sprintf("frozen Review and repair cap reached at %s (%d of %d rounds used)", shortSHA(head), used, maxRounds), false, "Wait for the User's `review_cap` Decision")
	}
	return err
}

func workspaceHasFullPipeline(plan *taskplan.Plan) bool {
	if plan == nil {
		return false
	}
	for _, member := range plan.Execution.Members {
		if member.Review == "full-pipeline" {
			return true
		}
	}
	return false
}

func (s *Service) prepareReviewCandidateCheckouts(ctx context.Context, db *store.DB, home string, project store.Project, change store.Task, plan *taskplan.Plan) error {
	if plan == nil {
		return nil
	}
	lanes, err := changedCandidateLanes(ctx, db, project, change)
	if err != nil {
		return err
	}
	for _, lane := range lanes {
		candidate, err := db.LatestValidationCandidate(ctx, change.ID, lane.repo)
		if err != nil || candidate.State != store.ValidationCandidateActive && candidate.State != store.ValidationCandidatePromoted {
			return axi.Failure("candidate_missing", lane.memberLabel("full-pipeline review requires a submitted private candidate"), false)
		}
		if candidate.State == store.ValidationCandidateActive {
			if err := s.ensureCandidateCheckout(ctx, db, home, lane.task, candidate); err != nil {
				return err
			}
		}
		targetHead, err := lane.targetHead(ctx)
		if err != nil {
			return err
		}
		if targetHead == "" || targetHead != candidate.TargetSHA {
			if candidate.State == store.ValidationCandidateActive && targetHead != "" {
				if err := db.MarkValidationCandidateStale(ctx, candidate.ID, targetHead); err != nil {
					return err
				}
			} else if targetHead != "" {
				if err := db.InvalidateValidationStages(ctx, candidate.ID, targetHead); err != nil {
					return err
				}
			}
			return axi.Failure("candidate_target_moved", lane.memberLabel("frozen target changed before Review admission"), false, "Submit a fresh candidate and rerun validation")
		}
	}
	return nil
}

func buildReviewRound(ctx context.Context, db *store.DB, project store.Project, change store.Task, plan taskplan.Plan, maxRounds int, home string) (*generatedReviewRound, error) {
	targets, err := changeCandidateTargets(ctx, db, project, change)
	if err != nil {
		return nil, axi.Failure("review_target_unresolved", "cannot pin the reviewed Change revision: "+err.Error(), false)
	}
	priorRounds, err := db.ReviewRoundsFor(ctx, change.ID)
	if err != nil {
		return nil, err
	}
	used := map[int]bool{}
	previous := map[string]store.ReviewRound{}
	for _, round := range priorRounds {
		if round.Status != "verdict" {
			continue
		}
		used[round.Round] = true
		current, found := previous[round.Repository]
		if !found || round.Round > current.Round {
			previous[round.Repository] = round
		}
	}
	roundNumber := len(used) + 1
	repositories, err := reviewRoundRepositories(ctx, db, project, change, targets)
	if err != nil {
		return nil, err
	}
	var scopes []reviewRoundScope
	for _, repository := range repositories {
		scope, err := calculateReviewRoundScope(ctx, repository, previous[repository.name], roundNumber, plan.ValidationLoops.Review.Guidance)
		if err != nil {
			return nil, err
		}
		scope.Binding.ChangeTaskID = change.ID
		scopes = append(scopes, scope)
	}
	if len(scopes) == 0 {
		return nil, axi.Failure("review_target_unresolved", "Change Task has no changed repository to review", false)
	}
	return renderReviewRound(ctx, db, change, plan, maxRounds, roundNumber, targets, scopes, filepath.Join(home, "projects", project.Name, "tasks"), project.IsWorkspace())
}

func reviewRoundRepositories(ctx context.Context, db *store.DB, project store.Project, change store.Task, targets []store.ReviewTarget) ([]reviewRoundRepository, error) {
	if !project.IsWorkspace() {
		if len(targets) != 1 || targets[0].Repository != project.Name {
			return nil, axi.Failure("review_target_unresolved", "Change candidate does not match its Project repository", false)
		}
		worktree := change.WorktreePath
		if isFullPipelineTask(change, "") {
			candidate, err := db.LatestValidationCandidate(ctx, change.ID, "")
			if err != nil || candidate.CandidateSHA != targets[0].Commit || candidate.State != store.ValidationCandidateActive && candidate.State != store.ValidationCandidatePromoted {
				return nil, axi.Failure("candidate_missing", "private candidate changed before Review admission", false)
			}
			worktree = candidate.WorktreePath
		}
		return []reviewRoundRepository{{name: project.Name, root: project.Root, worktree: worktree, targetBranch: taskTargetBranch(project, change), head: targets[0].Commit}}, nil
	}
	projectRepos, err := db.ProjectRepos(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]store.ProjectRepo, len(projectRepos))
	for _, repo := range projectRepos {
		byName[repo.Name] = repo
	}
	taskRepos, err := db.TaskRepos(ctx, change.ID)
	if err != nil {
		return nil, err
	}
	targetByName := make(map[string]string, len(targets))
	for _, target := range targets {
		targetByName[target.Repository] = target.Commit
	}
	var repositories []reviewRoundRepository
	for _, taskRepo := range taskRepos {
		if taskRepo.State == store.TaskRepoUnchanged {
			continue
		}
		repo, found := byName[taskRepo.Repo]
		head := targetByName[taskRepo.Repo]
		if !found || head == "" {
			return nil, axi.Failure("review_target_unresolved", "cannot resolve changed workspace Member "+taskRepo.Repo, false)
		}
		targetBranch := taskRepo.TargetBranch
		if targetBranch == "" {
			targetBranch = repo.DefaultBranch
		}
		worktree := taskRepo.WorktreePath
		if isFullPipelineTask(change, taskRepo.Repo) {
			candidate, err := db.LatestValidationCandidate(ctx, change.ID, taskRepo.Repo)
			if err != nil || candidate.CandidateSHA != head || candidate.State != store.ValidationCandidateActive && candidate.State != store.ValidationCandidatePromoted {
				return nil, axi.Failure("candidate_missing", "private candidate changed before Review admission for Member "+taskRepo.Repo, false)
			}
			worktree = candidate.WorktreePath
		}
		repositories = append(repositories, reviewRoundRepository{name: taskRepo.Repo, root: filepath.Join(project.Root, repo.Path), worktree: worktree, targetBranch: targetBranch, head: head, workspace: true})
	}
	sort.Slice(repositories, func(i, j int) bool { return repositories[i].name < repositories[j].name })
	return repositories, nil
}

func calculateReviewRoundScope(ctx context.Context, repository reviewRoundRepository, previous store.ReviewRound, round int, guidance []string) (reviewRoundScope, error) {
	if repository.root == "" || repository.worktree == "" || repository.targetBranch == "" || repository.head == "" {
		return reviewRoundScope{}, fmt.Errorf("incomplete review scope for repository %s", repository.name)
	}
	targetRef := "refs/heads/" + repository.targetBranch
	if _, err := gitOutput(ctx, repository.root, "remote", "get-url", "origin"); err == nil {
		targetRef = "refs/remotes/origin/" + repository.targetBranch
		if _, err := gitFetch(ctx, repository.root, "origin", repository.targetBranch+":"+targetRef); err != nil {
			return reviewRoundScope{}, axi.Failure("review_target_unavailable", "could not fetch frozen target origin/"+repository.targetBranch+" for "+repository.name, true, err.Error())
		}
	} else if _, err := gitOutput(ctx, repository.root, "rev-parse", "--verify", targetRef+"^{commit}"); err != nil {
		return reviewRoundScope{}, axi.Failure("review_target_unavailable", "frozen target branch "+repository.targetBranch+" is unavailable for "+repository.name, false, err.Error())
	}
	targetTip, err := gitOutput(ctx, repository.root, "rev-parse", "--verify", targetRef+"^{commit}")
	if err != nil {
		return reviewRoundScope{}, axi.Failure("review_target_unavailable", "cannot resolve the frozen target for "+repository.name, false, err.Error())
	}
	targetCommit, err := gitOutput(ctx, repository.root, "merge-base", repository.head, targetTip)
	if err != nil {
		return reviewRoundScope{}, axi.Failure("review_target_unavailable", "cannot find the frozen target base for "+repository.name, false, err.Error())
	}
	baselineCommit := targetCommit
	scopeBase := targetCommit
	scopeKind := "full"
	baselineTree, err := gitOutput(ctx, repository.root, "rev-parse", "--verify", baselineCommit+"^{tree}")
	if err != nil {
		return reviewRoundScope{}, err
	}
	var conflicts []string
	if previous.Round > 0 {
		scopeBase = previous.HeadSHA
		_, ancestorErr := gitOutput(ctx, repository.root, "merge-base", "--is-ancestor", previous.HeadSHA, repository.head)
		if ancestorErr != nil {
			scopeKind = "full_fallback"
		} else {
			scopeKind = "delta"
			_, containsErr := gitOutput(ctx, repository.root, "merge-base", "--is-ancestor", targetCommit, previous.HeadSHA)
			if containsErr == nil {
				baselineCommit = previous.HeadSHA
				baselineTree, err = gitOutput(ctx, repository.root, "rev-parse", "--verify", baselineCommit+"^{tree}")
			} else {
				baselineTree, conflicts, err = virtualMergeTree(ctx, repository.root, previous.HeadSHA, targetCommit)
			}
			if err != nil {
				return reviewRoundScope{}, err
			}
		}
	}
	if scopeKind == "full_fallback" {
		baselineCommit = targetCommit
		scopeBase = targetCommit
		baselineTree, err = gitOutput(ctx, repository.root, "rev-parse", "--verify", baselineCommit+"^{tree}")
		if err != nil {
			return reviewRoundScope{}, err
		}
	}
	conflictedPaths, err := json.Marshal(conflicts)
	if err != nil {
		return reviewRoundScope{}, err
	}
	diffArgs := []string{"diff", "--binary", "--no-ext-diff"}
	if repository.workspace {
		prefix := repository.name + "/"
		diffArgs = append(diffArgs, "--src-prefix=a/"+prefix, "--dst-prefix=b/"+prefix)
	}
	deltaArgs := append(append([]string(nil), diffArgs...), baselineTree, repository.head)
	delta, err := gitOutputRaw(ctx, repository.root, deltaArgs...)
	if err != nil {
		return reviewRoundScope{}, err
	}
	cumulativeArgs := append(append([]string(nil), diffArgs...), targetCommit, repository.head)
	cumulative, err := gitOutputRaw(ctx, repository.root, cumulativeArgs...)
	if err != nil {
		return reviewRoundScope{}, err
	}
	digest := sha256.Sum256([]byte(delta))
	guidanceFiles, err := readReviewGuidance(ctx, repository.root, targetTip, repository.targetBranch, guidance)
	if err != nil {
		return reviewRoundScope{}, err
	}
	return reviewRoundScope{
		Binding: store.ReviewRound{
			Round: round, Repository: repository.name, ScopeKind: scopeKind, ScopeBaseSHA: scopeBase,
			HeadSHA: repository.head, TargetCommitSHA: targetCommit, BaselineTree: baselineTree,
			DeltaSHA256: hex.EncodeToString(digest[:]), ConflictedPaths: string(conflictedPaths), Status: "admitted",
		},
		DeltaPatch: []byte(delta), Cumulative: []byte(cumulative), Conflicts: conflicts,
		Guidance: guidanceFiles, TargetBranch: repository.targetBranch,
	}, nil
}

func virtualMergeTree(ctx context.Context, root, reviewed, target string) (string, []string, error) {
	output, err := commandStdoutArgs(ctx, "", "git", "-C", root, "merge-tree", "--write-tree", "--name-only", "-z", reviewed, target)
	trimmed := strings.TrimLeft(output, "\n\r\t ")
	separator := strings.IndexAny(trimmed, "\n\x00")
	if separator < 0 {
		return "", nil, fmt.Errorf("git merge-tree did not return a virtual baseline tree: %w", err)
	}
	tree := strings.TrimSpace(trimmed[:separator])
	if len(tree) != 40 && len(tree) != 64 {
		return "", nil, fmt.Errorf("git merge-tree returned invalid tree id %q", tree)
	}
	if _, verifyErr := gitOutput(ctx, root, "cat-file", "-e", tree+"^{tree}"); verifyErr != nil {
		return "", nil, fmt.Errorf("git merge-tree returned an unavailable tree %s: %w", tree, verifyErr)
	}
	rest := trimmed[separator+1:]
	pathsEnd := strings.Index(rest, "\x00\x00")
	if pathsEnd < 0 {
		pathsEnd = strings.Index(rest, "\n\n")
	}
	var paths []string
	if pathsEnd >= 0 {
		pathSection := rest[:pathsEnd]
		if strings.Contains(pathSection, "\x00") {
			paths = strings.Split(pathSection, "\x00")
		} else if pathSection != "" {
			paths = strings.Split(pathSection, "\n")
		}
	}
	conflictSet := make(map[string]bool, len(paths))
	for _, candidate := range paths {
		candidate = strings.Trim(candidate, "\r\n")
		if candidate != "" {
			conflictSet[candidate] = true
		}
	}
	conflicts := make([]string, 0, len(conflictSet))
	for candidate := range conflictSet {
		conflicts = append(conflicts, candidate)
	}
	sort.Strings(conflicts)
	if err != nil && len(conflicts) == 0 {
		// A conflict can have no path output for unusual merge drivers, but a
		// completed virtual tree is still a valid baseline. Other failures do
		// not return a tree object and were rejected above.
		if strings.Contains(strings.ToLower(err.Error()), "exit status 1") {
			return tree, conflicts, nil
		}
	}
	return tree, conflicts, nil
}

const reviewDirPlaceholder = "{{review_dir}}"

// The Review Task's sequence is unknown until admission, so the generated
// brief names its files through a placeholder resolved once the Task home exists.
func withReviewDir(text, taskHome string) string {
	return strings.ReplaceAll(text, reviewDirPlaceholder, filepath.Join(taskHome, "review"))
}

func readReviewGuidance(ctx context.Context, root, targetRef, targetBranch string, paths []string) (map[string][]byte, error) {
	files := make(map[string][]byte, len(paths))
	for _, name := range paths {
		clean := path.Clean(strings.ReplaceAll(name, "\\", "/"))
		if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." || strings.HasPrefix(clean, "/") || strings.Contains(name, "\\") {
			return nil, axi.Failure("review_guidance_invalid", fmt.Sprintf("guidance path %q is not a safe repository-relative path", name), false)
		}
		data, err := commandStdoutArgs(ctx, "", "git", "-C", root, "show", targetRef+":"+clean)
		if err != nil {
			return nil, axi.Failure("review_guidance_unavailable", "cannot read guidance path "+clean+" from origin/"+targetBranch, false, err.Error())
		}
		files[clean] = []byte(data)
	}
	return files, nil
}

func renderReviewRound(ctx context.Context, db *store.DB, change store.Task, plan taskplan.Plan, maxRounds, round int, targets []store.ReviewTarget, scopes []reviewRoundScope, projectTasksPath string, workspace bool) (*generatedReviewRound, error) {
	files := map[string][]byte{}
	var roundEntries []reviewRoundJSONEntry
	var delta, cumulative, conflictText strings.Builder
	guidanceRoot := "review/guidance"
	for _, scope := range scopes {
		binding := scope.Binding
		paths := scope.Conflicts
		delta.Write(scope.DeltaPatch)
		cumulative.Write(scope.Cumulative)
		for _, conflict := range paths {
			fmt.Fprintf(&conflictText, "%s:%s\n", binding.Repository, conflict)
		}
		for name, contents := range scope.Guidance {
			relative := name
			if workspace {
				relative = filepath.Join(binding.Repository, filepath.FromSlash(name))
			}
			files[filepath.ToSlash(filepath.Join(guidanceRoot, relative))] = contents
		}
		roundEntries = append(roundEntries, reviewRoundJSONEntry{
			Repository: binding.Repository, TargetBranch: scope.TargetBranch, ScopeKind: binding.ScopeKind,
			ScopeBaseSHA: binding.ScopeBaseSHA, HeadSHA: binding.HeadSHA, TargetCommitSHA: binding.TargetCommitSHA,
			BaselineTree: binding.BaselineTree, DeltaSHA256: binding.DeltaSHA256, ConflictedPaths: paths,
		})
	}
	ledger, err := reviewLedgerMarkdown(ctx, db, change.ID)
	if err != nil {
		return nil, err
	}
	remainingRounds := maxRounds - round
	if remainingRounds < 0 {
		remainingRounds = 0
	}
	roundContents, err := json.MarshalIndent(reviewRoundJSON{Round: round, MaxRounds: maxRounds, RemainingRounds: remainingRounds, Repositories: roundEntries}, "", "  ")
	if err != nil {
		return nil, err
	}
	files["review/round.json"] = append(roundContents, '\n')
	files["review/delta.patch"] = []byte(delta.String())
	files["review/cumulative.patch"] = []byte(cumulative.String())
	files["review/conflicts.txt"] = []byte(conflictText.String())
	files["review/ledger.md"] = []byte(ledger)
	briefData := generatedReviewBrief(change, plan, round, scopes, workspace)
	briefPath := filepath.Join(projectTasksPath, "review-rounds", fmt.Sprintf("t%d-round-%d.md", change.Seq, round))
	bindings := store.ReviewRoundAdmission{
		ChangeTaskID: change.ID, Round: round, MaxRounds: maxRounds,
		SharedRepairBudget: planUsesSharedReviewRepairCap(&plan),
	}
	for _, scope := range scopes {
		bindings.Bindings = append(bindings.Bindings, scope.Binding)
	}
	return &generatedReviewRound{Admission: bindings, Targets: targets, BriefData: briefData, BriefPath: briefPath, Files: files}, nil
}

func reviewLedgerMarkdown(ctx context.Context, db *store.DB, changeTaskID int64) (string, error) {
	entries, err := db.ReviewLedgerFor(ctx, changeTaskID)
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	builder.WriteString("# Finding Ledger\n")
	if len(entries) == 0 {
		builder.WriteString("\nNo findings have been recorded yet.\n")
	} else {
		builder.WriteString("\n| Finding | Pattern | State | Severity | Fingerprint |\n| --- | --- | --- | --- | --- |\n")
		for _, entry := range entries {
			fmt.Fprintf(&builder, "| F-%d | P-%d `%s` | %s | %s | `%s` |\n", entry.FindingSeq, entry.PatternSeq, entry.PatternKey, entry.State, entry.Severity, entry.Fingerprint)
		}
	}
	builder.WriteString("\n## Patterns claimed fixed since the previous verdict\n")
	claimed := false
	for _, entry := range entries {
		if entry.State == "fix_claimed" {
			fmt.Fprintf(&builder, "- P-%d `%s` (F-%d)\n", entry.PatternSeq, entry.PatternKey, entry.FindingSeq)
			claimed = true
		}
	}
	if !claimed {
		builder.WriteString("\nNone.\n")
	}
	return builder.String(), nil
}

func generatedReviewBrief(change store.Task, plan taskplan.Plan, round int, scopes []reviewRoundScope, workspace bool) []byte {
	var builder strings.Builder
	fmt.Fprintf(&builder, "---\ntype: review\ntitle: Review round %d of t%d\ndone_when: the round records an anchored review verdict\nreview_of: t%d\n", round, change.Seq, change.Seq)
	if workspace {
		var names []string
		for _, scope := range scopes {
			names = append(names, scope.Binding.Repository)
		}
		sort.Strings(names)
		fmt.Fprintf(&builder, "repos: [%s]\n", strings.Join(names, ", "))
	}
	builder.WriteString("---\n")
	fmt.Fprintf(&builder, "Review this candidate as a numbered Review Round.\n\n## Review Round %d\n\nReview only findings in `{{review_dir}}/delta.patch`, using `{{review_dir}}/cumulative.patch` for context. The immutable bindings are in `{{review_dir}}/round.json`; conflicts are listed in `{{review_dir}}/conflicts.txt`; the Finding Ledger is in `{{review_dir}}/ledger.md`. Repository guidance is copied into `{{review_dir}}/guidance/` from the frozen target's origin ref, never from the Change branch. Do not modify the checkout.\n\nFinish by writing a JSON file in this Task's scratch directory using this schema, then signal done with `--report <report-file> --verdict <pass|changes_requested|inconclusive> --findings <findings-file>`. A pass must have no blocking open findings; Posse derives and checks the verdict. Use an empty patterns array for no findings.\n\n```json\n{\n  \"schema\": \"posse.review-findings.v1\",\n  \"patterns\": [{\n    \"key\": \"unchecked-error-return\",\n    \"title\": \"Store write errors are dropped\",\n    \"rule\": \"Every store write error reaches the caller\",\n    \"root_cause\": \"New helpers ignore database errors\",\n    \"fix_guidance\": \"Check every new write call in the affected package\",\n    \"severity\": \"P1\",\n    \"origin\": \"introduced\",\n    \"instances\": [{\n      \"repository\": \"posse\", \"path\": \"internal/app/x.go\", \"line\": 42,\n      \"symbol\": \"saveRound\", \"excerpt\": \"db.Exec(ctx, q)\",\n      \"description\": \"The returned error is ignored\", \"cause\": null,\n      \"matches\": null, \"regresses\": null\n    }]\n  }],\n  \"resolutions\": []\n}\n```\n\nEach excerpt must occur in its repository file at the bound head after whitespace normalization. Use `cause: \"path:line\"` only to cite an added line in `{{review_dir}}/delta.patch` that caused an interaction finding. `matches` and `regresses` use existing IDs such as `F-3`. Include one pattern per root cause, with every located instance. Missing or unknown severity is treated as unclassified and blocks. For every ledger finding claimed fixed, include a resolution with status `resolved` or `unresolved`; omitted answers remain unverified.\n\nScope check: every hunk in `{{review_dir}}/delta.patch` must trace to a `done_when` item of the Change Brief below, whatever skills are loaded. Record each hunk that traces to no `done_when` item, including unrelated refactors, separate bug fixes, debug prints, whitespace and docstring churn, as a finding with key `out-of-scope-hunk`, severity `P1` and origin `introduced`.\n\n", round)
	for _, input := range plan.Inputs {
		if input.Role != "brief" {
			continue
		}
		label, text := briefFixerSection(&plan)
		if label == "Intent (User words verbatim; Goal and Lead assumptions are Lead text)" {
			fmt.Fprintf(&builder, "## %s\n\n%s\n\n## Change Brief (Lead text)\n\n%s", label, text, withoutBriefSection(string(input.Bytes), "Intent"))
		} else {
			fmt.Fprintf(&builder, "## Change Brief (verbatim)\n\n%s", input.Bytes)
		}
		break
	}
	return []byte(builder.String())
}

func reviewRoundPlanInputs(files map[string][]byte) []taskplan.Input {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	inputs := make([]taskplan.Input, 0, len(names))
	for _, name := range names {
		inputs = append(inputs, taskplan.Select("review-round-file", name, files[name]))
	}
	return inputs
}

func writeReviewRoundFiles(taskHome string, files map[string][]byte) error {
	if err := os.MkdirAll(filepath.Join(taskHome, "review", "guidance"), 0o700); err != nil {
		return err
	}
	for name, contents := range files {
		clean := path.Clean(filepath.ToSlash(name))
		if clean == "review" || !strings.HasPrefix(clean, "review/") || path.IsAbs(clean) || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("invalid generated Review Round file path %q", name)
		}
		destination := filepath.Join(taskHome, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(destination, contents, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func mapReviewRoundAdmissionError(err error) error {
	switch {
	case errors.Is(err, store.ErrReviewRoundActive):
		return axi.Failure("review_round_active", "another Review Round is already active for this Change Task", false, "Wait for the active Review Task to finish or become void")
	case errors.Is(err, store.ErrReviewRoundUnchanged):
		return axi.Failure("review_round_unchanged", "the Change head is unchanged since its last verdict or is already covered by a carry", false)
	case errors.Is(err, store.ErrReviewRoundCap):
		return axi.Failure("review_round_cap", "the frozen Review Round cap has been reached", false)
	case errors.Is(err, store.ErrReviewRoundNumber):
		return axi.Failure("review_round_changed", "Review Round state changed during admission", true, "Retry `posse review start`; Posse will recompute the next round")
	default:
		return err
	}
}

func autoStartReviewLoop(plan *taskplan.Plan) bool {
	return plan != nil && plan.Review != "none" && plan.ValidationLoops != nil && plan.ValidationLoops.Review.Enabled && plan.ValidationLoops.Review.AutoStart
}

func autoStartReviewerUnavailable(err error) (*axi.Error, bool) {
	var failure *axi.Error
	if !errors.As(err, &failure) {
		return nil, false
	}
	switch failure.Code {
	case "review_identity_ineligible", "profile_required", "profile_unknown", "kind_disabled":
		return failure, true
	default:
		return failure, false
	}
}

// autoStartReviewProfile returns the Profile auto-start passes to `posse
// review start`: empty when one Dispatch Rule resolves an independent
// reviewer, otherwise the first candidate in config order whose identity
// differs from every author launch. The Review spawn rechecks identity.
func (s *Service) autoStartReviewProfile(ctx context.Context, db *store.DB, home string, project store.Project, change store.Task) (string, error) {
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return "", err
	}
	resolution, resolveErr := s.resolveDispatch(ctx, project, cfg, "review", "")
	var profileErr *dispatch.ProfileError
	if resolveErr != nil && (!errors.As(resolveErr, &profileErr) || profileErr.Code != "profile_required" || len(profileErr.Candidates) == 0) {
		return "", profileError(resolveErr)
	}
	authorLaunches, err := db.TaskLaunchIdentities(ctx, change.ID)
	if err != nil {
		return "", err
	}
	authorKnown := change.Launches > 0 && launchHistoryComplete(change, authorLaunches)
	ineligible := func(resolution dispatch.Resolution) string {
		kind, found := cfg.Kinds[resolution.Resolved.Kind]
		if !found {
			return "unknown agent kind " + resolution.Resolved.Kind
		}
		args := workerAgentArgs(resolution.Resolved, kind, "")
		var reasons []string
		if check := compareReviewIdentity(authorLaunches, authorKnown, configuredLaunchIdentity(resolution.Profile, resolution.Resolved, kind, args)); !check.Eligible {
			reasons = append(reasons, check.Reason)
		}
		if refusal := reviewSessionOverrideReason(resolution.Resolved.Kind, args); refusal != "" {
			reasons = append(reasons, refusal)
		}
		return strings.Join(reasons, "; ")
	}
	var rejected []string
	if resolveErr == nil {
		if reason := ineligible(resolution); reason != "" {
			rejected = append(rejected, fmt.Sprintf("%s (%s)", resolution.Profile, reason))
		}
	} else {
		for _, candidate := range profileErr.Candidates {
			reason := ""
			if candidateResolution, err := s.resolveDispatch(ctx, project, cfg, "review", candidate.Profile); err != nil {
				reason = err.Error()
			} else {
				reason = ineligible(candidateResolution)
			}
			if reason == "" {
				return candidate.Profile, nil
			}
			rejected = append(rejected, fmt.Sprintf("%s (%s)", candidate.Profile, reason))
		}
	}
	if len(rejected) > 0 {
		return "", axi.Failure("review_identity_ineligible", "no reviewer candidate is independent of every author launch: "+strings.Join(rejected, ", "), false)
	}
	return "", nil
}

// recordAutoStartReviewerReadiness raises one Notice per reviewed head, so a
// new head that is still skipped reaches the Lead again.
func recordAutoStartReviewerReadiness(ctx context.Context, db *store.DB, project store.Project, change store.Task, targets []store.ReviewTarget, failure *axi.Error) error {
	heads := make([]string, len(targets))
	for i, target := range targets {
		heads[i] = target.Repository + "@" + shortSHA(target.Commit)
	}
	head := strings.Join(heads, ", ")
	var found bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notices WHERE project_id=? AND task_id=? AND kind='review_auto_start_skipped' AND json_valid(data_json) AND json_extract(data_json,'$.head')=?)`, project.ID, change.ID, head).Scan(&found); err != nil || found {
		return err
	}
	summary := fmt.Sprintf("%s: Review auto-start skipped at %s because no eligible reviewer is available: %s. Configure an independent reviewer Profile, then run `posse review start %s --profile <profile>`.", change.Title, head, failure.Message, taskIDString(change.Seq))
	data := marshalJSON(map[string]any{"head": head, "reason_code": failure.Code, "reason": failure.Message, "help": failure.Help})
	_, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: change.ID, Kind: "review_auto_start_skipped", Summary: summary, DataJSON: data})
	return err
}

func (s *Service) maybeAutoStartReviewRound(ctx *axi.Context, db *store.DB, home string, project store.Project, change store.Task) (bool, error) {
	plan, err := change.AdmittedPlan()
	if err != nil {
		return false, admittedPlanFailure(err)
	}
	if !autoStartReviewLoop(plan) {
		return false, nil
	}
	targets, err := changeCandidateTargets(ctx.Context, db, project, change)
	if err != nil {
		return false, err
	}
	if project.IsWorkspace() {
		taskRepos, err := db.TaskRepos(ctx.Context, change.ID)
		if err != nil {
			return false, err
		}
		gated := make(map[string]string, len(taskRepos))
		for _, repo := range taskRepos {
			gated[repo.Repo] = repo.GatedSHA
		}
		for _, target := range targets {
			if gated[target.Repository] == "" || gated[target.Repository] != target.Commit {
				return false, nil
			}
		}
	} else if len(targets) != 1 || change.GatedSHA == "" || targets[0].Commit != change.GatedSHA {
		return false, nil
	}
	if covered, err := db.ReviewTargetsCovered(ctx.Context, change.ID, targets); err != nil || covered {
		return false, err
	}
	profile, err := s.autoStartReviewProfile(ctx.Context, db, home, project, change)
	if err == nil {
		err = s.startReviewRound(ctx, db, home, project, change, profile, "")
	}
	if failure, unavailable := autoStartReviewerUnavailable(err); unavailable {
		if noticeErr := recordAutoStartReviewerReadiness(ctx.Context, db, project, change, targets, failure); noticeErr != nil {
			return false, noticeErr
		}
		return false, nil
	}
	var failure *axi.Error
	if errors.As(err, &failure) && (failure.Code == "review_round_unchanged" || failure.Code == "review_round_active") {
		return false, nil
	}
	return err == nil, err
}
