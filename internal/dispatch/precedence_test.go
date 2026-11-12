package dispatch

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
)

const precedenceProfiles = `
[profiles.a]
kind = "claude"
[profiles.b]
kind = "claude"
[profiles.c]
kind = "claude"
[profiles.img]
kind = "codex"
`

// TestResolvePrecedenceAcrossGlobalAndProjectConfig covers the precedence in
// Resolve over combinations of global and Project Dispatch Rules. Candidates
// are written profile@source, with * when the candidate is the default that
// applies when no `when` condition does.
func TestResolvePrecedenceAcrossGlobalAndProjectConfig(t *testing.T) {
	for _, test := range []struct {
		name       string
		global     string
		project    string
		taskType   string
		requested  string
		wantRule   string
		wantUse    string
		candidates []string
	}{
		{
			name:     "explicit profile beats every rule and default",
			global:   "[[dispatch]]\nwhen = \"images\"\nuse = \"img\"\n[dispatch.default]\nuse = \"c\"\n",
			project:  "[[dispatch]]\ntype = \"change\"\nuse = \"a\"\n",
			taskType: "change", requested: "b",
			wantRule: "--profile b", wantUse: "b",
		},
		{
			name:     "review_loop.profile beats review rules",
			global:   "[review_loop]\nprofile = \"b\"\n",
			project:  "[[dispatch]]\ntype = \"review\"\nuse = \"a\"\n",
			taskType: "review",
			wantRule: "review_loop.profile", wantUse: "b",
		},
		{
			name:     "review_loop.profile does not apply to other types",
			global:   "[review_loop]\nprofile = \"b\"\n[[dispatch]]\ntype = \"change\"\nuse = \"a\"\n",
			taskType: "change",
			wantRule: "type=change", wantUse: "a",
		},
		{
			name:     "single typed unconditional Project rule",
			project:  "[[dispatch]]\ntype = \"change\"\nuse = \"a\"\n",
			taskType: "change",
			wantRule: "type=change", wantUse: "a",
		},
		{
			name:     "single generic unconditional global rule",
			global:   "[[dispatch]]\nuse = \"a\"\n",
			taskType: "research",
			wantRule: "when=", wantUse: "a",
		},
		{
			name:     "typed Project rule beats generic global rule",
			global:   "[[dispatch]]\nuse = \"b\"\n",
			project:  "[[dispatch]]\ntype = \"change\"\nuse = \"a\"\n",
			taskType: "change",
			wantRule: "type=change", wantUse: "a",
		},
		{
			name:     "typed global rule beats generic Project rule",
			global:   "[[dispatch]]\ntype = \"change\"\nuse = \"a\"\n",
			project:  "[[dispatch]]\nuse = \"b\"\n",
			taskType: "change",
			wantRule: "type=change", wantUse: "a",
		},
		{
			name:     "typed unconditional rule beats dispatch.default",
			global:   "[[dispatch]]\ntype = \"change\"\nuse = \"a\"\n[dispatch.default]\nuse = \"c\"\n",
			taskType: "change",
			wantRule: "type=change", wantUse: "a",
		},
		{
			name:     "generic unconditional rule beats dispatch.default",
			global:   "[dispatch.default]\nuse = \"c\"\n",
			project:  "[[dispatch]]\nuse = \"a\"\n",
			taskType: "change",
			wantRule: "when=", wantUse: "a",
		},
		{
			name:     "dispatch.default alone",
			global:   "[dispatch.default]\nuse = \"c\"\n",
			taskType: "research",
			wantRule: "dispatch.default", wantUse: "c",
		},
		{
			name:     "Project dispatch.default replaces the global one",
			global:   "[dispatch.default]\nuse = \"c\"\n",
			project:  "[dispatch.default]\nuse = \"b\"\n",
			taskType: "research",
			wantRule: "dispatch.default", wantUse: "b",
		},
		{
			name:     "rules for other types leave dispatch.default",
			global:   "[[dispatch]]\ntype = \"review\"\nwhen = \"security\"\nuse = \"a\"\n[dispatch.default]\nuse = \"c\"\n",
			taskType: "change",
			wantRule: "dispatch.default", wantUse: "c",
		},
		{
			name:       "rules for other types without a default require a Profile",
			global:     "[[dispatch]]\ntype = \"review\"\nuse = \"a\"\n",
			taskType:   "change",
			candidates: []string{},
		},
		{
			name:       "typed Project rules do not hide a generic global rule",
			global:     "[[dispatch]]\nwhen = \"images\"\nuse = \"img\"\n",
			project:    "[[dispatch]]\ntype = \"change\"\nwhen = \"heavy\"\nuse = \"a\"\n[[dispatch]]\ntype = \"change\"\nwhen = \"bug\"\nuse = \"b\"\n",
			taskType:   "change",
			candidates: []string{"a@project", "b@project", "img@global"},
		},
		{
			name:       "a conditional generic rule needs a choice beside a typed default",
			global:     "[[dispatch]]\nwhen = \"images\"\nuse = \"img\"\n",
			project:    "[[dispatch]]\ntype = \"change\"\nuse = \"a\"\n",
			taskType:   "change",
			candidates: []string{"img@global", "a@project*"},
		},
		{
			name:       "a conditional typed rule needs a choice beside a typed default",
			project:    "[[dispatch]]\ntype = \"research\"\nuse = \"a\"\n[[dispatch]]\ntype = \"research\"\nwhen = \"science\"\nuse = \"b\"\n",
			taskType:   "research",
			candidates: []string{"b@project", "a@project*"},
		},
		{
			name:       "dispatch.default is listed as the default beside conditional rules",
			global:     "[[dispatch]]\nwhen = \"images\"\nuse = \"img\"\n[[dispatch]]\nwhen = \"heavy\"\nuse = \"a\"\n[dispatch.default]\nuse = \"c\"\n",
			taskType:   "change",
			candidates: []string{"img@global", "a@global", "c@global*"},
		},
		{
			name:       "an outranked dispatch.default is not a candidate",
			global:     "[[dispatch]]\nwhen = \"images\"\nuse = \"img\"\n[dispatch.default]\nuse = \"c\"\n",
			project:    "[[dispatch]]\ntype = \"change\"\nuse = \"a\"\n",
			taskType:   "change",
			candidates: []string{"img@global", "a@project*"},
		},
		{
			name:       "several unconditional typed rules are all defaults",
			global:     "[[dispatch]]\ntype = \"review\"\nuse = \"a\"\n",
			project:    "[[dispatch]]\ntype = \"review\"\nuse = \"b\"\n",
			taskType:   "review",
			candidates: []string{"b@project*", "a@global*"},
		},
		{
			name:       "an unconditional array rule offers each Profile as a default",
			project:    "[[dispatch]]\ntype = \"review\"\nuse = [\"a\", \"b\"]\n",
			taskType:   "review",
			candidates: []string{"a@project*", "b@project*"},
		},
		{
			name:       "conditional review rules are never defaults",
			global:     "[[dispatch]]\nwhen = \"images\"\nuse = \"img\"\n[[dispatch]]\nwhen = \"heavy\"\nuse = \"a\"\n",
			taskType:   "review",
			candidates: []string{"img@global", "a@global"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			writeConfig(t, filepath.Join(home, "config.toml"), precedenceProfiles+test.global)
			writeConfig(t, filepath.Join(home, "projects", "shop", "config.toml"), test.project)
			cfg, err := config.Load(home, "shop")
			if err != nil {
				t.Fatal(err)
			}
			result, err := Resolve(cfg, test.taskType, test.requested)
			if test.candidates == nil {
				if err != nil || result.Profile != test.wantUse || result.Rule != test.wantRule {
					t.Fatalf("Resolve() = %#v, %v; want %s by %q", result, err, test.wantUse, test.wantRule)
				}
				return
			}
			var profileErr *ProfileError
			if !errors.As(err, &profileErr) || profileErr.Code != "profile_required" {
				t.Fatalf("Resolve() = %#v, %v; want profile_required", result, err)
			}
			got := []string{}
			for _, candidate := range profileErr.Candidates {
				label := candidate.Profile + "@" + candidate.Source
				if candidate.Default {
					label += "*"
				}
				got = append(got, label)
			}
			if !slices.Equal(got, test.candidates) {
				t.Fatalf("candidates = %v, want %v", got, test.candidates)
			}
		})
	}
}

func writeConfig(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimSpace(text)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
