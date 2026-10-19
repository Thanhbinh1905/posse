package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestFunctionalVocabularyHelp(t *testing.T) {
	service := testService(t.TempDir(), herdr.NewFake())
	cli := service.CLI()
	var output bytes.Buffer
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"--help"}); code != 0 {
		t.Fatal(output.String())
	}
	help := output.String()
	for _, name := range []string{"start", "signal", "teardown", "watch", "pool", "list", "merge"} {
		output.Reset()
		if code := cli.Run([]string{name, "--help"}); code != 0 {
			t.Errorf("%s help: %d %s", name, code, output.String())
		}
	}
	for _, old := range []string{"Rider", "Remuda", "Mount", "Lookout", "Sheriff", "Scout", "Ship Task", "posse playbook", "posse sweep"} {
		if strings.Contains(help, old) {
			t.Errorf("help contains old vocabulary %q: %s", old, help)
		}
	}
	output.Reset()
	if code := cli.Run([]string{"merge", "--help"}); code != 0 || !strings.Contains(output.String(), "--execute") || strings.Contains(output.String(), "--merge") {
		t.Fatalf("merge help: %s", output.String())
	}
}

func TestRemovedVocabularyCommandsFailWithReplacement(t *testing.T) {
	service := testService(t.TempDir(), herdr.NewFake())
	for _, pair := range [][2]string{{"ride", "start"}, {"holler", "signal"}, {"unsaddle", "teardown"}, {"lookout", "watch"}, {"remuda", "pool"}, {"roster", "list"}, {"land", "merge"}, {"sweep", "pool prune"}, {"playbook", "preferences"}} {
		t.Run(pair[0], func(t *testing.T) {
			for _, suffix := range []string{"ignored", "--help"} {
				code, output := runCLI(t, service, pair[0], suffix)
				if code == 0 || !strings.Contains(output, "was removed; use posse "+pair[1]) {
					t.Fatalf("legacy command still works or lacks replacement: %d %s", code, output)
				}
			}
		})
	}
}

func TestRemovedVocabularyFlagsFailBeforeEffects(t *testing.T) {
	for _, item := range []struct{ command, old, replacement string }{{"merge", "merge", "execute"}, {"start", "from-leftover", "from-orphan"}, {"update", "stop-lookouts", "stop-watches"}} {
		_, err := parseArgs(item.command, []string{"--" + item.old}, nil)
		if err == nil || !strings.Contains(err.Error(), "use --"+item.replacement) {
			t.Fatalf("%s --%s: %v", item.command, item.old, err)
		}
	}
}
