//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
			skillReads := filepath.Join(root, "read-skills.log")
			release := filepath.Join(root, "release-agents")
			fakeHarness := `#!/bin/sh
kind=${0##*/}
printf '%s|%s\n' "$kind" "$*" >> "$POSSE_E2E_CAPTURE_FILE"
case "$kind" in
  claude)
    previous=
    for argument in "$@"; do
      if [ "$previous" = plugin-dir ]; then
        case "$argument" in
          "$POSSE_HOME/projects/"*/launch-skills/*)
            for skill in posse posse-setup; do cat "$argument/skills/$skill/SKILL.md" >> "$POSSE_E2E_SKILL_READS"; done
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
        cat "$argument" >> "$POSSE_E2E_SKILL_READS"
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
            python3 - "$POSSE_E2E_SKILL_READS" <<'PY'
import json, os, re, sys
instructions = json.loads(os.environ["POSSE_E2E_SKILL_INSTRUCTIONS"])
paths = re.findall(r'[^ ;]+/skills/(?:posse|posse-setup)/SKILL\.md', instructions)
if len(paths) != 2:
    raise SystemExit(f"expected two exact skill paths, got {paths!r}")
with open(sys.argv[1], "a", encoding="utf-8") as output:
    for path in paths:
        with open(path, encoding="utf-8") as skill:
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
			env = setEnv(env, "POSSE_E2E_WORKTREES", filepath.Join(root, "posse", "remuda"))
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
			config := "[lead]\nkind = \"" + kind + "\"\n\n[defaults]\nlanding_mode = \"local\"\nauto_unsaddle = \"never\"\nmax_workers = 2\n\n[remuda]\nkeep_idle = 1\n\n[profiles.worker]\nkind = \"" + kind + "\"\n\n[dispatch.default]\nuse = \"worker\"\n"
			if err := os.WriteFile(filepath.Join(root, "posse", "config.toml"), []byte(config), 0o600); err != nil {
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
			brief := filepath.Join(root, "ship.md")
			if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: Launch skills\ndone_when: injected skills are readable\n---\nVerify launch skill injection.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(binary, "ride", "--brief", brief, "--name", "launch-skills")
			command.Dir, command.Env = repo, leadEnv
			output, rideErr := command.CombinedOutput()
			if rideErr != nil || !strings.Contains(string(output), "working") {
				snapshot, _ := client.Snapshot(context.Background())
				reportLog, _ := os.ReadFile(filepath.Join(root, "report-agent.log"))
				argsLog, _ := os.ReadFile(capture)
				failedTask, _ := db.Task(context.Background(), project.ID, "t1")
				t.Fatalf("start %s Rider: err=%v output=%s task=%#v snapshot=%#v reports=%s args=%s", kind, rideErr, output, failedTask, snapshot, reportLog, argsLog)
			}
			task, err := db.Task(context.Background(), project.ID, "t1")
			if err != nil {
				t.Fatalf("read launched Rider Task: %v", err)
			}
			argsContents, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "codex":
				if strings.Count(string(argsContents), "developer_instructions=") < 2 || !strings.Contains(string(argsContents), filepath.Join("skills", "posse", "SKILL.md")) || !strings.Contains(string(argsContents), filepath.Join("skills", "posse-setup", "SKILL.md")) || !strings.Contains(string(argsContents), filepath.Join(root, "posse", "projects", "shop", "launch-skills")) {
					t.Fatalf("Codex developer_instructions omitted exact snapshot paths: %s", argsContents)
				}
			case "claude":
				if strings.Count(string(argsContents), "--plugin-dir") < 2 {
					t.Fatalf("Claude launch omitted the scoped plugin argument: %s", argsContents)
				}
			case "pi":
				if strings.Count(string(argsContents), "--skill") < 4 {
					t.Fatalf("Pi Lead/Rider args omitted skill paths: %s", argsContents)
				}
			}
			contents, err := os.ReadFile(skillReads)
			if err != nil || !strings.Contains(string(contents), "name: posse") || !strings.Contains(string(contents), "name: posse-setup") {
				t.Fatalf("%s fake harness could not read both injected skills: %s err=%v", kind, contents, err)
			}
			for _, path := range []string{
				filepath.Join(root, "home", ".agents", "skills"),
				filepath.Join(repo, ".agents"), filepath.Join(repo, ".claude"), filepath.Join(repo, ".pi"), filepath.Join(repo, ".codex"),
				filepath.Join(task.WorktreePath, ".agents"), filepath.Join(task.WorktreePath, ".claude"), filepath.Join(task.WorktreePath, ".pi"), filepath.Join(task.WorktreePath, ".codex"),
			} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("%s launch created a global, Project or Mount skill path %s: %v", kind, path, err)
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
