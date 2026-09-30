package app

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/execgroup"
	"github.com/thanhbinh1905/posse/internal/store"
)

//go:embed lead_pi_extension.ts
var leadPiExtension string

//go:embed lead_opencode_plugin.js
var leadOpenCodePlugin string

//go:embed claude_lowkey/.claude-plugin/plugin.json claude_lowkey/FIRSTMATE-LICENSE claude_lowkey/hooks claude_lowkey/lib
var leadClaudeLowkey embed.FS

// The prompt limit is a byte-based approximation, not a model tokenizer.
const (
	leadInstructionTokenBudget   = 7500
	leadInstructionBytesPerToken = 4
	leadInstructionMaxBytes      = leadInstructionTokenBudget * leadInstructionBytesPerToken
	codexQueueTimeout            = 5 * time.Second
)

// codexOpeningPrompt starts the first turn of a codex Lead so codex creates the
// session that `codex queue` needs. It is passed as codex's own start prompt, so
// nothing is typed into the pane.
const codexOpeningPrompt = "Run `posse` and give the User a short status of the Project."

// noPollRule forbids spending turns on waiting; each polling turn resends the
// Lead's whole context.
const noPollRule = "Never poll with sleep, `posse peek`, `posse roster` or repeated `posse` calls to wait for Riders, CI or PRs; use `posse peek` only to inspect a specific concern."

const lowkeyReportingRule = "Lowkey mode on: message the User only for needs-decision, land_ready when autonomy.land=ask, failed/lost, pr_closed, persistent pr_watch_failing, and completed outcomes (done after review, pr_merged). Acknowledge routine Notices (pr_opened, working notes, first pr_watch_failing, restarts) silently; group them into one short line only if useful. This changes reporting, not autonomy: never answer a needs-decision Notice for the User. The wake message already contains the Notice text; do not run `posse` just to learn what arrived. Inspect with `posse show <task>` when needed."

const normalReportingRule = "Lowkey mode off: handle every Notice, tell the User the outcome in your own words without waiting to be asked, then run `posse ack <id|all>`."

func reportingRule(lowkey bool) string {
	if lowkey {
		return lowkeyReportingRule
	}
	return normalReportingRule
}

// noticeRule tells the Lead how its Notices arrive for the given delivery mode
// and how to wait for them.
func noticeRule(delivery string) string {
	switch delivery {
	case config.NoticeDeliveryLookout:
		return "Keep exactly one `posse lookout` running as a background command that re-invokes you when it returns. If it returns with `state=stopped` and `reason=update`, restart `posse lookout` on your next wake. Otherwise, read the Notices in its result, then use one background `posse lookout --ack <ids>` call to acknowledge handled Notices and restart the watch; do not type into a focused pane. Read the current lowkey state and reporting rule in each lookout result. " + noPollRule + " After (re)starting it, end your turn and let it wake you."
	case config.NoticeDeliveryCodexQueue:
		return "posse queues each batch of Notices into this conversation as a Posse Notice message, also while your pane is focused or the User is typing. Read the current lowkey state and reporting rule in each message. Do not run `posse lookout`. " + noPollRule + " End your turn and let that message wake you."
	case config.NoticeDeliveryPiExtension:
		return "The Pi extension queues actionable Notices into model context as hidden Posse Notice messages when lowkey is on, also while the User is typing or you are working. It acknowledges pr_opened without a turn only when lowkey is on; handle and acknowledge all other Notices yourself. With lowkey off, every Notice arrives as a visible Posse Notice message and needs full reporting. Read the current lowkey state and reporting rule in each message. Do not run `posse lookout`. " + noPollRule + " End your turn and let that message wake you."
	case config.NoticeDeliveryOpenCodePlugin:
		return "The Lead-only OpenCode plugin queues actionable Notices as Posse Notice turns through the session API after your current turn ends, without touching the composer. It acknowledges pr_opened without a turn only when lowkey is on; handle and acknowledge all other Notices yourself. Read the current lowkey state and reporting rule in each message. Do not run `posse lookout`. " + noPollRule + " End your turn and let that message wake you."
	default:
		return "Notices arrive as a Posse Notice prompt only while your pane is idle and unfocused. Read the current lowkey state and reporting rule in each message. Do not run `posse lookout`; run `posse` before you answer the User so waiting Notices are not missed. " + noPollRule + " End your turn and let that prompt wake you."
	}
}

// workerWaitRules tells a Worker how to wait on long commands and PRs.
func workerWaitRules(kind config.Kind) string {
	rules := "\n\nWaiting:\n"
	if kind.BackgroundCommands {
		rules += "- Run long commands (full test suites, e2e, builds) as background commands and never poll them with sleep, ps or repeated reads; do other work or end your turn until they finish.\n"
	}
	return rules + "- Do not wait for PR CI after `posse holler done`; posse watches the PR and the Lead sends fix instructions."
}

func noticeDelivery(kind config.Kind) string {
	if kind.NoticeDelivery == "" {
		return config.NoticeDeliveryPrompt
	}
	return kind.NoticeDelivery
}

// leadLaunch is how one Lead agent is started: its arguments and, for kinds
// that cannot receive instructions as a system prompt, the prompt to type once
// it is ready.
type leadLaunch struct {
	Args        []string
	Env         map[string]string
	TypedPrompt string
	// StartsBusy is true when Args carry the agent's first prompt, so the
	// agent is working as soon as it launches.
	StartsBusy bool
}

func (s *Service) waitLeadStarted(ctx context.Context, paneID string, launch leadLaunch) error {
	if launch.StartsBusy {
		return s.waitAgentLaunched(ctx, paneID)
	}
	return s.waitAgentReady(ctx, paneID)
}

func (s *Service) prepareLeadLaunch(home string, project store.Project, cfg config.Config, kind string, readiness ...[]readinessGap) (leadLaunch, error) {
	kindConfig := cfg.Kinds[kind]
	instructions := leadText(project, cfg, kind)
	if len(readiness) > 0 && len(readiness[0]) > 0 {
		context, err := axi.Encode(withReadiness(axi.Object{}, readiness[0]))
		if err != nil {
			return leadLaunch{}, err
		}
		instructions += "\nStartup readiness (recheck with `posse` after changes):\n" + context + "\n"
	}
	if size := len(instructions); size > leadInstructionMaxBytes {
		message := fmt.Sprintf("Lead instructions exceed the %d-token budget: %d bytes (maximum %d bytes at %d bytes per token)", leadInstructionTokenBudget, size, leadInstructionMaxBytes, leadInstructionBytesPerToken)
		return leadLaunch{}, axi.Failure("lead_instructions_too_large", message, false)
	}
	instructionsFile := filepath.Join(home, "projects", project.Name, "lead.md")
	if err := writeFile(instructionsFile, []byte(instructions)); err != nil {
		return leadLaunch{}, err
	}
	launch := leadLaunch{Args: make([]string, 0, len(kindConfig.AutoApproveArgs)+len(kindConfig.LeadArgs)+len(kindConfig.SystemPromptArgs)+2)}
	if kindConfig.LeadAutoApprove {
		launch.Args = append(launch.Args, kindConfig.AutoApproveArgs...)
	}
	launch.Args = append(launch.Args, kindConfig.LeadArgs...)
	oneLine := strings.Join(strings.Fields(instructions), " ")
	for _, argument := range kindConfig.SystemPromptArgs {
		argument = strings.ReplaceAll(argument, "{file}", instructionsFile)
		launch.Args = append(launch.Args, strings.ReplaceAll(argument, "{text}", oneLine))
	}
	if kind == "claude" {
		plugin, err := s.writeLeadClaudeLowkey(home, project)
		if err != nil {
			return leadLaunch{}, err
		}
		launch.Args = append(launch.Args, "--plugin-dir", plugin)
		launch.Env = map[string]string{
			"CLAUDE_CODE_ENABLE_FUNCTION_HOOKS": "1",
			"POSSE_LOWKEY_CONFIG":               config.ConfigPath(home, project.Name),
			"POSSE_LOWKEY_GLOBAL_CONFIG":        config.ConfigPath(home, ""),
			"POSSE_LOWKEY_NOTICES_DIR":          claudeNoticeDirectory(home, project.Name),
		}
	}
	if kind == "opencode" && noticeDelivery(kindConfig) == config.NoticeDeliveryOpenCodePlugin {
		plugin, err := s.writeLeadOpenCodePlugin(home, project)
		if err != nil {
			return leadLaunch{}, err
		}
		content, err := openCodePluginConfig(os.Getenv("OPENCODE_CONFIG_CONTENT"), plugin)
		if err != nil {
			return leadLaunch{}, err
		}
		launch.Env = map[string]string{"OPENCODE_CONFIG_CONTENT": content}
	}
	if noticeDelivery(kindConfig) == config.NoticeDeliveryPiExtension {
		extension, err := s.writeLeadPiExtension(home, project)
		if err != nil {
			return leadLaunch{}, err
		}
		launch.Args = append(launch.Args, "--extension", extension)
	}
	if profileName := cfg.Lead.Profiles[kind]; profileName != "" {
		profile, ok := cfg.Profiles[profileName]
		if !ok {
			return leadLaunch{}, axi.Failure("config_invalid", "Lead Profile "+profileName+" does not exist", false)
		}
		if profile.Kind != kind {
			return leadLaunch{}, axi.Failure("config_invalid", "Lead Profile "+profileName+" kind does not match "+kind, false)
		}
		profileArgs := resolvedProfileArgs(profile, kindConfig)
		if kindConfig.LeadAutoApprove {
			profileArgs = withoutArgumentSequence(profileArgs, kindConfig.AutoApproveArgs)
		}
		launch.Args = append(launch.Args, profileArgs...)
	}
	if kind == "opencode" {
		// --prompt starts a turn without typing into the pane. The first turn
		// creates the session required by the plugin's session API delivery.
		opening := codexOpeningPrompt
		if launch.Env == nil {
			opening = "Run `posse lead` and follow it."
		}
		launch.Args = append(launch.Args, "--prompt", opening)
		launch.StartsBusy = true
	} else if len(kindConfig.SystemPromptArgs) == 0 {
		launch.TypedPrompt = "Run `posse lead` and follow it."
	} else if noticeDelivery(kindConfig) == config.NoticeDeliveryCodexQueue {
		launch.Args = append(launch.Args, codexOpeningPrompt)
		launch.StartsBusy = true
	}
	return launch, nil
}

// withoutArgumentSequence removes Profile copies of the kind's already-added
// auto-approve arguments without changing unrelated Profile arguments.
func withoutArgumentSequence(args, sequence []string) []string {
	if len(sequence) == 0 || len(args) < len(sequence) {
		return args
	}
	result := make([]string, 0, len(args))
	for index := 0; index < len(args); {
		matches := index+len(sequence) <= len(args)
		if matches {
			for offset, argument := range sequence {
				if args[index+offset] != argument {
					matches = false
					break
				}
			}
		}
		if matches {
			index += len(sequence)
			continue
		}
		result = append(result, args[index])
		index++
	}
	return result
}

func claudeNoticeDirectory(home, project string) string {
	return filepath.Join(home, "projects", project, "lead-claude-notices")
}

func (s *Service) writeLeadClaudeLowkey(home string, project store.Project) (string, error) {
	root := filepath.Join(home, "projects", project.Name, "lead-claude-lowkey")
	for _, file := range []string{".claude-plugin/plugin.json", "FIRSTMATE-LICENSE", "hooks/hooks.json", "hooks/register.ts", "lib/presentation.ts"} {
		data, err := leadClaudeLowkey.ReadFile("claude_lowkey/" + file)
		if err != nil {
			return "", err
		}
		path := filepath.Join(root, file)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", err
		}
		if err := writeFile(path, data); err != nil {
			return "", err
		}
	}
	return root, nil
}

func (s *Service) writeLeadPiExtension(home string, project store.Project) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate posse executable for the pi extension: %w", err)
	}
	quoted, err := json.Marshal(executable)
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, "projects", project.Name, "lead-pi-extension.ts")
	contents := strings.Replace(leadPiExtension, `"posse"; // POSSE_EXECUTABLE`, string(quoted)+";", 1)
	return path, writeFile(path, []byte(contents))
}

func (s *Service) writeLeadOpenCodePlugin(home string, project store.Project) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate posse executable for the OpenCode plugin: %w", err)
	}
	quotedExec, err := json.Marshal(executable)
	if err != nil {
		return "", err
	}
	quotedLead, err := json.Marshal(filepath.Join(home, "projects", project.Name, "lead.md"))
	if err != nil {
		return "", err
	}
	contents := strings.Replace(leadOpenCodePlugin, `"posse"; // POSSE_EXECUTABLE`, string(quotedExec)+";", 1)
	contents = strings.Replace(contents, `"lead.md"; // POSSE_LEAD_FILE`, string(quotedLead)+";", 1)
	path := filepath.Join(home, "projects", project.Name, "lead-opencode-plugin.js")
	return path, writeFile(path, []byte(contents))
}

// OpenCode merges OPENCODE_CONFIG_CONTENT after user and project config,
// concatenating plugin lists. Preserve any existing inline config as well.
func openCodePluginConfig(existing, plugin string) (string, error) {
	cfg := map[string]any{}
	if existing != "" {
		if err := json.Unmarshal([]byte(existing), &cfg); err != nil {
			return "", fmt.Errorf("OPENCODE_CONFIG_CONTENT must be a JSON object to add the Lead plugin: %w", err)
		}
		if cfg == nil {
			return "", fmt.Errorf("OPENCODE_CONFIG_CONTENT must be a JSON object to add the Lead plugin")
		}
	}
	plugins, ok := cfg["plugin"]
	if ok {
		list, valid := plugins.([]any)
		if !valid {
			return "", fmt.Errorf("OPENCODE_CONFIG_CONTENT plugin must be an array")
		}
		cfg["plugin"] = append(list, "file://"+plugin)
	} else {
		cfg["plugin"] = []string{"file://" + plugin}
	}
	encoded, err := json.Marshal(cfg)
	return string(encoded), err
}

// queueCodexPrompt hands the prompt to the Lead's own codex session. codex
// holds it until the current turn ends and never touches the composer, so it is
// safe while the pane is focused or the User is typing.
func queueCodexPrompt(ctx context.Context, cwd, session, prompt string) error {
	commandContext, cancel := context.WithTimeout(ctx, codexQueueTimeout)
	defer cancel()
	command := execgroup.CommandContext(commandContext, "codex", "queue", "--thread", session, "--message", prompt)
	command.Dir = cwd
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("codex queue: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
