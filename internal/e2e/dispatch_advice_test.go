//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDispatchAlternativesWithoutQuotaCollectionCLI(t *testing.T) {
	f := reviewIdentityFixture(t)
	defer f.db.Close()
	marker := filepath.Join(f.root, "quota-called")
	stub := filepath.Join(filepath.Dir(f.binary), "quota-axi")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ntouch "+shellQuote(marker)+"\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "dispatch", `[{type="change",use="author"},{type="change",when="complex work",use=["author","reviewer"]}]`)
	brief := filepath.Join(f.root, "dispatch-alternatives.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Dispatch alternatives\ndone_when: one Profile is selected\n---\nInspect the alternatives.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, output := runB05CLI(t, f, f.leadEnv, "dispatch", "--brief", brief, "--json")
	if code != 1 || !strings.Contains(output, "profile_required") || !strings.Contains(output, "reviewer") || strings.Contains(output, "quota_advice") {
		t.Fatalf("ambiguous dispatch: %d %s", code, output)
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
	if len(choices) != 3 || !strings.Contains(choices[0], "complex work") || !strings.Contains(choices[1], "complex work") || choices[2] != `when="" profile=author rule=type=change source=global default` {
		t.Fatalf("conditional ordering: %+v", choices)
	}
	output = runPosse(t, f.binary, f.repo, f.leadEnv, "dispatch", "--brief", brief, "--profile", "reviewer", "--json")
	var result struct {
		Profile string `json:"profile"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.Profile != "reviewer" || strings.Contains(output, "quota_advice") {
		t.Fatalf("explicit Profile lost: %s", output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("core invoked quota-axi: %v", err)
	}
	for _, use := range []string{`[]`, `[""]`, `[1]`, `["missing"]`} {
		code, output := runB05CLI(t, f, f.env, "config", "set", "dispatch", `[{type="change",use=`+use+`}]`)
		if code == 0 || !strings.Contains(output, "config_invalid") {
			t.Fatalf("accepted invalid alternatives %s: %d %s", use, code, output)
		}
	}
}
