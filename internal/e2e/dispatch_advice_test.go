//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDispatchAlternativesAndQuotaAdviceCLI(t *testing.T) {
	f := reviewIdentityFixture(t)
	defer f.db.Close()
	stub := filepath.Join(filepath.Dir(f.binary), "quota-axi")
	script := "#!/bin/sh\n[ \"$*\" = '--json --full --no-credential-refresh --provider claude' ] || exit 9\nprintf '{\"schemaVersion\":5,\"generatedAt\":\"%s\",\"providers\":[]}' \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"\n"
	if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "dispatch", `[{type="change",when="complex work",use=["author","reviewer"]},{type="change",use="author"}]`)
	brief := filepath.Join(f.root, "dispatch-advice.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Dispatch alternatives\ndone_when: one Profile is selected\n---\nInspect the alternatives.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, output := runB05CLI(t, f, f.leadEnv, "dispatch", "--brief", brief, "--json")
	if code != 1 || !strings.Contains(output, "profile_required") || !strings.Contains(output, "quota_advice") || !strings.Contains(output, "reviewer") {
		t.Fatalf("ambiguous dispatch: %d %s", code, output)
	}
	output = runPosse(t, f.binary, f.repo, f.leadEnv, "dispatch", "--brief", brief, "--profile", "reviewer", "--json")
	var result struct {
		Profile string `json:"profile"`
		Advice  struct {
			Phase    string `json:"phase"`
			Evidence struct {
				Codex     string `json:"codex"`
				Generated string `json:"generated_at"`
			} `json:"evidence"`
		} `json:"quota_advice"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.Profile != "reviewer" || result.Advice.Phase != "selection" || result.Advice.Evidence.Generated == "" || !strings.Contains(result.Advice.Evidence.Codex, "excluded") {
		t.Fatalf("explicit Profile or safe evidence missing: %s", output)
	}
}
