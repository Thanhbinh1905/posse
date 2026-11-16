package app

import (
	"strconv"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWatchChildIdentityRequiresProjectTokenAndWatchAncestry(t *testing.T) {
	watch := store.WatchRuntime{PID: 420, Token: "watch-token", StartedAt: 987}
	flags := map[string]string{
		"watch-project-id": "17",
		"watch-token":      watch.Token,
		"watch-started-at": strconv.FormatInt(watch.StartedAt, 10),
		"watch-pid":        strconv.Itoa(watch.PID),
	}
	environment := map[string]string{
		watchRuntimeProjectEnv: strconv.FormatInt(17, 10),
		watchRuntimeTokenEnv:   watch.Token,
		watchRuntimeStartedEnv: strconv.FormatInt(watch.StartedAt, 10),
	}
	ancestors := []ancestorProcess{{pid: watch.PID}}
	if !watchChildIdentityMatches(17, flags, environment, watch, ancestors) {
		t.Fatal("valid Watch child identity was rejected")
	}

	for name, change := range map[string]func(map[string]string, map[string]string, []ancestorProcess){
		"wrong project":     func(args, env map[string]string, _ []ancestorProcess) { args["watch-project-id"] = "18" },
		"wrong token":       func(args, env map[string]string, _ []ancestorProcess) { env[watchRuntimeTokenEnv] = "forged-token" },
		"wrong start":       func(args, env map[string]string, _ []ancestorProcess) { args["watch-started-at"] = "988" },
		"not a Watch child": func(_ map[string]string, _ map[string]string, _ []ancestorProcess) {},
	} {
		t.Run(name, func(t *testing.T) {
			argsCopy := map[string]string{}
			envCopy := map[string]string{}
			for key, value := range flags {
				argsCopy[key] = value
			}
			for key, value := range environment {
				envCopy[key] = value
			}
			parentsCopy := append([]ancestorProcess(nil), ancestors...)
			change(argsCopy, envCopy, parentsCopy)
			if name == "not a Watch child" {
				parentsCopy = nil
			}
			if watchChildIdentityMatches(17, argsCopy, envCopy, watch, parentsCopy) {
				t.Fatal("invalid Watch child identity was accepted")
			}
		})
	}
}
