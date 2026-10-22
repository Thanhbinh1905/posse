package app

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/thanhbinh1905/posse/internal/axi"
)

func renderDoctorHuman(ctx *axi.Context, checks []axi.Object, help []axi.Object, verbose bool) error {
	groups := []string{"Storage", "Projects", "Integrations", "Setup"}
	byGroup := map[string][]axi.Object{}
	counts := map[string]int{}
	for _, check := range checks {
		name, _ := fieldString(check, "check")
		status, _ := fieldString(check, "status")
		group := doctorGroup(name)
		byGroup[group] = append(byGroup[group], check)
		counts[status]++
	}
	for _, group := range groups {
		if len(byGroup[group]) == 0 {
			continue
		}
		fmt.Fprintf(ctx.Out, "%s\n", group)
		for _, check := range byGroup[group] {
			name, _ := fieldString(check, "check")
			status, _ := fieldString(check, "status")
			detail, _ := fieldString(check, "detail")
			glyph := "✓"
			if status == "warn" || status == "info" {
				glyph = "!"
			} else if status == "fail" {
				glyph = "✗"
			}
			if _, err := fmt.Fprintf(ctx.Out, "  %s %s\n", glyph, name); err != nil {
				return err
			}
			if status != "ok" || verbose {
				if err := writeWrapped(ctx.Out, detail, ctx.Width); err != nil {
					return err
				}
			}
		}
	}
	fmt.Fprintf(ctx.Out, "\nSummary: %d checks, %d passed, %s, %d informational, %s\n", len(checks), counts["ok"], pluralCount(counts["warn"], "warning", "warnings"), counts["info"], pluralCount(counts["fail"], "error", "errors"))
	if len(help) > 0 {
		fmt.Fprintln(ctx.Out, "\nNext")
		for _, item := range help {
			name, _ := fieldString(item, "check")
			action, _ := fieldString(item, "action")
			if _, err := fmt.Fprintf(ctx.Out, "  - %s: %s\n", name, action); err != nil {
				return err
			}
		}
	}
	return nil
}

func pluralCount(count int, singular, plural string) string {
	if count == 1 {
		return fmt.Sprintf("1 %s", singular)
	}
	return fmt.Sprintf("%d %s", count, plural)
}

func doctorGroup(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "project "), strings.HasPrefix(lower, "execution resources"):
		return "Projects"
	case strings.Contains(lower, "setup"), strings.Contains(lower, "skills"), strings.Contains(lower, "hooks"), strings.Contains(lower, "agent configuration"), lower == "gh", lower == "no-mistakes":
		return "Setup"
	case strings.Contains(lower, "herdr"), strings.Contains(lower, "plugin"), strings.Contains(lower, "agent"), strings.Contains(lower, "forge"), strings.HasPrefix(lower, "glab"), strings.Contains(lower, "claude"):
		return "Integrations"
	default:
		return "Storage"
	}
}

func fieldString(object axi.Object, key string) (string, bool) {
	for _, field := range object {
		if field.Key == key {
			value, ok := field.Value.(string)
			return value, ok
		}
	}
	return "", false
}

func writeWrapped(out io.Writer, text string, width int) error {
	if text == "" {
		return nil
	}
	if width < 20 {
		width = 20
	}
	words := strings.Fields(text)
	line := "      "
	lineWidth := 6
	for _, word := range words {
		wordWidth := utf8.RuneCountInString(word)
		if lineWidth > 6 && lineWidth+1+wordWidth > width {
			if _, err := fmt.Fprintln(out, line); err != nil {
				return err
			}
			line, lineWidth = "      ", 6
		}
		if lineWidth > 6 {
			line += " "
			lineWidth++
		}
		line += word
		lineWidth += wordWidth
	}
	if lineWidth > 6 {
		_, err := fmt.Fprintln(out, line)
		return err
	}
	return nil
}

func renderUpdateCheckHuman(ctx *axi.Context, current, latest string, watches []watchProcess) error {
	fmt.Fprintf(ctx.Out, "Posse update\n  Current: %s\n  Latest:  %s\n", current, latest)
	if len(watches) == 0 {
		fmt.Fprintln(ctx.Out, "  Watches: none running")
	} else {
		fmt.Fprintf(ctx.Out, "  Watches: %d running; they restart automatically after installation\n", len(watches))
	}
	return nil
}

func renderUpdateInstalledHuman(ctx *axi.Context, current, version, binary string, stopped, restarted []watchProcess, reexecWarning, backupWarning string) error {
	fmt.Fprintf(ctx.Out, "Posse update\n  Current:  %s\n  Latest:   %s\n  Download: complete\n  Verify:   SHA-256 checksum passed\n  Installed: %s\n  Binary:    %s\n", current, version, version, binary)
	if len(stopped) > 0 {
		fmt.Fprintf(ctx.Out, "  Watches:  stopped %d\n", len(stopped))
	} else if len(restarted) > 0 {
		fmt.Fprintf(ctx.Out, "  Watches:  restart requested for %d\n", len(restarted))
	} else {
		fmt.Fprintln(ctx.Out, "  Watches:  none needed restarting")
	}
	for _, process := range append(append([]watchProcess(nil), stopped...), restarted...) {
		fmt.Fprintf(ctx.Out, "    %s (%s): %s\n", process.Project, process.Kind, watchRestartDescription(process))
	}
	if reexecWarning != "" {
		fmt.Fprintf(ctx.Out, "  Warning: watch restart request failed: %s\n", reexecWarning)
	}
	if backupWarning != "" {
		fmt.Fprintf(ctx.Out, "  Warning: %s\n", backupWarning)
	}
	fmt.Fprintln(ctx.Out, "Next: run `posse doctor` to verify the update.")
	return nil
}

func watchRestartDescription(process watchProcess) string {
	if process.PollOnly {
		return "Project watch will re-exec after installation or on the next Posse command"
	}
	if process.StopHook {
		return "legacy Claude Stop hook owns the next bounded Notice wait"
	}
	if process.QuietRoutine {
		return "Lead extension restarts this process automatically"
	}
	return "Lead restarts its background Watch on its next wake"
}
