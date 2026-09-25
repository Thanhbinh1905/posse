package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// printHumanSetup writes the setup result as a checklist for people. A preview
// lists pending changes with "+" and the agent CLIs it found; an applied run
// lists the changes with "✓".
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
	for _, row := range run.plan {
		for _, line := range humanSetupRow(row, run.applied) {
			fmt.Fprintln(out, line)
		}
	}
	if run.applied {
		return
	}
	for _, kind := range run.inspection.ReferencedKinds {
		tool := agentCLIName(kind)
		if setupPrerequisiteMissing(run.prerequisites, tool) {
			fmt.Fprintf(out, "%s  %s is not on PATH\n", humanKept, tool)
		} else {
			fmt.Fprintf(out, "%s  %s is on PATH\n", humanDone, tool)
		}
	}
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
			return kept("pi Rider guard already installed")
		case "update":
			return change("Update the pi Rider guard at "+displayPath(target), "Updated the pi Rider guard at "+displayPath(target))
		case "conflict":
			return []string{humanFailed + "  " + displayPath(target) + " was not written by posse"}
		default:
			return change("Install the pi Rider guard at "+displayPath(target), "Installed the pi Rider guard at "+displayPath(target))
		}
	case "default_config":
		if action == "keep" {
			return kept("Config already exists at " + displayPath(target))
		}
		return change("Create the default config at "+displayPath(target), "Created the default config at "+displayPath(target))
	case "skill":
		name := filepath.Base(target)
		switch action {
		case "keep":
			return kept(name + " skill already installed")
		case "conflict":
			return []string{humanFailed + "  " + displayPath(target) + " was not written by posse"}
		default:
			return change("Install the "+name+" skill", "Installed the "+name+" skill")
		}
	case "session_start_hook", "guard_hook":
		agent, _ := row["agent"].(string)
		hook := "SessionStart hook"
		if step == "guard_hook" {
			hook = "Rider guard hook"
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
