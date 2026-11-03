package app

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestStartRejectsUserChoiceWithoutBriefPreferences(t *testing.T) {
	f := newStartFixture(t)
	brief := "---\ntype: research\ntitle: Research probe\ndone_when: findings are recorded\n---\nInvestigate.\n"
	if err := os.WriteFile(f.brief, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	cli := f.service.CLI()
	cli.Out, cli.ErrOut = output, output
	code := cli.Run([]string{"start", "--brief", f.brief, "--name", "research-probe", "--user-approved", "no preference selected"})
	if code != 1 || !strings.Contains(output.String(), "user_choice_unused") || !strings.Contains(output.String(), "Drop --user-approved") {
		t.Fatalf("unused User choice refusal: code=%d output=%s", code, output.String())
	}
	if len(f.fake.Calls) != 0 {
		t.Fatalf("start prepared resources before refusal: %#v", f.fake.Calls)
	}
}
