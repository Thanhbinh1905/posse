package app

import (
	"os/exec"
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
			i += 2
		case strings.HasPrefix(arg, "-c") && len(arg) > 2:
			recordGitAlias(aliases, arg[2:])
			i++
		case strings.HasPrefix(arg, "--git-dir=") || strings.HasPrefix(arg, "--work-tree=") || strings.HasPrefix(arg, "--namespace=") || strings.HasPrefix(arg, "--config-env=") || arg == "--no-pager" || arg == "--paginate" || arg == "--bare" || arg == "--version" || arg == "--help":
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
	if subcommand == "push" || subcommand == "send-pack" || subcommand == "receive-pack" {
		return true
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
