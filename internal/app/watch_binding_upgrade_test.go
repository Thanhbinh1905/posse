package app

import "testing"

func TestHistoricalSupervisedWatchBindingRetainsCustody(t *testing.T) {
	home := t.TempDir()
	environment := map[string]string{
		"POSSE_HOME":               home,
		"POSSE_LOOKOUT_PROJECT_ID": "17",
		"POSSE_LOOKOUT_TOKEN":      "historical-owner",
		"POSSE_LOOKOUT_STARTED_AT": "1234",
	}
	process, ok := watchFromArgs(123, []string{"posse", "lookout", "--poll-only"}, environment, home)
	if !ok || !process.PollOnly || process.WatchProjectID != 17 || process.WatchToken != "historical-owner" || process.WatchStartedAt != 1234 {
		t.Fatalf("lost historical ownership: %#v %v", process, ok)
	}
	canonical, ok := watchFromArgs(123, []string{"posse", "watch", "--poll-only"}, environment, home)
	if !ok || canonical.WatchProjectID != 0 || canonical.WatchToken != "" || canonical.WatchStartedAt != 0 {
		t.Fatalf("historical bindings became canonical input aliases: %#v %v", canonical, ok)
	}
	for _, key := range []string{watchRuntimeProjectEnv, watchRuntimeTokenEnv, watchRuntimeStartedEnv} {
		t.Run(key, func(t *testing.T) {
			environment[key] = "conflicting"
			defer delete(environment, key)
			if process, ok := watchFromArgs(123, []string{"posse", "lookout", "--poll-only"}, environment, home); ok {
				t.Fatalf("accepted conflicting ownership: %#v", process)
			}
		})
	}
}
