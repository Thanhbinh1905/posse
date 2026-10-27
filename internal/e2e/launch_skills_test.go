//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLaunchSkillsAreInjectedOnlyIntoPosseSessions(t *testing.T) {
	for _, kind := range []string{"claude", "codex", "pi"} {
		t.Run(kind, func(t *testing.T) {
			root := newFixtureRoot(t, herdr.TestRootName())
			binDir := filepath.Join(root, "bin")
			if err := os.MkdirAll(binDir, 0o700); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(binDir, "posse")
			build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
			build.Dir = moduleRoot(t)
			if output, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build posse: %v\n%s", err, output)
			}
			herdrBinary, err := exec.LookPath("herdr")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(herdrBinary, filepath.Join(binDir, "herdr")); err != nil {
				t.Fatal(err)
			}
			capture := filepath.Join(root, "managed-args.log")
			unrelatedCapture := filepath.Join(root, "unrelated-args.log")
			skillReads := filepath.Join(root, "read-skills")
			if err := os.MkdirAll(skillReads, 0o700); err != nil {
				t.Fatal(err)
			}
			customSkill := filepath.Join(root, "my-skills", "go-style")
			if err := os.MkdirAll(customSkill, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(customSkill, "SKILL.md"), []byte("---\nname: go-style\ndescription: Go style for this User.\n---\n\nGo style.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			release := filepath.Join(root, "release-agents")
			fakeHarness := `#!/bin/sh
kind=${0##*/}
printf '%s|%s\n' "$kind" "$*" >> "$POSSE_E2E_CAPTURE_FILE"
reads="$POSSE_E2E_SKILL_READS"
[ -d "$reads" ] && reads="$reads/${PWD##*/}"
case "$kind" in
  claude)
    previous=
    for argument in "$@"; do
      if [ "$previous" = plugin-dir ]; then
        case "$argument" in
          "$POSSE_HOME/projects/"*/launch-skills/*)
            cat "$argument"/skills/*/SKILL.md >> "$reads"
            ;;
        esac
        previous=
        continue
      fi
      [ "$argument" = --plugin-dir ] && previous=plugin-dir || previous=
    done
    ;;
  pi)
    previous=
    for argument in "$@"; do
      if [ "$previous" = skill ]; then
        if [ -d "$argument" ]; then
          cat "$argument"/*/SKILL.md >> "$reads"
        else
          cat "$argument" >> "$reads"
        fi
        previous=
        continue
      fi
      [ "$argument" = --skill ] && previous=skill || previous=
    done
    ;;
  codex)
    if [ -n "${HERDR_PANE_ID:-}" ]; then
      for argument in "$@"; do
        case "$argument" in
          developer_instructions=*)
            POSSE_E2E_SKILL_INSTRUCTIONS=${argument#developer_instructions=}
            export POSSE_E2E_SKILL_INSTRUCTIONS
            python3 - "$reads" <<'PY'
import json, os, re, sys
instructions = json.loads(os.environ["POSSE_E2E_SKILL_INSTRUCTIONS"])
match = re.search(r'(\S+)/\{name\}/SKILL\.md when relevant, where \{name\} is one of: ([a-z, -]+)\.', instructions)
if not match:
    raise SystemExit(f"expected the frozen skills directory and names in {instructions!r}")
with open(sys.argv[1], "a", encoding="utf-8") as output:
    for name in match.group(2).split(", "):
        with open(os.path.join(match.group(1), name, "SKILL.md"), encoding="utf-8") as skill:
            output.write(skill.read())
PY
            ;;
        esac
      done
    fi
    ;;
esac
if [ -z "${HERDR_PANE_ID:-}" ]; then exit 0; fi
if [ "$kind" = codex ]; then
  state=working
else
  state=idle
fi
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent "$kind" --state "$state" >> "$POSSE_TEST_ROOT/report-agent.log" 2>&1 || exit 1
case "$PWD/" in
  "$POSSE_E2E_WORKTREES/"*)
    if [ "$kind" != codex ]; then
      IFS= read -r prompt || exit 0
      herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent "$kind" --state working >> "$POSSE_TEST_ROOT/report-agent.log" 2>&1 || exit 1
    fi
    ;;
esac
trap 'exit 0' TERM INT
while [ ! -e "$POSSE_E2E_RELEASE" ]; do sleep 0.05; done
`
			if err := os.WriteFile(filepath.Join(binDir, kind), []byte(fakeHarness), 0o700); err != nil {
				t.Fatal(err)
			}
			env := isolatedE2EEnv(t, root)
			env = setEnv(env, "POSSE_TEST_ROOT", root)
			env = setEnv(env, "POSSE_E2E_CAPTURE_FILE", capture)
			env = setEnv(env, "POSSE_E2E_SKILL_READS", skillReads)
			env = setEnv(env, "POSSE_E2E_WORKTREES", filepath.Join(root, "posse", "pool"))
			env = setEnv(env, "POSSE_E2E_RELEASE", release)
			env = setEnv(env, "PATH", binDir+string(os.PathListSeparator)+"/usr/local/bin:/usr/bin:/bin")
			env = setEnv(env, "GIT_AUTHOR_NAME", "Posse E2E")
			env = setEnv(env, "GIT_AUTHOR_EMAIL", "posse-e2e@example.test")
			env = setEnv(env, "GIT_COMMITTER_NAME", "Posse E2E")
			env = setEnv(env, "GIT_COMMITTER_EMAIL", "posse-e2e@example.test")
			for _, dir := range []string{"home", "posse", "claude", "codex", "pi"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := herdr.WriteIsolatedConfig(root); err != nil {
				t.Fatal(err)
			}
			repo := filepath.Join(root, "repo")
			remote := filepath.Join(root, "remote.git")
			initRepository(t, repo, remote, env)
			config := "[lead]\nkind = \"" + kind + "\"\n\n[defaults]\nmerge_mode = \"local\"\nauto_teardown = \"never\"\nmax_workers = 2\n\n[pool]\nkeep_idle = 1\n\n[profiles.worker]\nkind = \"" + kind + "\"\n\n[dispatch.default]\nuse = \"worker\"\n"
			skills := "\n[skills]\nlead = [\"posse-whats-next\", \"posse-catch-up\"]\nchange = [\"posse-tdd\", \"" + customSkill + "\"]\nreview = [\"posse-review\"]\nresearch = [\"posse-research\"]\n"
			if err := os.WriteFile(filepath.Join(root, "posse", "config.toml"), []byte(config+skills), 0o600); err != nil {
				t.Fatal(err)
			}

			client := herdr.NewWithEnv("herdr", env)
			startServer(t, client)
			t.Cleanup(func() {
				_ = os.WriteFile(release, []byte("release\n"), 0o600)
			})
			if err := client.CheckProtocol(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Run(context.Background(), "integration", "install", kind); err != nil {
				t.Fatalf("install isolated %s integration: %v", kind, err)
			}
			workspace, err := createWorkspace(client, repo)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Run(context.Background(), "pane", "run", workspace.RootPane.PaneID, "posse up --name shop --yes"); err != nil {
				t.Fatalf("start %s Lead: %v", kind, err)
			}
			db, err := store.Open(filepath.Join(root, "posse"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var project store.Project
			if !waitForCondition(30*time.Second, func() bool {
				project, err = db.ProjectByName(context.Background(), "shop")
				return err == nil && project.LeadPaneID != ""
			}) {
				t.Fatalf("%s Lead was not recorded", kind)
			}
			leadEnv := setEnv(env, "HERDR_ENV", "1")
			leadEnv = setEnv(leadEnv, "HERDR_PANE_ID", project.LeadPaneID)
			leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", project.HerdrWorkspaceID)
			startWorker := func(id, name string) store.Task {
				t.Helper()
				brief := filepath.Join(root, name+".md")
				if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Launch skills\ndone_when: injected skills are readable\n---\nVerify launch skill injection.\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				command := exec.Command(binary, "start", "--brief", brief, "--name", name)
				command.Dir, command.Env = repo, leadEnv
				output, startErr := command.CombinedOutput()
				if startErr != nil || !strings.Contains(string(output), "working") {
					snapshot, _ := client.Snapshot(context.Background())
					reportLog, _ := os.ReadFile(filepath.Join(root, "report-agent.log"))
					argsLog, _ := os.ReadFile(capture)
					failedTask, _ := db.Task(context.Background(), project.ID, id)
					t.Fatalf("start %s Worker: err=%v output=%s task=%#v snapshot=%#v reports=%s args=%s", kind, startErr, output, failedTask, snapshot, reportLog, argsLog)
				}
				task, err := db.Task(context.Background(), project.ID, id)
				if err != nil {
					t.Fatalf("read launched Worker Task: %v", err)
				}
				return task
			}
			platform := []string{"posse", "posse-setup", "posse-down", "posse-update"}
			task := startWorker("t1", "launch-skills")
			argsContents, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "codex":
				if strings.Count(string(argsContents), "developer_instructions=") < 2 || !strings.Contains(string(argsContents), filepath.Join("skills", "{name}", "SKILL.md")) || !strings.Contains(string(argsContents), filepath.Join(root, "posse", "projects", "shop", "launch-skills")) {
					t.Fatalf("Codex developer_instructions omitted the frozen skills directory: %s", argsContents)
				}
			case "claude":
				if strings.Count(string(argsContents), "--plugin-dir") < 2 {
					t.Fatalf("Claude launch omitted the scoped plugin argument: %s", argsContents)
				}
			case "pi":
				if strings.Count(string(argsContents), "--skill "+filepath.Join(root, "posse", "projects", "shop", "launch-skills")) < 2 {
					t.Fatalf("Pi Lead/Worker args omitted the frozen skills directory: %s", argsContents)
				}
			}
			assertReadSkills(t, kind+" Lead", filepath.Join(skillReads, filepath.Base(repo)), append(platform, "posse-whats-next", "posse-catch-up"))
			assertReadSkills(t, kind+" Change Worker", filepath.Join(skillReads, filepath.Base(task.WorktreePath)), append(platform, "posse-tdd", "go-style"))

			if err := os.WriteFile(filepath.Join(root, "posse", "config.toml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			unconfigured := startWorker("t2", "no-skills-block")
			assertReadSkills(t, kind+" Worker without a [skills] block", filepath.Join(skillReads, filepath.Base(unconfigured.WorktreePath)), platform)
			for _, path := range []string{
				filepath.Join(root, "home", ".agents", "skills"),
				filepath.Join(repo, ".agents"), filepath.Join(repo, ".claude"), filepath.Join(repo, ".pi"), filepath.Join(repo, ".codex"),
				filepath.Join(task.WorktreePath, ".agents"), filepath.Join(task.WorktreePath, ".claude"), filepath.Join(task.WorktreePath, ".pi"), filepath.Join(task.WorktreePath, ".codex"),
			} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("%s launch created a global, Project or Sandbox skill path %s: %v", kind, path, err)
				}
			}
			unrelatedEnv := setEnv(env, "POSSE_E2E_CAPTURE_FILE", unrelatedCapture)
			unrelatedSkillReads := filepath.Join(root, "unrelated-skills.log")
			unrelatedEnv = setEnv(unrelatedEnv, "POSSE_E2E_SKILL_READS", unrelatedSkillReads)
			unrelatedCommand := exec.Command(filepath.Join(binDir, kind))
			unrelatedCommand.Env = unrelatedEnv
			if output, err := unrelatedCommand.CombinedOutput(); err != nil {
				t.Fatalf("start unrelated %s session: %v\n%s", kind, err, output)
			}
			unrelatedArgs, err := os.ReadFile(unrelatedCapture)
			unrelatedSkills, readErr := os.ReadFile(unrelatedSkillReads)
			if err != nil || strings.Contains(string(unrelatedArgs), "--plugin-dir") || strings.Contains(string(unrelatedArgs), "--skill") || strings.Contains(string(unrelatedArgs), "developer_instructions") || strings.Contains(string(unrelatedArgs), "name: posse") {
				t.Fatalf("unrelated %s session received Posse skill arguments: %s err=%v", kind, unrelatedArgs, err)
			}
			if readErr != nil && !os.IsNotExist(readErr) || len(unrelatedSkills) != 0 {
				t.Fatalf("unrelated %s session read Posse skills: %s err=%v", kind, unrelatedSkills, readErr)
			}
		})
	}
}

// assertReadSkills checks that a fake harness read exactly the named skills.
func assertReadSkills(t *testing.T, session, path string, want []string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s read no injected skills: %v", session, err)
	}
	read := map[string]bool{}
	for _, match := range regexp.MustCompile(`(?m)^name: (\S+)$`).FindAllStringSubmatch(string(contents), -1) {
		read[match[1]] = true
	}
	got := make([]string, 0, len(read))
	for name := range read {
		got = append(got, name)
	}
	sort.Strings(got)
	want = append([]string(nil), want...)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s read skills %v, want %v", session, got, want)
	}
}
