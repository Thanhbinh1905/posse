package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// noMistakesMigration suggests replacement settings for one Project or
// workspace Member still using no-mistakes. It is advice only: Posse never
// writes config for this migration, the User chooses and runs the commands.
type noMistakesMigration struct {
	Project, Member string
	Uses            []string
	Set             []string
	LighterReview   []string
	TrustedConfig   string
	TrustedCommands []string
}

// noMistakesMigrationNotes apply to every migration and are printed once.
var noMistakesMigrationNotes = []string{
	"Posse never changes config for this migration; run the chosen `posse config set` commands yourself.",
	"Tasks already admitted keep their frozen no-mistakes plan, target branch (for example staging) and receipt history; new settings apply to new Tasks only.",
	"full-pipeline is selectable but not yet production-ready (docs/spec.md section 25); it does not promise no-mistakes parity. quick is an independent exact-head review; none records review as not required and relies on forge CI.",
	"Local workflow runs are CI-equivalent evidence, not forge CI; PR delivery still requires green forge CI at the gated head.",
	"Merging a frozen no-mistakes Task still needs a receipt from `no-mistakes axi receipt`; no-mistakes v1.50.0 does not produce it, and a PR attestation comment is not a receipt.",
}

// trustedNoMistakesConfig holds the no-mistakes settings Posse can translate.
type trustedNoMistakesConfig struct {
	Commands struct {
		Test string `yaml:"test"`
		Lint string `yaml:"lint"`
	} `yaml:"commands"`
	AutoFix struct {
		CI *int `yaml:"ci"`
	} `yaml:"auto_fix"`
}

func noMistakesMigrations(ctx context.Context, db *store.DB, project store.Project, cfg config.Config) ([]noMistakesMigration, error) {
	if project.IsDirectory() {
		return nil, nil
	}
	var migrations []noMistakesMigration
	if !project.IsWorkspace() {
		if migration, ok := newNoMistakesMigration(ctx, cfg, project.Name, "", project.Root, project.DefaultBranch, cfg.Defaults.MergeMode, cfg.Defaults.Review); ok {
			migrations = append(migrations, migration)
		}
		return migrations, nil
	}
	repos, err := db.ProjectRepos(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	for _, repo := range repos {
		if repo.Status != store.RepoActive {
			continue
		}
		root := filepath.Join(project.Root, repo.Path)
		if migration, ok := newNoMistakesMigration(ctx, cfg, project.Name, repo.Name, root, repo.DefaultBranch, savedMemberMergeMode(cfg, repo.Name, repo.OriginHost), cfg.MemberReview(repo.Name)); ok {
			migrations = append(migrations, migration)
		}
	}
	return migrations, nil
}

func newNoMistakesMigration(ctx context.Context, cfg config.Config, project, member, root, branch, mergeMode, review string) (noMistakesMigration, bool) {
	if mergeMode != "no-mistakes" && review != "no-mistakes" {
		return noMistakesMigration{}, false
	}
	prefix := "defaults."
	if member != "" {
		prefix = "repositories." + member + "."
	}
	set := func(key, value string) string {
		return "posse config set " + key + " " + value + " --project " + project
	}
	migration := noMistakesMigration{Project: project, Member: member}
	if mergeMode == "no-mistakes" {
		migration.Uses = append(migration.Uses, prefix+"merge_mode=no-mistakes")
		migration.Set = append(migration.Set, set(prefix+"merge_mode", "pr"))
	}
	if review == "no-mistakes" {
		migration.Uses = append(migration.Uses, prefix+"review=no-mistakes")
	}
	// A no-mistakes Merge Mode validated the whole pipeline, so its
	// replacement is full-pipeline review even when review named another mode.
	if review != "full-pipeline" {
		migration.Set = append(migration.Set, set(prefix+"review", "full-pipeline"))
		migration.LighterReview = []string{set(prefix+"review", "quick"), set(prefix+"review", "none")}
	}
	trusted, source, err := readTrustedNoMistakesConfig(ctx, root, branch)
	migration.TrustedConfig = source
	if err != nil {
		migration.TrustedConfig = "unreadable: " + err.Error()
	}
	if trusted.AutoFix.CI != nil && *trusted.AutoFix.CI >= 0 && *trusted.AutoFix.CI != cfg.CILoop.MaxFixRounds {
		migration.Set = append(migration.Set, set("ci_loop.max_fix_rounds", strconv.Itoa(*trusted.AutoFix.CI)))
	}
	if command := strings.TrimSpace(trusted.Commands.Test); command != "" {
		migration.TrustedCommands = append(migration.TrustedCommands, "commands.test `"+command+"`: no command Gate runs it (ADR 0028); a full-pipeline Worker records it with `posse validate test --verify \""+command+" -> <result>\"`, and forge CI should run it at the gated head")
	}
	if command := strings.TrimSpace(trusted.Commands.Lint); command != "" {
		migration.TrustedCommands = append(migration.TrustedCommands, "commands.lint `"+command+"`: keep it in the repository CI workflow; `"+set("full_pipeline.run_workflows", "true")+"` lets the full-pipeline lint stage run that workflow locally")
	}
	return migration, true
}

// readTrustedNoMistakesConfig reads what no-mistakes itself trusts: the
// default-branch .no-mistakes.yaml over the global config, never the working
// tree, so a candidate branch cannot inject commands into the suggestions.
func readTrustedNoMistakesConfig(ctx context.Context, root, branch string) (trustedNoMistakesConfig, string, error) {
	var trusted trustedNoMistakesConfig
	var sources []string
	if path := noMistakesGlobalConfigPath(); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if err := yaml.Unmarshal(data, &trusted); err != nil {
				return trustedNoMistakesConfig{}, path, err
			}
			sources = append(sources, path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return trustedNoMistakesConfig{}, path, err
		}
	}
	if branch == "" {
		return trusted, strings.Join(sources, ", "), nil
	}
	ref := "origin/" + branch
	if _, err := gitOutput(ctx, root, "rev-parse", "--verify", "--quiet", "refs/remotes/"+ref+"^{commit}"); err != nil {
		ref = branch
	}
	if _, err := gitOutput(ctx, root, "cat-file", "-e", ref+":.no-mistakes.yaml"); err != nil {
		return trusted, strings.Join(sources, ", "), nil
	}
	data, err := gitOutputRaw(ctx, root, "show", ref+":.no-mistakes.yaml")
	source := ref + ":.no-mistakes.yaml"
	if err != nil {
		return trustedNoMistakesConfig{}, source, err
	}
	var repo trustedNoMistakesConfig
	if err := yaml.Unmarshal([]byte(data), &repo); err != nil {
		return trustedNoMistakesConfig{}, source, err
	}
	if repo.Commands.Test != "" {
		trusted.Commands.Test = repo.Commands.Test
	}
	if repo.Commands.Lint != "" {
		trusted.Commands.Lint = repo.Commands.Lint
	}
	if repo.AutoFix.CI != nil {
		trusted.AutoFix.CI = repo.AutoFix.CI
	}
	return trusted, strings.Join(append(sources, source), ", "), nil
}

func noMistakesGlobalConfigPath() string {
	if home := strings.TrimSpace(os.Getenv("NM_HOME")); home != "" {
		return filepath.Join(home, "config.yaml")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".no-mistakes", "config.yaml")
	}
	return ""
}

func noMistakesMigrationOutput(migrations []noMistakesMigration) axi.Object {
	scopes := make([]axi.Object, 0, len(migrations))
	for _, migration := range migrations {
		scope := axi.Object{
			{Key: "project", Value: migration.Project},
			{Key: "member", Value: migration.Member},
			{Key: "uses", Value: stringsToAny(migration.Uses)},
			{Key: "set", Value: stringsToAny(migration.Set)},
		}
		if len(migration.LighterReview) > 0 {
			scope = append(scope, axi.Field{Key: "lighter_review", Value: stringsToAny(migration.LighterReview)})
		}
		if migration.TrustedConfig != "" {
			scope = append(scope, axi.Field{Key: "trusted_config", Value: migration.TrustedConfig})
		}
		if len(migration.TrustedCommands) > 0 {
			scope = append(scope, axi.Field{Key: "trusted_commands", Value: stringsToAny(migration.TrustedCommands)})
		}
		scopes = append(scopes, scope)
	}
	return axi.Object{{Key: "scopes", Value: scopes}, {Key: "notes", Value: stringsToAny(noMistakesMigrationNotes)}}
}
