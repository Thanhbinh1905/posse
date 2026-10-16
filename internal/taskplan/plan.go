// Package taskplan resolves the supported Brief contract into a versioned,
// self-contained execution plan. It performs no I/O and infers no provider identity.
package taskplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
)

const Version = 1

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
	if brief.LandingMode != "" {
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
	return fields
}

type Member struct {
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	Base       string   `json:"base"`
	BaseCommit string   `json:"base_commit,omitempty"`
	Target     string   `json:"target"`
	Delivery   string   `json:"delivery"`
	Gate       []string `json:"gate"`
}

type Execution struct {
	Mode       string   `json:"mode"`
	Scope      string   `json:"scope"`
	Base       string   `json:"base"`
	BaseCommit string   `json:"base_commit,omitempty"`
	Target     string   `json:"target"`
	Members    []Member `json:"members,omitempty"`
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

type Plan struct {
	Version        int           `json:"version"`
	Revision       int           `json:"revision"`
	PreviousSHA256 string        `json:"previous_sha256,omitempty"`
	Type           string        `json:"type"`
	Outcome        string        `json:"outcome"`
	Executor       string        `json:"executor"`
	Execution      Execution     `json:"execution"`
	Review         string        `json:"review"`
	ReviewOf       int64         `json:"review_of,omitempty"`
	Delivery       string        `json:"delivery"`
	Grants         []Grant       `json:"grants"`
	Preferences    []Preference  `json:"preferences,omitempty"`
	UserChoice     *UserChoice   `json:"user_choice,omitempty"`
	Inputs         []Input       `json:"inputs"`
	Paths          Paths         `json:"paths"`
	Launch         Launch        `json:"launch"`
	Gate           []string      `json:"gate"`
	MergeMethod    string        `json:"merge_method"`
	Remuda         config.Remuda `json:"remuda"`
}

type Request struct {
	Brief         Input
	Inputs        []Input
	Execution     Execution
	Launch        Launch
	Review        string
	ReviewOf      int64
	Delivery      string
	Autonomy      config.Autonomy
	ConfigSources []string
	UserChoice    *UserChoice
	Paths         Paths
	Gate          []string
	MergeMethod   string
	Remuda        config.Remuda
}

// Resolve retains legacy Brief semantics; it does not introduce new workflow
// types, executors or execution modes. Callers resolve repository facts first.
func Resolve(r Request) (Plan, error) {
	brief, err := dispatch.ParseBriefText(string(r.Brief.Bytes))
	if err != nil {
		return Plan{}, err
	}
	userChoice := r.UserChoice != nil
	delivery, autonomy, err := dispatch.ResolveDelivery(brief, r.Delivery, r.Autonomy, userChoice)
	if err != nil {
		return Plan{}, err
	}
	review, err := dispatch.ResolveReview(brief.Review, r.Review, userChoice)
	if err != nil {
		return Plan{}, err
	}
	outcome := "report"
	if brief.Type == "ship" {
		outcome = "change"
	}
	source := "effective config: " + strings.Join(r.ConfigSources, ", ")
	reviewSource, landSource := source, source
	if brief.AutonomyReview != "" {
		reviewSource = "brief: " + r.Brief.Source
	}
	if brief.AutonomyLand != "" {
		landSource = "brief: " + r.Brief.Source
	}
	p := Plan{Version: Version, Revision: 1, Type: brief.Type, Outcome: outcome, Executor: "rider",
		Execution: r.Execution, Review: review, ReviewOf: r.ReviewOf, Delivery: delivery,
		Grants: []Grant{{Name: "autonomy.review", Value: autonomy.Review, Source: reviewSource}, {Name: "autonomy.land", Value: autonomy.Land, Source: landSource}},
		Inputs: append([]Input{r.Brief}, r.Inputs...), Paths: r.Paths, Launch: r.Launch,
		Gate: r.Gate, MergeMethod: r.MergeMethod, Remuda: r.Remuda}
	p.Preferences = []Preference{{Name: "delivery", Value: p.Delivery, Source: source}, {Name: "review", Value: p.Review, Source: source}}
	for i, explicit := range []bool{brief.LandingMode != "", brief.Review != ""} {
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
	if p.Execution.Mode == "" {
		p.Execution.Mode = "mount"
	}
	// Snapshot interpreted configuration fields consumed by the contract, not
	// entire TOML files, other Profiles or unrelated forge content.
	settings, err := json.Marshal(struct {
		Execution   Execution     `json:"execution"`
		Review      string        `json:"review"`
		Delivery    string        `json:"delivery"`
		Grants      []Grant       `json:"grants"`
		Gate        []string      `json:"gate"`
		MergeMethod string        `json:"merge_method"`
		Remuda      config.Remuda `json:"remuda"`
	}{p.Execution, p.Review, p.Delivery, p.Grants, p.Gate, p.MergeMethod, p.Remuda})
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
	if p.Version != Version || p.Revision < 1 {
		return invalid("unsupported version or revision")
	}
	if p.Revision == 1 && p.PreviousSHA256 != "" || p.Revision > 1 && len(p.PreviousSHA256) != 64 {
		return invalid("invalid previous revision digest")
	}
	if p.Executor != "rider" || p.Execution.Mode != "mount" {
		return invalid("unsupported executor or execution mode")
	}
	if p.Execution.Scope != "repo" && p.Execution.Scope != "workspace" {
		return invalid("unsupported scope")
	}
	if p.Type != "ship" && p.Type != "scout" && p.Type != "review" {
		return invalid("unsupported Task type")
	}
	outcome := "report"
	if p.Type == "ship" {
		outcome = "change"
	}
	if p.Outcome != outcome {
		return invalid("outcome does not match Task type")
	}
	if p.Type == "review" && p.ReviewOf < 1 || p.Type != "review" && p.ReviewOf != 0 {
		return invalid("review target does not match Task type")
	}
	if p.Review != "always" && p.Review != "on_risk" && p.Review != "never" {
		return invalid("unsupported review policy")
	}
	if p.Delivery != "local" && p.Delivery != "pr" && p.Delivery != "no-mistakes" {
		return invalid("unsupported delivery")
	}
	if p.Execution.Scope == "workspace" && p.Delivery == "no-mistakes" {
		return invalid("no-mistakes delivery is unsupported for Workspaces")
	}
	if p.Execution.Scope == "repo" && len(p.Execution.Members) != 0 {
		return invalid("repository execution cannot select Members")
	}
	if p.Execution.Scope == "workspace" && p.Type == "ship" && len(p.Execution.Members) == 0 {
		return invalid("workspace Ship requires Members")
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
	brief, err := dispatch.ParseBriefText(string(p.Inputs[0].Bytes))
	if err != nil {
		return err
	}
	if brief.Type != p.Type {
		return invalid("selected Brief has another Task type")
	}
	if p.UserChoice != nil {
		choice := p.UserChoice
		if strings.TrimSpace(choice.Quote) == "" || choice.Actor != "lead" && choice.Actor != "user" || choice.Source != "ride --user-approved" || choice.BriefSHA256 != p.Inputs[0].SHA256 || len(choice.Fields) == 0 || strings.Join(choice.Fields, ",") != strings.Join(preferenceFields(brief), ",") {
			return invalid("invalid scoped User choice provenance")
		}
	}
	if len(p.Preferences) > 0 {
		if len(p.Preferences) != 2 || p.Preferences[0].Name != "delivery" || p.Preferences[0].Value != p.Delivery || p.Preferences[1].Name != "review" || p.Preferences[1].Value != p.Review || p.Preferences[0].Source == "" || p.Preferences[1].Source == "" {
			return invalid("invalid preference values or provenance")
		}
	}
	grants := map[string]string{}
	for _, grant := range p.Grants {
		if grant.Source == "" || grants[grant.Name] != "" {
			return invalid("missing provenance or duplicate grant")
		}
		grants[grant.Name] = grant.Value
	}
	if len(grants) != 2 || grants["autonomy.review"] != "ask" && grants["autonomy.review"] != "lead" || grants["autonomy.land"] != "ask" && grants["autonomy.land"] != "auto" {
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
	return p, p.Validate()
}

func (p Plan) Encode() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(p)
	return string(data), err
}

func (p Plan) SHA256() string {
	data, _ := json.Marshal(p)
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

// Reconfigure changes launch settings explicitly while retaining the admitted
// contract. Evidence from earlier revisions cannot attest the new revision.
func (p Plan) Reconfigure(launch Launch) (Plan, error) {
	if err := p.Validate(); err != nil {
		return Plan{}, err
	}
	p.PreviousSHA256 = p.SHA256()
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
