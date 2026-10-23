package app

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	setupassets "github.com/thanhbinh1905/posse/internal/setup"
)

func TestPiNativeSkillDiscoveryUsesFrozenCopiesWithoutDroppingUserSkills(t *testing.T) {
	modulePath := installedPiModule(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable for Pi's native resource loader")
	}

	for _, upgraded := range []bool{true, false} {
		name := "opt-in"
		if upgraded {
			name = "upgraded"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			userHome := filepath.Join(root, "user-home")
			piHome := filepath.Join(userHome, ".pi", "agent")
			projectRoot := filepath.Join(userHome, "project")
			posseHome := filepath.Join(root, "posse-home")
			t.Setenv("HOME", userHome)
			t.Setenv("PI_CODING_AGENT_DIR", piHome)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
			t.Setenv("PI_OFFLINE", "1")

			if upgraded {
				for _, skillName := range []string{"posse", "posse-setup", "posse-down", "posse-update"} {
					writePiSkill(t, filepath.Join(userHome, ".agents", "skills", skillName), skillName, "OLD_GLOBAL_"+skillName)
				}
			}
			writePiSkill(t, filepath.Join(userHome, ".agents", "skills", "unrelated-user"), "unrelated-user", "UNRELATED_USER_SKILL")
			writePiSkill(t, filepath.Join(piHome, "skills", "pi-user"), "pi-user", "PI_USER_SKILL")
			writePiSkill(t, filepath.Join(projectRoot, ".agents", "skills", "project-user"), "project-user", "PROJECT_USER_SKILL")

			skills, err := freezePosseLaunchSkills(posseHome, "shop", projectRoot)
			if err != nil {
				t.Fatalf("freeze launch skills: %v", err)
			}
			var frozenPaths []string
			for _, skillName := range []string{"posse", "posse-setup", "posse-down", "posse-update"} {
				frozenPaths = append(frozenPaths, filepath.Join(skills.ClaudePluginDir, "skills", skillName, "SKILL.md"))
			}
			fixture, err := json.Marshal(map[string]any{
				"cwd":         projectRoot,
				"agentDir":    piHome,
				"args":        skills.PiArgs,
				"frozenPaths": frozenPaths,
				"modulePath":  modulePath,
			})
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command(node, filepath.Join("testdata", "pi-native-skill-loader.mjs"))
			command.Env = append(os.Environ(), "POSSE_PI_SKILL_TEST="+string(fixture))
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("run Pi DefaultResourceLoader: %v\n%s", err, output)
			}
			var result struct {
				Baseline piNativeLoadResult `json:"baseline"`
				Launch   piNativeLoadResult `json:"launch"`
			}
			if err := json.Unmarshal(output, &result); err != nil {
				t.Fatalf("decode Pi resource-loader output %q: %v", output, err)
			}
			if upgraded {
				for _, skillName := range []string{"posse", "posse-setup", "posse-down", "posse-update"} {
					baseline, found := loadedPiSkill(result.Baseline.Skills, skillName)
					if !found || !strings.Contains(baseline.Content, "OLD_GLOBAL_") {
						t.Errorf("native discovery did not reproduce the old global %s winner: skill=%#v found=%t", skillName, baseline, found)
					}
					if !hasPiCollision(result.Baseline.Diagnostics, skillName) {
						t.Errorf("native discovery did not report the %s collision in the upgraded baseline", skillName)
					}
				}
			} else {
				for _, skillName := range []string{"posse", "posse-setup", "posse-down", "posse-update"} {
					want, err := setupassets.Skill(skillName)
					if err != nil {
						t.Fatal(err)
					}
					baseline, found := loadedPiSkill(result.Baseline.Skills, skillName)
					if !found || !bytes.Equal([]byte(baseline.Content), want) {
						t.Errorf("native opt-in discovery did not load frozen %s: skill=%#v found=%t", skillName, baseline, found)
					}
				}
			}
			for _, diagnostic := range result.Launch.Diagnostics {
				if diagnostic.Type == "collision" {
					t.Errorf("Pi native discovery reported a skill collision: %s", diagnostic.Message)
				}
			}
			for _, skillName := range []string{"posse", "posse-setup", "posse-down", "posse-update"} {
				want, err := setupassets.Skill(skillName)
				if err != nil {
					t.Fatal(err)
				}
				loaded, found := loadedPiSkill(result.Launch.Skills, skillName)
				if !found || !bytes.Equal([]byte(loaded.Content), want) {
					t.Errorf("Pi loaded %s from %q with frozen content=%t; want exact frozen copy", skillName, loaded.Path, found && bytes.Equal([]byte(loaded.Content), want))
				}
			}
			for _, skillName := range []string{"unrelated-user", "pi-user"} {
				if _, found := loadedPiSkill(result.Launch.Skills, skillName); !found {
					t.Errorf("Pi native discovery dropped unrelated User skill %q", skillName)
				}
			}
			if _, found := loadedPiSkill(result.Launch.Skills, "project-user"); found {
				t.Error("Pi launch exposed Project skills while restoring only User skills")
			}
			if upgraded {
				for _, skillName := range []string{"posse", "posse-setup", "posse-down", "posse-update"} {
					path := filepath.Join(userHome, ".agents", "skills", skillName, "SKILL.md")
					contents, err := os.ReadFile(path)
					if err != nil || !bytes.Contains(contents, []byte("OLD_GLOBAL_")) {
						t.Errorf("legacy global %s copy was changed or removed: %q err=%v", skillName, contents, err)
					}
				}
			}
		})
	}
}

func installedPiModule(t *testing.T) string {
	t.Helper()
	pi, err := exec.LookPath("pi")
	if err != nil {
		t.Skip("Pi is not installed; native resource-loader coverage requires Pi")
	}
	pi, err = filepath.EvalSymlinks(pi)
	if err != nil {
		t.Skipf("resolve Pi executable: %v", err)
	}
	if filepath.Base(filepath.Dir(pi)) == "bin" {
		agentDir := filepath.Dir(filepath.Dir(pi))
		if version, readErr := os.ReadFile(filepath.Join(agentDir, "install", "current-version")); readErr == nil {
			modulePath := filepath.Join(agentDir, "install", "releases", strings.TrimSpace(string(version)), "node_modules", "@earendil-works", "pi-coding-agent", "dist", "index.js")
			if _, statErr := os.Stat(modulePath); statErr == nil {
				return modulePath
			}
		}
	}
	for dir := filepath.Dir(pi); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		manifest, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err != nil {
			continue
		}
		var packageInfo struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(manifest, &packageInfo) == nil && packageInfo.Name == "@earendil-works/pi-coding-agent" {
			modulePath := filepath.Join(dir, "dist", "index.js")
			if _, err := os.Stat(modulePath); err == nil {
				return modulePath
			}
		}
	}
	t.Skip("Pi's executable does not expose the native resource-loader module")
	return ""
}

func writePiSkill(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, "SKILL.md")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: test skill\n---\n\n" + content + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

type piNativeLoadResult struct {
	Skills      []piNativeSkill      `json:"skills"`
	Diagnostics []piNativeDiagnostic `json:"diagnostics"`
}

type piNativeDiagnostic struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type piNativeSkill struct {
	Name    string `json:"name"`
	Path    string `json:"filePath"`
	Content string `json:"content"`
}

func loadedPiSkill(skills []piNativeSkill, name string) (piNativeSkill, bool) {
	for _, skill := range skills {
		if skill.Name == name {
			return skill, true
		}
	}
	return piNativeSkill{}, false
}

func hasPiCollision(diagnostics []piNativeDiagnostic, name string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Type == "collision" && strings.Contains(diagnostic.Message, `name "`+name+`" collision`) {
			return true
		}
	}
	return false
}
