// Package taskplan resolves the supported Brief contract into a versioned,
// self-contained execution plan. It performs no I/O and infers no provider identity.
package taskplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
)

const Version = 5

var workflowRunIDPattern = regexp.MustCompile(`^w[1-9][0-9]*$`)

// planVersionCurrentBriefContract marks the first plan version written with
// the current Brief vocabulary and semantics.
const planVersionCurrentBriefContract = 3

const (
	RootContextNone     = "none"
	RootContextReadOnly = "read-only"
)

// Input contains only bytes consumed by the contract, not an entire forge thread
// or configuration file. Bytes are base64 encoded by JSON, preserving CRLF and
// non-UTF-8 input without normalization.
type Input struct {
	Role   string `json:"role"`
	Source string `json:"source"`
	Bytes  []byte `json:"bytes"`
	SHA256 string `json:"sha256"`
	Layer  string `json:"layer,omitempty"`
	Legacy bool   `json:"legacy,omitempty"`
}

func Select(role, source string, data []byte) Input {
	sum := sha256.Sum256(data)
	return Input{Role: role, Source: source, Bytes: append([]byte{}, data...), SHA256: hex.EncodeToString(sum[:])}
}

type Grant struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

type Preference struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// UserChoice records consent for this selected Brief, not authentication or an
// effect grant. Fields contains only explicitly selected workflow preferences.
type UserChoice struct {
	Quote       string   `json:"quote"`
	Actor       string   `json:"actor"`
	Source      string   `json:"source"`
	BriefSHA256 string   `json:"brief_sha256"`
	Fields      []string `json:"fields"`
}

func preferenceFields(brief dispatch.Brief) []string {
	var fields []string
	if brief.MergeMode != "" {
		fields = append(fields, "delivery")
	}
	if brief.Review != "" {
		fields = append(fields, "review")
	}
	if brief.Base != "" || len(brief.BaseByRepo) > 0 {
		fields = append(fields, "base")
	}
	if brief.Target != "" || len(brief.TargetByRepo) > 0 {
		fields = append(fields, "target")
	}
	if brief.Execution != "" {
		fields = append(fields, "execution")
	}
	if len(brief.Paths) > 0 {
		fields = append(fields, "paths")
	}
	return fields
}

type Member struct {
	Review       string `json:"review,omitempty"`
	ReviewSource string `json:"review_source,omitempty"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	Base         string `json:"base"`
	BaseCommit   string `json:"base_commit,omitempty"`
	Target       string `json:"target"`
	Delivery     string `json:"delivery"`
}

type Execution struct {
	Mode        string   `json:"mode"`
	Scope       string   `json:"scope"`
	Base        string   `json:"base"`
	BaseCommit  string   `json:"base_commit,omitempty"`
	Target      string   `json:"target"`
	RootContext string   `json:"root_context,omitempty"`
	Members     []Member `json:"members,omitempty"`
	Paths       []string `json:"paths,omitempty"`
}

type Launch struct {
	ProfileName     string         `json:"profile_name"`
	DispatchRule    string         `json:"dispatch_rule"`
	Profile         config.Profile `json:"profile"`
	Kind            config.Kind    `json:"kind"`
	Args            []string       `json:"args"`
	ConfiguredModel string         `json:"configured_model"`
	ModelKnown      bool           `json:"model_known"`
}

type Paths struct {
	Scratch string `json:"scratch"`
	Output  string `json:"output"`
}

type ValidationLoops struct {
	Review            config.ReviewLoop `json:"review_loop"`
	CI                config.CILoop     `json:"ci_loop"`
	BriefReviewRounds int               `json:"brief_review_rounds,omitempty"`
}

func (loops ValidationLoops) MaxReviewRounds() int {
	if loops.BriefReviewRounds > 0 {
		return loops.BriefReviewRounds
	}
	return loops.Review.MaxRounds
}

type Plan struct {
	// Original bytes retain the digest and provenance of admitted 0.14 plans.
	legacyJSON      string
	Version         int                       `json:"version"`
	Revision        int                       `json:"revision"`
	PreviousSHA256  string                    `json:"previous_sha256,omitempty"`
	Type            string                    `json:"type"`
	Outcome         string                    `json:"outcome"`
	Executor        string                    `json:"executor"`
	Execution       Execution                 `json:"execution"`
	Review          string                    `json:"review"`
	ReviewOf        int64                     `json:"review_of,omitempty"`
	Dependencies    []dispatch.Dependency     `json:"dependencies,omitempty"`
	Delivery        string                    `json:"delivery"`
	Grants          []Grant                   `json:"grants"`
	Preferences     []Preference              `json:"preferences,omitempty"`
	UserChoice      *UserChoice               `json:"user_choice,omitempty"`
	Inputs          []Input                   `json:"inputs"`
	Paths           Paths                     `json:"paths"`
	Launch          Launch                    `json:"launch"`
	WorkflowJobs    bool                      `json:"workflow_jobs,omitempty"`
	ValidationLoops *ValidationLoops          `json:"validation_loops,omitempty"`
	Workflow        *config.WorkflowSelection `json:"workflow,omitempty"`
	MergeMethod     string                    `json:"merge_method"`
	ProseLanguage   string                    `json:"prose_language,omitempty"`
	Pool            config.Pool               `json:"pool"`
}

type Request struct {
	Brief           Input
	Inputs          []Input
	Execution       Execution
	Launch          Launch
	Review          string
	ReviewOf        int64
	Delivery        string
	Autonomy        config.Autonomy
	ConfigSources   []string
	UserChoice      *UserChoice
	Paths           Paths
	WorkflowJobs    bool
	ValidationLoops *ValidationLoops
	Workflow        *config.WorkflowSelection
	MergeMethod     string
	ProseLanguage   string
	Pool            config.Pool
}

// Resolve interprets the admitted Brief contract after callers resolve repository facts.
func Resolve(r Request) (Plan, error) {
	brief, err := dispatch.ParseBriefText(string(r.Brief.Bytes))
	if err != nil {
		return Plan{}, err
	}
	var validationLoops *ValidationLoops
	if brief.Type == "change" && r.ValidationLoops != nil {
		frozen := *r.ValidationLoops
		frozen.Review.Guidance = append([]string(nil), frozen.Review.Guidance...)
		if brief.ReviewRounds > frozen.Review.MaxRounds {
			return Plan{}, &dispatch.BriefError{Field: "review_rounds", Reason: "cannot exceed the configured review_loop.max_rounds"}
		}
		frozen.BriefReviewRounds = brief.ReviewRounds
		validationLoops = &frozen
	} else if brief.ReviewRounds > 0 {
		return Plan{}, &dispatch.BriefError{Field: "review_rounds", Reason: "cannot be frozen without review loop configuration"}
	}
	userChoice := r.UserChoice != nil
	delivery, autonomy, err := dispatch.ResolveDelivery(brief, r.Delivery, r.Autonomy, userChoice)
	if err != nil {
		return Plan{}, err
	}
	// Workspace tightening is checked against every Member below, not against
	// a Project default that the selected Members may all override.
	review, err := dispatch.ResolveReview(brief.Review, r.Review, userChoice || r.Execution.Scope == "workspace")
	if err != nil {
		return Plan{}, err
	}
	outcome := "report"
	if brief.Type == "change" {
		outcome = "change"
	}
	source := "effective config: " + strings.Join(r.ConfigSources, ", ")
	reviewSource, mergeSource := source, source
	if brief.AutonomyReview != "" {
		reviewSource = "brief: " + r.Brief.Source
	}
	if brief.AutonomyMerge != "" {
		mergeSource = "brief: " + r.Brief.Source
	}
	if brief.Execution != "" {
		r.Execution.Mode = brief.Execution
	}
	r.Execution.Paths = append([]string(nil), brief.Paths...)
	if r.Execution.Scope == "workspace" && r.Execution.RootContext == "" {
		r.Execution.RootContext = brief.RootContext
		if r.Execution.RootContext == "" {
			r.Execution.RootContext = RootContextNone
		}
	}
	p := Plan{Version: Version, Revision: 1, Type: brief.Type, Outcome: outcome, Executor: "worker",
		Execution: r.Execution, Review: review, ReviewOf: r.ReviewOf, Delivery: delivery,
		Grants: []Grant{{Name: "autonomy.review", Value: autonomy.Review, Source: reviewSource}, {Name: "autonomy.merge", Value: autonomy.Merge, Source: mergeSource}},
		Inputs: append([]Input{r.Brief}, r.Inputs...), Paths: r.Paths, Launch: r.Launch,
		WorkflowJobs: r.WorkflowJobs && brief.Type == "change" && review == "full-pipeline", ValidationLoops: validationLoops, Workflow: r.Workflow, MergeMethod: r.MergeMethod, ProseLanguage: r.ProseLanguage, Pool: r.Pool}
	p.Preferences = []Preference{{Name: "delivery", Value: p.Delivery, Source: source}, {Name: "review", Value: p.Review, Source: source}}
	for i, explicit := range []bool{brief.MergeMode != "", brief.Review != ""} {
		if explicit {
			p.Preferences[i].Source = "brief: " + r.Brief.Source
		}
		if explicit && userChoice {
			p.Preferences[i].Source = "User choice: " + r.UserChoice.Source
		}
	}
	if userChoice {
		choice := *r.UserChoice
		choice.BriefSHA256, choice.Fields = r.Brief.SHA256, preferenceFields(brief)
		p.UserChoice = &choice
	}
	p.Execution.Members = append([]Member(nil), p.Execution.Members...)
	for i := range p.Execution.Members {
		member := &p.Execution.Members[i]
		current := member.Review
		if current == "" {
			current = r.Review
		}
		member.Review, err = dispatch.ResolveReview(brief.Review, current, userChoice)
		if err != nil {
			return Plan{}, fmt.Errorf("member %s: %w", member.Name, err)
		}
		if member.ReviewSource == "" {
			member.ReviewSource = source
		}
		if brief.Review != "" {
			member.ReviewSource = p.Preferences[1].Source
		}
	}
	if p.Execution.Mode == "" {
		p.Execution.Mode = "sandbox"
	}
	// Snapshot interpreted configuration fields consumed by the contract, not
	// entire TOML files, other Profiles or unrelated forge content.
	settings, err := json.Marshal(struct {
		Execution       Execution                 `json:"execution"`
		Review          string                    `json:"review"`
		Delivery        string                    `json:"delivery"`
		Grants          []Grant                   `json:"grants"`
		WorkflowJobs    bool                      `json:"workflow_jobs,omitempty"`
		ValidationLoops *ValidationLoops          `json:"validation_loops,omitempty"`
		Workflow        *config.WorkflowSelection `json:"workflow,omitempty"`
		MergeMethod     string                    `json:"merge_method"`
		ProseLanguage   string                    `json:"prose_language"`
		Pool            config.Pool               `json:"pool"`
	}{p.Execution, p.Review, p.Delivery, p.Grants, p.WorkflowJobs, p.ValidationLoops, p.Workflow, p.MergeMethod, p.ProseLanguage, p.Pool})
	if err != nil {
		return Plan{}, err
	}
	p.Inputs = append(p.Inputs, Select("execution-settings", source, settings))
	if err := p.Validate(); err != nil {
		return Plan{}, err
	}
	// Detach slices and maps from mutable configuration owned by the caller.
	data, err := json.Marshal(p)
	if err != nil {
		return Plan{}, err
	}
	return Decode(string(data))
}

func (p Plan) Validate() error {
	invalid := func(message string) error { return fmt.Errorf("invalid execution plan: %s", message) }
	if p.Version < 1 || p.Version > Version || p.Revision < 1 {
		return invalid("unsupported version or revision")
	}
	if p.Revision == 1 && p.PreviousSHA256 != "" || p.Revision > 1 && len(p.PreviousSHA256) != 64 {
		return invalid("invalid previous revision digest")
	}
	if p.Executor != "worker" || p.Execution.Mode != "sandbox" && p.Execution.Mode != "in-place" {
		return invalid("unsupported executor or execution mode")
	}
	if p.Execution.Scope != "repo" && p.Execution.Scope != "workspace" && p.Execution.Scope != "directory" {
		return invalid("unsupported scope")
	}
	if p.Type != "change" && p.Type != "research" && p.Type != "review" {
		return invalid("unsupported Task type")
	}
	outcome := "report"
	if p.Type == "change" {
		outcome = "change"
	}
	if p.Outcome != outcome {
		return invalid("outcome does not match Task type")
	}
	if p.Type == "review" && p.ReviewOf < 1 || p.Type != "review" && p.ReviewOf != 0 {
		return invalid("review target does not match Task type")
	}
	if p.Version == 1 {
		if !config.LegacyReview(p.Review) {
			return invalid("unsupported legacy review policy")
		}
	} else if p.Version < 4 {
		if err := config.ValidateHistoricalReview(p.Review); err != nil {
			return invalid(err.Error())
		}
	} else if err := config.ValidateReview(p.Review); err != nil {
		return invalid(err.Error())
	}
	if p.Delivery != "local" && p.Delivery != "pr" && p.Delivery != "no-mistakes" {
		return invalid("unsupported delivery")
	}
	if p.Execution.Scope == "workspace" && p.Delivery == "no-mistakes" {
		return invalid("no-mistakes delivery is unsupported for Workspaces")
	}
	if p.Version >= 4 && p.Review == "full-pipeline" && p.Delivery == "no-mistakes" {
		return invalid("full-pipeline cannot use no-mistakes delivery")
	}
	if p.Execution.Mode == "in-place" {
		if p.Execution.Scope != "directory" || len(p.Execution.Paths) == 0 || p.Delivery != "local" {
			return invalid("in-place execution requires a local directory scope and selected paths")
		}
		if p.Execution.Base != "" || p.Execution.BaseCommit != "" || p.Execution.Target != "" || p.Execution.RootContext != "" || len(p.Execution.Members) != 0 || p.Type == "review" {
			return invalid("in-place execution cannot select Git branches, workspace context, Members or Review revisions")
		}
		expectedReview := "none"
		if p.Version == 1 {
			expectedReview = "never"
		}
		if p.Review != expectedReview {
			return invalid("in-place execution cannot request Review")
		}
	} else if p.Execution.Scope == "directory" || len(p.Execution.Paths) != 0 {
		return invalid("directory scope and paths require in-place execution")
	}
	if p.Execution.Scope == "repo" && len(p.Execution.Members) != 0 {
		return invalid("repository execution cannot select Members")
	}
	if p.Execution.Scope == "repo" && p.Execution.RootContext != "" && p.Execution.RootContext != RootContextNone {
		return invalid("repository execution cannot select workspace root context")
	}
	if p.Execution.Scope == "workspace" && p.Execution.RootContext != "" && p.Execution.RootContext != RootContextNone && p.Execution.RootContext != RootContextReadOnly {
		return invalid("unsupported workspace root context")
	}
	if p.Execution.Scope == "workspace" && p.Type == "change" && len(p.Execution.Members) == 0 {
		return invalid("workspace Change requires Members")
	}
	if p.Execution.Mode == "in-place" {
		seen := map[string]bool{}
		for _, path := range p.Execution.Paths {
			if path == "" || !filepath.IsLocal(path) || filepath.Clean(path) != path || seen[path] {
				return invalid("invalid or duplicate in-place path scope")
			}
			for previous := range seen {
				if path == "." || previous == "." || strings.HasPrefix(path, previous+string(filepath.Separator)) || strings.HasPrefix(previous, path+string(filepath.Separator)) {
					return invalid("overlapping in-place path scopes")
				}
			}
			for _, component := range strings.Split(filepath.ToSlash(path), "/") {
				if component == ".git" {
					return invalid("in-place paths cannot include Git metadata")
				}
			}
			seen[path] = true
		}
	}
	validCommit := func(value string) bool {
		if value == "" {
			return true
		}
		_, err := hex.DecodeString(value)
		return err == nil && (len(value) == 40 || len(value) == 64)
	}
	if !validCommit(p.Execution.BaseCommit) {
		return invalid("invalid pinned base commit")
	}
	seenNames, seenPaths := map[string]bool{}, map[string]bool{}
	for _, member := range p.Execution.Members {
		if member.Name == "" || seenNames[member.Name] || member.Path == "" || !filepath.IsLocal(member.Path) || member.Path == "." || seenPaths[member.Path] {
			return invalid("invalid or duplicate Member")
		}
		if !validCommit(member.BaseCommit) {
			return invalid("invalid pinned Member base commit")
		}
		if p.Version >= 2 {
			validateReview := config.ValidateReview
			if p.Version < 4 {
				validateReview = config.ValidateHistoricalReview
			}
			if err := validateReview(member.Review); err != nil {
				return invalid("Member " + member.Name + ": " + err.Error())
			}
			if member.ReviewSource == "" {
				return invalid("missing Member review provenance")
			}
		}
		if member.Delivery != "local" && member.Delivery != "pr" {
			return invalid("unsupported Member delivery")
		}
		seenNames[member.Name], seenPaths[member.Path] = true, true
	}
	if p.Launch.ProfileName == "" || p.Launch.Profile.Kind == "" {
		return invalid("missing launch Profile or kind")
	}
	if p.Launch.ModelKnown && strings.TrimSpace(p.Launch.ConfiguredModel) == "" {
		return invalid("known model is empty")
	}
	if !filepath.IsAbs(p.Paths.Scratch) || !filepath.IsAbs(p.Paths.Output) {
		return invalid("scratch and output paths must be absolute")
	}
	if len(p.Inputs) == 0 || p.Inputs[0].Role != "brief" {
		return invalid("missing selected Brief")
	}
	parseBrief := dispatch.ParseBriefText
	if p.Version < planVersionCurrentBriefContract {
		parseBrief = dispatch.ParseHistoricalBriefText
	}
	brief, err := parseBrief(string(p.Inputs[0].Bytes))
	if err != nil {
		return err
	}
	if brief.Type != p.Type {
		return invalid("selected Brief has another Task type")
	}
	if p.Workflow == nil {
		if brief.Workflow != "" || brief.WorkflowRun != "" || brief.Stage != "" {
			return invalid("selected Brief workflow is not frozen in the plan")
		}
	} else {
		if p.Workflow.Name == "" || p.Workflow.Source == "" || brief.Workflow != "" && p.Workflow.Name != brief.Workflow {
			return invalid("frozen workflow does not match the selected Brief or has no source")
		}
		if len(p.Workflow.SHA256) != 64 {
			return invalid("frozen workflow has an invalid digest")
		}
		if _, err := hex.DecodeString(p.Workflow.SHA256); err != nil {
			return invalid("frozen workflow has an invalid digest")
		}
		digest, err := config.WorkflowDigest(p.Workflow.Profile)
		if err != nil || digest != p.Workflow.SHA256 {
			return invalid("frozen workflow content does not match its digest")
		}
		for _, issue := range config.ValidateWorkflowProfile(p.Workflow.Profile) {
			return invalid("workflows." + p.Workflow.Name + "." + issue.Key + ": " + issue.Reason)
		}
		runtimeBound := p.Workflow.RunID != "" || p.Workflow.StageName != "" || p.Workflow.StageIndex != 0
		if !runtimeBound {
			if p.Version >= Version || brief.Workflow == "" || brief.WorkflowRun != "" || brief.Stage != "" {
				return invalid("frozen workflow does not match the selected Brief or lacks run and stage identity")
			}
		} else {
			if p.Workflow.RunID != "new" && !workflowRunIDPattern.MatchString(p.Workflow.RunID) {
				return invalid("frozen workflow has an invalid run ID")
			}
			if p.Workflow.StageName == "" || p.Workflow.StageIndex < 0 || p.Workflow.StageIndex >= len(p.Workflow.Profile.Stages) {
				return invalid("frozen workflow has an invalid stage identity")
			}
			stage := p.Workflow.Profile.Stages[p.Workflow.StageIndex]
			if stage.Name != p.Workflow.StageName || stage.TaskType != p.Type {
				return invalid("frozen workflow stage does not match the Task type")
			}
			if stage.TaskType == "none" {
				return invalid("a none workflow stage cannot admit a Worker Task")
			}
			initialStage := p.Workflow.StageIndex == 0 && brief.Workflow != "" && brief.WorkflowRun == "" && brief.Stage == ""
			if p.Workflow.RunID == "new" {
				if !initialStage {
					return invalid("new workflow run must start at its first stage")
				}
			} else if !initialStage && (brief.WorkflowRun != p.Workflow.RunID || brief.Stage != p.Workflow.StageName) {
				return invalid("frozen workflow run or stage does not match the selected Brief")
			}
		}
	}
	if p.ValidationLoops == nil {
		if brief.ReviewRounds > 0 {
			return invalid("Brief review_rounds is not frozen in the plan")
		}
	} else {
		if p.Type != "change" {
			return invalid("validation loops are only valid for Change Tasks")
		}
		settings := []struct {
			key   string
			value any
		}{
			{"review_loop.enabled", p.ValidationLoops.Review.Enabled},
			{"review_loop.max_rounds", p.ValidationLoops.Review.MaxRounds},
			{"review_loop.block_at", p.ValidationLoops.Review.BlockAt},
			{"review_loop.auto_start", p.ValidationLoops.Review.AutoStart},
			{"review_loop.docs", p.ValidationLoops.Review.Docs},
			{"review_loop.guidance", p.ValidationLoops.Review.Guidance},
			{"ci_loop.max_fix_rounds", p.ValidationLoops.CI.MaxFixRounds},
			{"ci_loop.rerun_cancelled", p.ValidationLoops.CI.RerunCancelled},
		}
		for _, setting := range settings {
			if err := config.ValidateSetting(setting.key, setting.value, false); err != nil {
				return invalid(err.Error())
			}
		}
		if brief.ReviewRounds != p.ValidationLoops.BriefReviewRounds || p.ValidationLoops.BriefReviewRounds > p.ValidationLoops.Review.MaxRounds {
			return invalid("frozen review_rounds does not match or exceeds the selected config")
		}
	}
	if brief.Execution != "" && brief.Execution != p.Execution.Mode || strings.Join(brief.Paths, "\x00") != strings.Join(p.Execution.Paths, "\x00") {
		return invalid("selected Brief execution scope does not match the admitted plan")
	}
	if p.Execution.Mode == "in-place" && brief.Execution != "in-place" {
		return invalid("in-place execution must be explicitly selected in the Brief")
	}
	if p.Execution.Mode == "in-place" && brief.Review != "" {
		legacyNoReview := p.Version == 1 && brief.Review == "never"
		if !legacyNoReview {
			return invalid("in-place execution cannot request Review")
		}
	}
	if p.Execution.Mode == "in-place" && (p.UserChoice == nil || p.UserChoice.Source != "start --user-approved") {
		return invalid("in-place execution requires recorded User consent")
	}
	if p.Execution.Mode == "in-place" && (brief.Ticket != nil || len(brief.Issues) > 0 || len(brief.Refs) > 0 || p.Delivery != "local") {
		return invalid("in-place execution cannot use forge issue links or non-local delivery")
	}
	if p.Execution.Mode == "in-place" && (brief.Base != "" || brief.Target != "" || len(brief.BaseByRepo) > 0 || len(brief.TargetByRepo) > 0 || len(brief.Repos) > 0 || brief.RootContext != "") {
		return invalid("in-place execution cannot select Git branches, workspace Members or root context")
	}
	if p.UserChoice != nil {
		choice := p.UserChoice
		if strings.TrimSpace(choice.Quote) == "" || choice.Actor != "lead" && choice.Actor != "user" || choice.Source != "start --user-approved" || choice.BriefSHA256 != p.Inputs[0].SHA256 || len(choice.Fields) == 0 || strings.Join(choice.Fields, ",") != strings.Join(preferenceFields(brief), ",") {
			return invalid("invalid scoped User choice provenance")
		}
	}
	if len(p.Preferences) > 0 {
		if len(p.Preferences) != 2 || p.Preferences[0].Name != "delivery" || p.Preferences[0].Value != p.Delivery || p.Preferences[1].Name != "review" || p.Preferences[1].Value != p.Review || p.Preferences[0].Source == "" || p.Preferences[1].Source == "" {
			return invalid("invalid preference values or provenance")
		}
	}
	if brief.RootContext != "" && (p.Execution.Scope != "workspace" || p.Execution.RootContext != brief.RootContext) {
		return invalid("selected Brief root context does not match workspace execution")
	}
	if p.Execution.Scope == "workspace" && p.Execution.RootContext != "" && p.Execution.RootContext != RootContextNone && brief.RootContext != p.Execution.RootContext {
		return invalid("workspace root context is not selected in the Brief")
	}
	grants := map[string]string{}
	for _, grant := range p.Grants {
		if grant.Source == "" || grants[grant.Name] != "" {
			return invalid("missing provenance or duplicate grant")
		}
		grants[grant.Name] = grant.Value
	}
	if len(grants) != 2 || grants["autonomy.review"] != "ask" && grants["autonomy.review"] != "lead" || grants["autonomy.merge"] != "ask" && grants["autonomy.merge"] != "auto" {
		return invalid("unsupported autonomy grant")
	}
	for _, input := range p.Inputs {
		selected := Select(input.Role, input.Source, input.Bytes)
		if input.SHA256 != selected.SHA256 {
			return invalid("selected input digest mismatch: " + input.Source)
		}
	}
	return nil
}

func Decode(data string) (Plan, error) {
	var p Plan
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		return Plan{}, fmt.Errorf("decode execution plan: %w", err)
	}
	if p.Version < Version {
		p.legacyJSON = data
	}
	if p.Version < planVersionCurrentBriefContract {
		switch p.Type {
		case "ship":
			p.Type = "change"
		case "scout":
			p.Type = "research"
		}
		if p.Executor == "rider" {
			p.Executor = "worker"
		}
		if p.Execution.Mode == "mount" {
			p.Execution.Mode = "sandbox"
		}
		if p.Launch.Kind.NoticeDelivery == "lookout" {
			p.Launch.Kind.NoticeDelivery = "watch"
		}
		if p.UserChoice != nil && p.UserChoice.Source == "ride --user-approved" {
			p.UserChoice.Source = "start --user-approved"
		}
		for i := range p.Grants {
			if p.Grants[i].Name == "autonomy.land" {
				p.Grants[i].Name = "autonomy.merge"
			}
		}
		var legacy struct {
			Pool config.Pool `json:"remuda"`
		}
		if err := json.Unmarshal([]byte(data), &legacy); err != nil {
			return Plan{}, err
		}
		if p.Pool.Clean == "" {
			p.Pool = legacy.Pool
		}
	}
	return p, p.Validate()
}

func (p Plan) Encode() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	if p.legacyJSON != "" {
		return p.legacyJSON, nil
	}
	data, err := json.Marshal(p)
	return string(data), err
}

func (p Plan) SHA256() string {
	data, _ := json.Marshal(p)
	if p.legacyJSON != "" {
		data = []byte(p.legacyJSON)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Bind resolves only declared managed paths, never bytes selected from a source.
// It runs inside the Task creation transaction after choosing a sequence.
func (p Plan) Bind(taskID string) Plan {
	if filepath.Base(p.Paths.Scratch) == "{task}" {
		p.Paths.Scratch = filepath.Join(filepath.Dir(p.Paths.Scratch), taskID)
	}
	if filepath.Base(p.Paths.Output) == "{task}" {
		p.Paths.Output = filepath.Join(filepath.Dir(p.Paths.Output), taskID)
	}
	return p
}

// WithoutDependency removes one declared prerequisite as an explicit plan revision.
func (p Plan) WithoutDependency(taskID, milestone string) (Plan, error) {
	if err := p.Validate(); err != nil {
		return Plan{}, err
	}
	dependencies := make([]dispatch.Dependency, 0, len(p.Dependencies))
	removed := false
	for _, dependency := range p.Dependencies {
		if dependency.TaskID == taskID && dependency.Milestone == milestone {
			removed = true
			continue
		}
		dependencies = append(dependencies, dependency)
	}
	if !removed {
		return Plan{}, fmt.Errorf("prerequisite %s:%s is not in the admitted plan", taskID, milestone)
	}
	p.PreviousSHA256 = p.SHA256()
	p.legacyJSON = ""
	p.Revision++
	p.Dependencies = dependencies
	data, err := p.Encode()
	if err != nil {
		return Plan{}, err
	}
	return Decode(data)
}

// Reconfigure changes launch settings explicitly while retaining the admitted
// contract. Evidence from earlier revisions cannot attest the new revision.
func (p Plan) Reconfigure(launch Launch) (Plan, error) {
	if err := p.Validate(); err != nil {
		return Plan{}, err
	}
	p.PreviousSHA256 = p.SHA256()
	p.legacyJSON = ""
	p.Revision++
	p.Launch = launch
	data, err := p.Encode()
	if err != nil {
		return Plan{}, err
	}
	return Decode(data)
}

// Arguments reuses the admitted vector, adding only this launch's recorded
// session through the frozen resume template.
func (launch Launch) Arguments(session string) []string {
	args := append([]string{}, launch.Args...)
	if session != "" {
		for _, arg := range launch.Kind.ResumeArgs {
			args = append(args, strings.ReplaceAll(arg, "{session}", session))
		}
	}
	return args
}

func Arguments(profile config.Profile, kind config.Kind, session string) []string {
	args := append([]string{}, kind.AutoApproveArgs...)
	render := func(template []string, token, value string) {
		for _, arg := range template {
			args = append(args, strings.ReplaceAll(arg, token, value))
		}
	}
	if profile.Model != "" {
		render(kind.ModelArgs, "{model}", profile.Model)
	}
	if profile.Effort != "" {
		render(kind.EffortArgs, "{effort}", profile.Effort)
	}
	args = append(args, profile.Args...)
	if session != "" {
		render(kind.ResumeArgs, "{session}", session)
	}
	return args
}
