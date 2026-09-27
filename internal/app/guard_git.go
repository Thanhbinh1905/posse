package app

import (
	"os/exec"
	"strconv"
	"strings"
)

// Git options precede the subcommand; "push" in a commit message or in
// `git stash push` is not a remote write. Dynamic subcommands fail closed.
func gitRemoteWrite(e *shellEnv, args []string, literal []bool) bool {
	return gitRemoteWriteDepth(e, args, literal, 0)
}

func gitRemoteWriteDepth(e *shellEnv, args []string, literal []bool, depth int) bool {
	if depth >= 6 {
		return true
	}
	e = e.clone() // -C applies only to this git invocation, not the shell.
	aliases := map[string]string{}
	i := 0
	for i < len(args) {
		if !literal[i] {
			return true
		}
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			break
		}
		switch {
		case arg == "-c" || arg == "-C" || arg == "--git-dir" || arg == "--work-tree" || arg == "--namespace" || arg == "--config-env":
			if i+1 >= len(args) || !literal[i+1] {
				return true
			}
			if arg == "-c" {
				recordGitAlias(aliases, args[i+1])
			}
			if arg == "-C" {
				e.cwd = e.path(args[i+1])
			}
			if arg == "--config-env" {
				recordGitConfigEnvAlias(aliases, e, args[i+1])
			}
			i += 2
		case strings.HasPrefix(arg, "-c") && len(arg) > 2:
			recordGitAlias(aliases, arg[2:])
			i++
		case strings.HasPrefix(arg, "-C") && len(arg) > 2:
			e.cwd = e.path(arg[2:])
			i++
		case strings.HasPrefix(arg, "--config-env="):
			recordGitConfigEnvAlias(aliases, e, strings.TrimPrefix(arg, "--config-env="))
			i++
		case strings.HasPrefix(arg, "--git-dir=") || strings.HasPrefix(arg, "--work-tree=") || strings.HasPrefix(arg, "--namespace=") || strings.HasPrefix(arg, "--exec-path=") || strings.HasPrefix(arg, "--list-cmds=") ||
			oneOfString(arg, "-P", "-p", "--no-pager", "--paginate", "--bare", "--version", "--help", "--no-optional-locks", "--no-replace-objects", "--literal-pathspecs", "--glob-pathspecs", "--noglob-pathspecs", "--icase-pathspecs", "--exec-path", "--html-path", "--man-path", "--info-path", "--no-lazy-fetch", "--no-advice"):
			i++
		default:
			// An unknown option may consume a value. Do not misidentify its value
			// as a harmless subcommand.
			return true
		}
	}
	if i >= len(args) {
		return false
	}
	if !literal[i] {
		return true
	}
	subcommand := args[i]
	if oneOfString(subcommand, "push", "send-pack", "receive-pack", "http-push") {
		return true
	}
	if subcommand == "subtree" {
		for j := i + 1; j < len(args); j++ {
			if !literal[j] || args[j] == "push" {
				return true
			}
		}
	}
	if subcommand == "config" {
		for j := i + 1; j < len(args); j++ {
			// An alias write in the same shell command changes what a later
			// git invocation will execute; the hook runs before that write.
			if strings.HasPrefix(args[j], "alias.") && j+1 < len(args) && args[j-1] != "--get" {
				return true
			}
		}
	}
	count, _ := strconv.Atoi(e.values["GIT_CONFIG_COUNT"])
	if count > 128 || e.unknown["GIT_CONFIG_COUNT"] {
		return true
	}
	for n := 0; n < count; n++ {
		if e.unknown["GIT_CONFIG_KEY_"+strconv.Itoa(n)] || e.unknown["GIT_CONFIG_VALUE_"+strconv.Itoa(n)] {
			return true
		}
	}
	for n := 0; n < count; n++ {
		if e.values["GIT_CONFIG_KEY_"+strconv.Itoa(n)] == "alias."+subcommand {
			aliases[subcommand] = e.values["GIT_CONFIG_VALUE_"+strconv.Itoa(n)]
		}
	}
	alias := aliases[subcommand]
	if alias == "" && e.cwd != "" {
		output, err := exec.Command("git", "-C", e.cwd, "config", "--get", "alias."+subcommand).Output()
		if err == nil {
			alias = strings.TrimSpace(string(output))
		}
	}
	if alias != "" {
		// Shell aliases can run arbitrary commands; ordinary aliases are parsed
		// recursively, including `alias.p = push`.
		if strings.HasPrefix(alias, "!") {
			return true
		}
		fields := strings.Fields(alias)
		if len(fields) == 0 {
			return true
		}
		return gitRemoteWriteDepth(e, fields, allLiteral(len(fields)), depth+1)
	}
	return false
}

func recordGitConfigEnvAlias(aliases map[string]string, e *shellEnv, spec string) {
	key, name, ok := strings.Cut(spec, "=")
	if ok && strings.HasPrefix(key, "alias.") {
		aliases[strings.TrimPrefix(key, "alias.")] = e.values[name]
	}
}

func recordGitAlias(aliases map[string]string, config string) {
	key, value, ok := strings.Cut(config, "=")
	if ok && strings.HasPrefix(key, "alias.") {
		aliases[strings.TrimPrefix(key, "alias.")] = value
	}
}

func allLiteral(size int) []bool {
	result := make([]bool, size)
	for i := range result {
		result[i] = true
	}
	return result
}
