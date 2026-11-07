package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
)

// Marks shared with install.sh, which indents these lines under its own steps.
const (
	humanDone    = "✓"
	humanPending = "+"
	humanKept    = "-"
	humanManual  = "!"
	humanFailed  = "✗"
)

// printHumanSetup writes the setup result as a short checklist for people,
// one line per area: "+" will be added, "✓" done, "-" already set, "!" needs
// the User, "✗" failed. Detail lines under an area are indented.
func printHumanSetup(out io.Writer, run setupRun, err error) {
	if err != nil {
		var structured *axi.Error
		if !errors.As(err, &structured) {
			structured = axi.Failure("internal_error", err.Error(), false)
		}
		fmt.Fprintf(out, "%s  %s\n", humanFailed, structured.Message)
		for _, help := range structured.Help {
			fmt.Fprintf(out, "   %s\n", help)
		}
		return
	}
	for _, line := range humanSetupGroups(run.plan, run.applied) {
		fmt.Fprintln(out, line)
	}
	if run.applied {
		return
	}
	for _, kind := range run.inspection.ReferencedKinds {
		tool := agentCLIName(kind)
		if setupPrerequisiteMissing(run.prerequisites, tool) {
			fmt.Fprintf(out, "%s  %s is in your config but not on PATH\n", humanManual, tool)
		}
	}
}

// Severity of a checklist area; the worst row decides the area's mark.
const (
	humanStateKept = iota
	humanStateChanged
	humanStateManual
	humanStateFailed
)

type humanGroup struct {
	label   string
	agents  []string
	state   int
	note    string
	details []string
	codex   bool
}

func humanSetupGroups(plan []map[string]any, applied bool) []string {
	order := []string{"herdr", "sidebar", "session", "guard", "skills", "global"}
	labels := map[string]string{
		"herdr":   "Herdr plugin and agent integrations",
		"sidebar": "Agents sidebar in Herdr",
		"session": "SessionStart hook",
		"guard":   "Worker guard hook",
		"skills":  "Recommended workflow skills",
		"global":  "Global copies of the posse skills",
	}
	groups := map[string]*humanGroup{}
	for _, row := range plan {
		key, state, note, include := humanRowState(row, applied)
		if !include {
			continue
		}
		group := groups[key]
		if group == nil {
			group = &humanGroup{label: labels[key]}
			groups[key] = group
		}
		if agent, _ := row["agent"].(string); agent != "" && !slices.Contains(group.agents, agent) {
			group.agents = append(group.agents, agent)
		}
		if row["step"] == "pi_guard" && !slices.Contains(group.agents, "pi") {
			group.agents = append(group.agents, "pi")
		}
		if agent, _ := row["agent"].(string); agent == "Codex" && state == humanStateChanged {
			group.codex = true
		}
		if state > group.state {
			group.state = state
		}
		if note != "" && group.note == "" {
			group.note = note
		}
		if state >= humanStateManual || row["action"] == "remove_preserving" {
			for index, line := range humanSetupRow(row, applied) {
				if index == 0 {
					line = "     " + line[strings.Index(line, "  ")+2:]
				}
				group.details = append(group.details, line)
			}
		}
	}
	codexNoted := false
	var lines []string
	for _, key := range order {
		group := groups[key]
		if group == nil {
			continue
		}
		label := group.label
		if len(group.agents) > 0 {
			label += " for " + strings.Join(group.agents, ", ")
		}
		switch group.state {
		case humanStateFailed:
			lines = append(lines, humanFailed+"  "+label+": failed")
		case humanStateManual:
			lines = append(lines, humanManual+"  "+label+": see below")
		case humanStateChanged:
			if applied {
				lines = append(lines, humanDone+"  "+label)
			} else {
				lines = append(lines, humanPending+"  "+label+": will be added")
			}
		default:
			note := group.note
			if note == "" {
				note = "already set"
			}
			lines = append(lines, humanKept+"  "+label+": "+note)
		}
		lines = append(lines, group.details...)
		if group.codex && !codexNoted {
			lines = append(lines, "     Codex asks once to trust its new hook.")
			codexNoted = true
		}
	}
	return lines
}

// humanRowState places one plan row in its checklist area. Rows that only
// offer optional global skill copies are left out.
func humanRowState(row map[string]any, applied bool) (key string, state int, note string, include bool) {
	step, _ := row["step"].(string)
	action, _ := row["action"].(string)
	switch step {
	case "integration", "plugin":
		key = "herdr"
	case "sidebar_layout":
		key = "sidebar"
		switch {
		case action == "offer":
			return key, humanStateKept, "optional (posse setup --sidebar-layout)", true
		case action == "upgrade":
			return key, humanStateKept, "update available (posse setup --sidebar-layout)", true
		case action == "manual" && row["error"] == nil:
			return key, humanStateKept, "your own rows are kept", true
		}
	case "session_start_hook":
		key = "session"
	case "guard_hook", "pi_guard":
		key = "guard"
	case "recommended_skills":
		key = "skills"
	case "recommended_skills_offer":
		key = "skills"
		switch action {
		case "keep":
			status, _ := row["status"].(string)
			return key, humanStateKept, status, true
		case "complete":
			return key, humanStateKept, "", true
		case "offer":
			return key, humanStateKept, "not in config; add them with `posse skills add`", true
		case "fresh":
			return key, humanStateChanged, "", true
		}
	case "default_config":
		return "", 0, "", false
	case "skill":
		key = "global"
		switch action {
		case "offer_global":
			return "", 0, "", false
		case "keep":
			if note, _ := row["note"].(string); strings.Contains(note, "no Posse-owned global files") {
				return "", 0, "", false
			}
		case "offer_removal":
			return key, humanStateKept, "found; remove unchanged copies with `posse setup --remove-global-skills`", true
		case "preserve":
			return key, humanStateKept, "modified or foreign files are preserved", true
		case "remove_preserving":
			return key, humanStateChanged, "", true
		}
	}
	lines := humanSetupRow(row, applied)
	switch {
	case strings.HasPrefix(lines[0], humanFailed):
		state = humanStateFailed
	case strings.HasPrefix(lines[0], humanManual):
		state = humanStateManual
	case strings.HasPrefix(lines[0], humanKept):
		state = humanStateKept
	default:
		state = humanStateChanged
	}
	return key, state, "", true
}

func humanSetupRow(row map[string]any, applied bool) []string {
	step, _ := row["step"].(string)
	target, _ := row["target"].(string)
	action, _ := row["action"].(string)
	change := func(pending, done string) []string {
		if applied {
			return []string{humanDone + "  " + done}
		}
		return []string{humanPending + "  " + pending}
	}
	kept := func(text string) []string { return []string{humanKept + "  " + text} }
	switch step {
	case "sidebar_layout":
		switch action {
		case "keep":
			return kept("Posse Agents sidebar layout already present at " + displayPath(target))
		case "installed":
			return []string{humanDone + "  Added Posse Agents sidebar layout to " + displayPath(target)}
		case "symlink":
			lines := []string{humanManual + "  " + displayPath(target) + " is managed elsewhere; add this layout through its config manager:"}
			snippet, _ := row["snippet"].(string)
			for _, line := range strings.Split(strings.TrimSuffix(snippet, "\n"), "\n") {
				lines = append(lines, "     "+line)
			}
			return lines
		case "upgrade":
			lines := []string{humanManual + "  Upgrade Posse Agents sidebar layout in " + displayPath(target) + " (adds dim = false)? Run `posse setup` to confirm:"}
			return append(lines, sidebarSnippetLines(row)...)
		case "manual":
			message := "Herdr Agents rows already configured at " + displayPath(target)
			if reason, ok := row["error"].(string); ok && reason != "" {
				message = "Cannot add Herdr Agents layout at " + displayPath(target) + ": " + reason
			}
			lines := []string{humanManual + "  " + message + "; reference layout:"}
			snippet, _ := row["snippet"].(string)
			for _, line := range strings.Split(strings.TrimSuffix(snippet, "\n"), "\n") {
				lines = append(lines, "     "+line)
			}
			return lines
		default:
			lines := []string{humanManual + "  Add Posse Agents sidebar layout to " + displayPath(target) + "? Run `posse setup` to confirm:"}
			return append(lines, sidebarSnippetLines(row)...)
		}
	case "integration":
		switch action {
		case "keep":
			return kept("Herdr integration for " + target + " already installed")
		case "repair":
			return change("Repair the Herdr integration for "+target, "Repaired the Herdr integration for "+target)
		default:
			return change("Install the Herdr integration for "+target, "Installed the Herdr integration for "+target)
		}
	case "plugin":
		switch action {
		case "keep":
			return kept("posse plugin already linked into Herdr")
		case "update":
			return change("Update the posse plugin in Herdr", "Updated the posse plugin in Herdr")
		case "conflict":
			return []string{humanFailed + "  " + displayPath(target) + " was not written by posse"}
		default:
			return change("Link the posse plugin into Herdr", "Linked the posse plugin into Herdr")
		}
	case "pi_guard":
		switch action {
		case "keep":
			return kept("pi Worker guard already installed")
		case "update":
			return change("Update the pi Worker guard at "+displayPath(target), "Updated the pi Worker guard at "+displayPath(target))
		case "conflict":
			return []string{humanFailed + "  " + displayPath(target) + " was not written by posse"}
		default:
			return change("Install the pi Worker guard at "+displayPath(target), "Installed the pi Worker guard at "+displayPath(target))
		}
	case "default_config":
		if action == "keep" {
			return kept("Config already exists at " + displayPath(target))
		}
		return change("Create the default config at "+displayPath(target), "Created the default config at "+displayPath(target))
	case "recommended_skills":
		return change("Add the recommended workflow skills to "+displayPath(target)+" so this upgrade keeps loading them", "Added the recommended workflow skills to "+displayPath(target))
	case "recommended_skills_offer":
		switch action {
		case "installed":
			return []string{humanDone + "  Added the recommended workflow skills to " + displayPath(target)}
		case "keep":
			status, _ := row["status"].(string)
			return kept(status)
		default:
			return []string{humanManual + "  No workflow skills in " + displayPath(target) + "; run `posse setup` to add the recommended set, or `posse skills add` to pick"}
		}
	case "skill":
		name := filepath.Base(target)
		switch action {
		case "keep":
			if note, _ := row["note"].(string); strings.Contains(note, "no Posse-owned global files") {
				return kept("No Posse-owned global files for the " + name + " skill")
			}
			return kept("The global " + name + " skill is already installed")
		case "offer_global":
			return []string{humanManual + "  The " + name + " skill is available only to Posse-launched sessions; optionally install it globally with `posse setup --global-skills`"}
		case "offer_removal":
			return []string{humanManual + "  Posse-owned global " + name + " skill detected; remove unchanged files with `posse setup --remove-global-skills`"}
		case "preserve":
			return []string{humanManual + "  Modified or foreign files for the " + name + " skill are preserved"}
		case "remove":
			return change("Remove unchanged Posse-owned global files for the "+name+" skill", "Removed the Posse-owned global "+name+" skill")
		case "remove_preserving":
			if applied {
				return []string{humanDone + "  Removed unchanged Posse-owned files for the " + name + " skill; modified or foreign files were preserved"}
			}
			return []string{humanPending + "  Remove unchanged Posse-owned files for the " + name + " skill; modified or foreign files will be preserved"}
		case "conflict":
			return []string{humanFailed + "  " + displayPath(target) + " was not written by posse"}
		default:
			return change("Install the "+name+" skill globally", "Installed the "+name+" skill globally")
		}
	case "session_start_hook", "guard_hook":
		agent, _ := row["agent"].(string)
		hook := "SessionStart hook"
		if step == "guard_hook" {
			hook = "Worker guard hook"
		}
		if resolved, ok := row["resolved_target"].(string); ok && resolved != "" {
			target = resolved
		}
		suffix := ""
		if agent == "Codex" {
			suffix = "; Codex asks once to trust it"
		}
		switch action {
		case "keep":
			return kept(agent + " " + hook + " already present")
		case "stamp_version":
			return change("Record the "+agent+" "+hook+" for this posse version", "Recorded the "+agent+" "+hook+" for this posse version")
		case "setup_hook_symlink":
			lines := []string{humanManual + "  " + displayPath(target) + " is read-only; add this " + agent + " " + hook + " through its config manager:"}
			snippet, _ := row["snippet"].(string)
			for _, line := range strings.Split(snippet, "\n") {
				lines = append(lines, "     "+line)
			}
			return lines
		case "invalid":
			message, _ := row["error"].(string)
			return []string{humanFailed + "  Cannot add the " + agent + " " + hook + " to " + displayPath(target) + ": " + message}
		default:
			return change("Add the "+agent+" "+hook+" to "+displayPath(target)+suffix, "Added the "+agent+" "+hook+" to "+displayPath(target)+suffix)
		}
	}
	return change(step+" "+displayPath(target), step+" "+displayPath(target))
}

func sidebarSnippetLines(row map[string]any) []string {
	snippet, _ := row["snippet"].(string)
	lines := make([]string, 0, strings.Count(snippet, "\n"))
	for _, line := range strings.Split(strings.TrimSuffix(snippet, "\n"), "\n") {
		lines = append(lines, "     "+line)
	}
	return lines
}

func displayPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == string(filepath.Separator) {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}
