//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDispatchShowsGlobalRulesBesideProjectRulesCLI(t *testing.T) {
	f := reviewIdentityFixture(t)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "dispatch", `[{when="Task requires generating images",use="reviewer"}]`)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "dispatch.default.use", "author-b")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "--project", f.project.Name, "dispatch", `[{type="change",when="Heavy work",use="author"}]`)
	brief := filepath.Join(f.root, "image-brief.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Generate a set of images\ndone_when: the images are committed\n---\nGenerate a set of images for this Project.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output := runB05CLI(t, f, f.leadEnv, "dispatch", "--brief", brief, "--json")
	if code != 1 || !strings.Contains(output, "profile_required") {
		t.Fatalf("dispatch: code=%d %s", code, output)
	}
	var failure struct {
		Help []string `json:"help"`
	}
	if err := json.Unmarshal([]byte(output), &failure); err != nil {
		t.Fatal(err)
	}
	var choices []string
	for _, line := range failure.Help {
		if strings.HasPrefix(line, "when=") {
			choices = append(choices, line)
		}
	}
	want := []string{
		`when="Heavy work" profile=author rule=type=change source=project`,
		`when="Task requires generating images" profile=reviewer rule=type=any source=global`,
		`when="" profile=author-b rule=dispatch.default source=global default`,
	}
	if strings.Join(choices, "\n") != strings.Join(want, "\n") {
		t.Fatalf("candidates:\n%s\nwant:\n%s", strings.Join(choices, "\n"), strings.Join(want, "\n"))
	}

	output = runPosse(t, f.binary, f.repo, f.env, "config", "show", "--effective", "--project", f.project.Name, "--json")
	var effective struct {
		Rules []map[string]string `json:"dispatch_rules"`
	}
	if err := json.Unmarshal([]byte(output), &effective); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	var rows []string
	for _, rule := range effective.Rules {
		rows = append(rows, rule["use"]+" "+rule["type"]+" "+rule["source"]+" "+rule["precedence"])
	}
	wantRows := []string{
		"author change project conditional: listed for the Lead to choose",
		"reviewer any global conditional: listed for the Lead to choose",
		"author-b dispatch.default global default when no unconditional rule applies",
	}
	if strings.Join(rows, "\n") != strings.Join(wantRows, "\n") {
		t.Fatalf("dispatch_rules:\n%s\nwant:\n%s", strings.Join(rows, "\n"), strings.Join(wantRows, "\n"))
	}
}
