package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

type readinessGap struct {
	Code          string `json:"code"`
	Consequence   string `json:"consequence"`
	Key           string `json:"key"`
	UserOnly      bool   `json:"user_only"`
	Fix           string `json:"fix"`
	Informational bool   `json:"informational"`
	Member        string `json:"member"` // Always present to keep TOON rows tabular.
}

// Readiness is a projection, not a first-run flag. Keep its ordering stable so
// the same effective config and diagnostics produce the same conversation.
func projectReadiness(cfg config.Config, checks []doctorCheck) []readinessGap {
	gaps := []readinessGap{}
	for _, code := range []string{"forge_auth", "gate_empty", "autonomy_ask", "no_mistakes_uninitialized", "machine_setup"} {
		if code == "autonomy_ask" {
			if cfg.Defaults.LandingMode == "" {
				continue // Machine-only diagnostics have no Project authority.
			}
			var actions []string
			var keys []string
			if cfg.Autonomy.Review == "" || cfg.Autonomy.Review == "ask" {
				actions = append(actions, "decide review findings")
				keys = append(keys, "autonomy.review")
			}
			if cfg.Autonomy.Land == "" || cfg.Autonomy.Land == "ask" {
				actions = append(actions, "Land (merge)")
				keys = append(keys, "autonomy.land")
			}
			if len(actions) > 0 {
				gaps = append(gaps, readinessGap{Code: code, Consequence: "The Lead must ask before it may " + strings.Join(actions, " or ") + ".", Key: strings.Join(keys, ", "), UserOnly: true, Fix: "Keep ask unless this request needs more authority. Record one-off approvals separately; change standing Autonomy only with the User's explicit grant.", Informational: true})
			}
			continue
		}
		for _, check := range checks {
			if check.GapCode == code && check.Status != "ok" {
				gaps = append(gaps, readinessGap{Code: code, Consequence: check.Detail, Key: check.Key, UserOnly: check.UserOnly, Fix: check.Action, Informational: code == "forge_auth" && check.Status == "info", Member: check.Member})
			}
		}
	}
	return gaps
}

func withReadiness(result axi.Object, gaps []readinessGap) axi.Object {
	if len(gaps) != 0 {
		result = append(result, axi.Field{Key: "readiness", Value: gaps})
	}
	return result
}

func withReadinessHelp(err error, gaps []readinessGap) error {
	failure, ok := err.(*axi.Error)
	if !ok {
		return err
	}
	for _, gap := range gaps {
		if gap.Code == "no_mistakes_uninitialized" {
			failure.Help = append(failure.Help, "Readiness gap: "+gap.Code+" - "+gap.Consequence, "Readiness fix: "+gap.Fix)
			break
		}
	}
	return failure
}

func (s *Service) readiness(ctx context.Context, db *store.DB, project store.Project, cfg config.Config) ([]readinessGap, error) {
	checks, err := s.projectReadinessChecks(ctx, db, project, cfg)
	if err != nil {
		return nil, err
	}
	checks = append(checks, s.machineSetupCheck(ctx)...)
	return projectReadiness(cfg, checks), nil
}

func (s *Service) projectReadinessChecks(ctx context.Context, db *store.DB, project store.Project, cfg config.Config) ([]doctorCheck, error) {
	targets, err := s.projectTargets(ctx, db, project)
	if err != nil {
		return nil, err
	}
	return repositoryReadinessChecks(ctx, cfg, targets), nil
}

// These checks use the same effective Member policies as Landing, never a
// guessed test command or a repository script. Auth/status probes are read-only.
type forgeReadinessResult struct {
	Forge repositoryForge
	Err   error
}

type forgeAuthenticationError struct {
	CLI  string
	Host string
}

func (e *forgeAuthenticationError) Error() string {
	return e.CLI + " authentication for " + e.Host + " is missing or could not be verified"
}

func repositoryForgeReadiness(ctx context.Context, root string, cfg config.Config, member string) (repositoryForge, error) {
	forge, probe, err := resolveForgeForRepository(ctx, root, cfg, member)
	if err != nil {
		return forge, err
	}
	if probe == nil {
		result, err := cachedForgeProbe(ctx, root, forge.Host, cfg, forge.Kind)
		if err != nil {
			return forge, err
		}
		probe = &result
	}
	if probe.timedOut(forge.Kind) {
		return forge, forgeProbeTimeoutError(forge.Host)
	}
	if !probe.authenticated(forge.Kind) {
		cli := "gh"
		if forge.Kind == "gitlab" {
			cli = "glab"
		}
		return forge, &forgeAuthenticationError{CLI: cli, Host: forge.Host}
	}
	return forge, nil
}

type forgeAuthenticationUnknownError struct {
	Host string
}

func (e *forgeAuthenticationUnknownError) Error() string {
	if e.Host == "" {
		return "no cached forge authentication check is available"
	}
	return "no cached forge authentication check is available for " + e.Host
}

func cachedRepositoryForgeReadiness(ctx context.Context, target repoTarget, cfg config.Config) (repositoryForge, error) {
	host := target.OriginHost
	if host == "" && target.Name == "" {
		host = originHost(ctx, target.Root)
	}
	if host == "" || host == noOriginHost {
		return repositoryForge{Root: target.Root}, &forgeAuthenticationUnknownError{}
	}
	kind := cfg.Defaults.Forge
	if target.Name != "" {
		if override, ok := cfg.Repositories[target.Name]; ok && override.Forge != "" {
			kind = override.Forge
		}
	}
	probeMode := kind
	if kind == "" || kind == "auto" {
		switch host {
		case "github.com":
			kind, probeMode = "github", "github"
		case "gitlab.com":
			kind, probeMode = "gitlab", "gitlab"
		default:
			probeMode = "auto"
		}
	}
	if probeMode != "github" && probeMode != "gitlab" && probeMode != "auto" {
		return repositoryForge{Host: host, Root: target.Root}, axi.Failure("pr_forge_unknown", "origin host has no configured forge: "+host, false, "Set defaults.forge to github or gitlab for this Project")
	}
	outcome, found, err := cachedForgeProbeOnly(host, cfg, probeMode)
	forge := repositoryForge{Kind: kind, Host: host, Root: target.Root}
	if err != nil {
		return forge, err
	}
	if !found {
		return forge, &forgeAuthenticationUnknownError{Host: host}
	}
	if probeMode == "auto" {
		switch {
		case outcome.GitLabAuthenticated && outcome.GitHubAuthenticated:
			return forge, axi.Failure("pr_forge_ambiguous", "origin host is authenticated with both gh and glab: "+host, false, "Set defaults.forge or repositories.<repository>.forge")
		case outcome.GitLabAuthenticated:
			forge.Kind = "gitlab"
		case outcome.GitHubAuthenticated:
			forge.Kind = "github"
		case outcome.anyTimedOut():
			return forge, forgeProbeTimeoutError(host)
		default:
			return forge, axi.Failure("pr_forge_unknown", "origin host has no configured forge: "+host, false, "Set defaults.forge to github or gitlab for this Project")
		}
	}
	if outcome.timedOut(forge.Kind) {
		return forge, forgeProbeTimeoutError(host)
	}
	if !outcome.authenticated(forge.Kind) {
		cli := "gh"
		if forge.Kind == "gitlab" {
			cli = "glab"
		}
		return forge, &forgeAuthenticationError{CLI: cli, Host: host}
	}
	return forge, nil
}

func repositoryDoctorChecks(ctx context.Context, cfg config.Config, targets []repoTarget) []doctorCheck {
	return repositoryChecks(ctx, cfg, targets, true)
}

func repositoryReadinessChecks(ctx context.Context, cfg config.Config, targets []repoTarget) []doctorCheck {
	return repositoryChecks(ctx, cfg, targets, false)
}

func repositoryChecks(ctx context.Context, cfg config.Config, targets []repoTarget, probeForge bool) []doctorCheck {
	checks := []doctorCheck{}
	modes := make([]string, len(targets))
	forgeChecks := make([]forgeReadinessResult, len(targets))
	var probes sync.WaitGroup
	for index, target := range targets {
		remote := target.OriginHost
		if probeForge || target.Name == "" {
			remote = originHost(ctx, target.Root)
		}
		target.OriginHost = remote
		modes[index] = cfg.Defaults.LandingMode
		if target.Name != "" {
			modes[index] = savedMemberLandingMode(cfg, target.Name, remote)
		}
		if modes[index] != "pr" {
			continue
		}
		probes.Add(1)
		go func(index int, target repoTarget) {
			defer probes.Done()
			if probeForge {
				forgeChecks[index].Forge, forgeChecks[index].Err = repositoryForgeReadiness(ctx, target.Root, cfg, target.Name)
			} else {
				forgeChecks[index].Forge, forgeChecks[index].Err = cachedRepositoryForgeReadiness(ctx, target, cfg)
			}
		}(index, target)
	}
	probes.Wait()

	for index, target := range targets {
		mode, gate, gateKey := modes[index], cfg.Defaults.Gate, "defaults.gate"
		if target.Name != "" {
			if override, ok := cfg.Repositories[target.Name]; ok && override.Gate != nil {
				gate = override.Gate
			}
			gateKey = "repositories." + target.Name + ".gate"
		}
		suffix := ""
		if target.Name != "" {
			suffix = " for Member " + target.Name
		}
		gateCheck := doctorCheck{Name: "Gate" + suffix, Status: "ok", Detail: "Gate configured", GapCode: "gate_empty", Key: gateKey, Member: target.Name, UserOnly: true}
		if len(gate) == 0 {
			gateCheck.Status = "info"
			gateCheck.Detail = "No Project Gate commands run before Landing" + suffix + "."
			gateCheck.Action = "Propose Gate commands found in the repository; save them only after an explicit yes with --user-approved, or continue without a Gate if the User declines."
		}
		checks = append(checks, gateCheck)
		if mode == "pr" {
			forgeCheck := forgeChecks[index]
			auth := doctorCheck{Name: "forge auth" + suffix, Status: "ok", Detail: "authenticated", GapCode: "forge_auth", Key: "defaults.forge", Member: target.Name}
			if target.Name != "" {
				auth.Key = "repositories." + target.Name + ".forge"
			}
			if forgeCheck.Forge.Host != "" {
				auth.Name += " " + forgeCheck.Forge.Host
			}
			if forgeCheck.Err != nil {
				var unknown *forgeAuthenticationUnknownError
				if errors.As(forgeCheck.Err, &unknown) {
					auth.Status = "info"
					auth.Detail = "Forge kind or authentication is unknown" + suffix + ": " + forgeCheck.Err.Error()
					auth.Action = "Posse will resolve forge access when a Task needs this repository."
				} else {
					auth.Status = "warn"
					auth.Detail = "Pull requests cannot be verified or opened" + suffix + ": " + forgeCheck.Err.Error()
					auth.Action = "Configure an origin remote and its forge, then authenticate gh or glab for that host."
				}
				var authErr *forgeAuthenticationError
				if errors.As(forgeCheck.Err, &authErr) {
					auth.Action = "Run `" + authErr.CLI + " auth login --hostname " + authErr.Host + "`."
				}
				var probeErr *axi.Error
				if errors.As(forgeCheck.Err, &probeErr) && probeErr.Code == "pr_forge_probe_timeout" {
					auth.Action = "Check forge CLI connectivity and authentication for this host."
				}
			}
			checks = append(checks, auth)
		}
		if mode == "no-mistakes" {
			check := doctorCheck{Name: "no-mistakes" + suffix, Status: "ok", Detail: "initialized", GapCode: "no_mistakes_uninitialized", Key: "defaults.landing_mode", Member: target.Name}
			if err := ensureNoMistakesInitialized(ctx, target.Root); err != nil {
				check.Status = "warn"
				check.Detail = "The no-mistakes pipeline cannot run" + suffix + ": " + err.Error()
				check.Action = "Install no-mistakes if needed, then run `no-mistakes init` in the repository."
			}
			checks = append(checks, check)
		}
	}
	return checks
}

// Reuse setup's read-only plan so readiness notices missing or changed assets,
// not just whether setup happened at some point in the past.
func (s *Service) machineSetupCheck(ctx context.Context) []doctorCheck {
	check := doctorCheck{Name: "machine setup", Status: "ok", Detail: "current", GapCode: "machine_setup", Action: "Run `posse setup --check` to inspect changes, then apply `posse setup` with the User's go-ahead."}
	inspect := func() error {
		home, err := s.homePath()
		if err != nil {
			return err
		}
		manifest, found, err := readSetupManifest(filepath.Join(home, setupManifestName))
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("machine setup has not been applied")
		}
		if s.Herdr == nil {
			return fmt.Errorf("herdr setup could not be inspected")
		}
		state, err := s.inspectSetup(ctx, home)
		if err != nil {
			return err
		}
		dirs, err := setupDirectories()
		if err != nil {
			return err
		}
		binary := manifest.Binary
		if binary == "" {
			binary, err = os.Executable()
			if err != nil {
				return err
			}
		}
		plan := s.setupPlan(home, binary, s.binaryVersion(), dirs, manifest, found, state)
		for _, row := range plan {
			action, _ := row["action"].(string)
			if action != "keep" && action != "offer" && action != "offer_global" && action != "offer_removal" && action != "preserve" && action != "manual" {
				return fmt.Errorf("machine setup has remaining changes (%s: %s)", row["step"], action)
			}
		}
		return nil
	}
	if err := inspect(); err != nil {
		check.Status = "warn"
		check.Detail = "Rider guards or Notice delivery may be missing: " + err.Error() + "."
	}
	return []doctorCheck{check}
}
