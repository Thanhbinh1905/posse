package research

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MinimumVersions are the oldest harness releases whose research interfaces
// Posse verified: Claude Code SubagentStart/SubagentStop hooks with agent_id
// in subagent tool hooks, `codex exec --json` threads with `exec resume` and
// rollout sandbox records, and Pi JSON mode with persistent --session files,
// --tools and --no-extensions.
var MinimumVersions = map[Harness]string{
	Claude: "2.1.0",
	Codex:  "0.160.0",
	Pi:     "1.2.0",
}

var versionPattern = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// ParseVersion extracts the first major.minor.patch from a --version line.
func ParseVersion(output string) ([3]int, bool) {
	match := versionPattern.FindStringSubmatch(output)
	if match == nil {
		return [3]int{}, false
	}
	var version [3]int
	for i := range version {
		version[i], _ = strconv.Atoi(match[i+1])
	}
	return version, true
}

// CheckVersion refuses a harness whose reported version is missing or older
// than its verified minimum.
func CheckVersion(harness Harness, output string) (string, error) {
	minimum, ok := MinimumVersions[harness]
	if !ok {
		return "", fmt.Errorf("no research adapter for %s", harness)
	}
	installed, found := ParseVersion(output)
	if !found {
		return "", fmt.Errorf("%s did not report a version (%q); research children are refused", harness, strings.TrimSpace(firstLine(output)))
	}
	required, _ := ParseVersion(minimum)
	rendered := fmt.Sprintf("%d.%d.%d", installed[0], installed[1], installed[2])
	for i := range installed {
		if installed[i] != required[i] {
			if installed[i] < required[i] {
				return rendered, fmt.Errorf("%s %s is older than %s, the oldest version with a verified research interface; research children are refused", harness, rendered, minimum)
			}
			break
		}
	}
	return rendered, nil
}

func firstLine(value string) string {
	line, _, _ := strings.Cut(value, "\n")
	return line
}
