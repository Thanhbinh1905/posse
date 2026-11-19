package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGitForHazardTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func TestGreenhouseRepositoryHazards(t *testing.T) {
	for _, test := range []struct {
		name   string
		config string
		hook   string
	}{
		{name: "core hooksPath", config: "core.hooksPath=/tmp/hooks"},
		{name: "signing", config: "commit.gpgsign=true"},
		{name: "filter", config: "filter.lfs.clean=git-lfs clean -- %f"},
		{name: "executable hook", hook: "pre-commit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := t.TempDir()
			runGitForHazardTest(t, repo, "init", "-q")
			if test.config != "" {
				key, value, ok := strings.Cut(test.config, "=")
				if !ok {
					t.Fatal("invalid hazard fixture")
				}
				runGitForHazardTest(t, repo, "config", "--local", key, value)
			}
			if test.hook != "" {
				hooks := filepath.Join(repo, ".git", "hooks")
				if err := os.WriteFile(filepath.Join(hooks, test.hook), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := requireGreenhouseRepositorySafe(repo); err == nil {
				t.Fatal("accepted repository with unsupported Greenhouse hazard")
			}
		})
	}
}
