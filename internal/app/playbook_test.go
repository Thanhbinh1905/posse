package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func writePlaybook(t *testing.T, home, relative, content string) {
	t.Helper()
	path := filepath.Join(home, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPlaybookSourcesLayerUserBeforeWorkspaceProjectAndMissingFilesAreEmpty(t *testing.T) {
	home := t.TempDir()
	writePlaybook(t, home, "playbook/rider.md", "User rider rules.\n")
	writePlaybook(t, home, "projects/stack/playbook/rider.md", "Workspace rider rules.\n")
	project := store.Project{Name: "stack", Kind: store.ProjectKindWorkspace}

	sources, err := playbookSources(home, project, "rider")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0].Layer != "User" || sources[1].Layer != "Project stack" {
		t.Fatalf("Playbook source order = %#v", sources)
	}
	effective := renderedPlaybook(sources)
	userIndex := strings.Index(effective, "User rider rules")
	projectIndex := strings.Index(effective, "Workspace rider rules")
	if userIndex < 0 || projectIndex <= userIndex || !strings.Contains(effective, sources[0].Path) || !strings.Contains(effective, sources[1].Path) {
		t.Fatalf("effective Playbook lost source order or headers: %s", effective)
	}

	missing, err := playbookSources(home, project, "lead")
	if err != nil {
		t.Fatal(err)
	}
	if renderedPlaybook(missing) != "" || len(missing) != 2 || missing[0].Exists || missing[1].Exists {
		t.Fatalf("missing Playbooks were not empty: sources=%#v effective=%q", missing, renderedPlaybook(missing))
	}
}

func TestPrepareLeadLaunchUsesOnlyLayeredLeadPlaybooks(t *testing.T) {
	home := t.TempDir()
	writePlaybook(t, home, "playbook/lead.md", "User lead instruction marker.\n")
	writePlaybook(t, home, "projects/shop/playbook/lead.md", "Project lead instruction marker.\n")
	writePlaybook(t, home, "playbook/rider.md", "Rider-only instruction marker.\n")
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	launch, err := service.prepareLeadLaunch(home, store.Project{Name: "shop"}, cfg, "codex")
	if err != nil {
		t.Fatal(err)
	}
	var instructions string
	for _, arg := range launch.Args {
		if value, ok := strings.CutPrefix(arg, "developer_instructions="); ok {
			instructions = value
			break
		}
	}
	userIndex := strings.Index(instructions, "User lead instruction marker")
	projectIndex := strings.Index(instructions, "Project lead instruction marker")
	if userIndex < 0 || projectIndex <= userIndex || strings.Contains(instructions, "Rider-only instruction marker") {
		t.Fatalf("Lead launch did not preserve role separation and layer order: %s", instructions)
	}
	if !strings.Contains(instructions, "Never edit the Project repository") || !strings.Contains(instructions, "Only the User answers Decisions") || !strings.Contains(instructions, "User-only settings") {
		t.Fatalf("Playbook overrode or omitted non-overridable Lead boundaries: %s", instructions)
	}
}

func TestPlaybookShowAndPathExposeEffectiveSources(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse-home")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProject(context.Background(), "shop", repo, "main"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writePlaybook(t, home, "playbook/rider.md", "User source marker.\n")
	writePlaybook(t, home, "projects/shop/playbook/rider.md", "Project source marker.\n")
	t.Chdir(repo)
	service := testService(home, nil)

	var output bytes.Buffer
	cli := service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"playbook", "show", "--json"}); code != 0 {
		t.Fatalf("playbook show failed: %s", output.String())
	}
	text := output.String()
	userIndex := strings.Index(text, "User source marker")
	projectIndex := strings.Index(text, "Project source marker")
	if userIndex < 0 || projectIndex <= userIndex || !strings.Contains(text, filepath.Join(home, "playbook", "rider.md")) || !strings.Contains(text, filepath.Join(home, "projects", "shop", "playbook", "rider.md")) {
		t.Fatalf("playbook show omitted effective content or sources: %s", text)
	}

	output.Reset()
	cli = service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"playbook", "path", "rider", "--json"}); code != 0 {
		t.Fatalf("playbook path failed: %s", output.String())
	}
	if !strings.Contains(output.String(), filepath.Join(home, "playbook", "rider.md")) || !strings.Contains(output.String(), filepath.Join(home, "projects", "shop", "playbook", "rider.md")) {
		t.Fatalf("playbook path omitted one of its sources: %s", output.String())
	}
}

func TestDoctorWarnsWhenEffectivePlaybookExceedsAdvisorySize(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse-home")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProject(context.Background(), "shop", repo, "main"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writePlaybook(t, home, "playbook/rider.md", strings.Repeat("u", 3000))
	writePlaybook(t, home, "projects/shop/playbook/rider.md", strings.Repeat("p", 1200))
	service := testService(home, nil)
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"doctor", "--json"}); code != 0 {
		t.Fatalf("doctor failed: %s", output.String())
	}
	var result struct {
		Checks []map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode doctor result %s: %v", output.String(), err)
	}
	for _, check := range result.Checks {
		if check["check"] == "playbook shop rider" {
			if check["status"] != "warn" || !strings.Contains(check["detail"], "4200 bytes") {
				t.Fatalf("Playbook doctor warning = %#v", check)
			}
			return
		}
	}
	t.Fatalf("doctor omitted the large effective Playbook warning: %s", output.String())
}

func TestLeadPlaybookDeliveryPreservesMarkdownForEveryKind(t *testing.T) {
	const quotedMarkdown = "```sh\nprintf 'two  spaces\\n'\n# Explain why verification is needed.\ngo test ./...\n```\n"
	const userPlaybook = "\n" + quotedMarkdown + "\n  "
	for _, kind := range []string{"claude", "codex", "pi", "opencode"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			writePlaybook(t, home, "playbook/lead.md", userPlaybook)
			cfg, err := config.Load(home, "")
			if err != nil {
				t.Fatal(err)
			}
			service := testService(home, nil)
			project := store.Project{Name: "shop"}
			launch, err := service.prepareLeadLaunch(home, project, cfg, kind)
			if err != nil {
				t.Fatal(err)
			}
			instructionsPath := filepath.Join(home, "projects", project.Name, "lead.md")
			instructions, err := os.ReadFile(instructionsPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(instructions), userPlaybook) {
				t.Fatalf("stored instructions changed the Playbook Markdown: got %q, want exact bytes %q", instructions, userPlaybook)
			}

			switch kind {
			case "codex":
				var injected string
				for _, arg := range launch.Args {
					if !strings.HasPrefix(arg, "developer_instructions=") {
						continue
					}
					var config map[string]string
					if _, err := toml.Decode(arg, &config); err != nil {
						t.Fatalf("Codex developer_instructions override is not a valid TOML string: %q: %v", arg, err)
					}
					injected = config["developer_instructions"]
					break
				}
				if injected != string(instructions) {
					t.Fatalf("Codex injected %q, want byte-for-byte instructions %q", injected, instructions)
				}
			case "claude", "pi":
				if !stringListContains(launch.Args, instructionsPath) {
					t.Fatalf("%s launch does not inject the exact instructions file: %#v", kind, launch.Args)
				}
			case "opencode":
				pluginPath := filepath.Join(home, "projects", project.Name, "lead-opencode-plugin.js")
				plugin, err := os.ReadFile(pluginPath)
				if err != nil {
					t.Fatal(err)
				}
				quotedPath, _ := json.Marshal(instructionsPath)
				if !strings.Contains(string(plugin), "const leadFile = "+string(quotedPath)+";") || !strings.Contains(string(plugin), `readFile(leadFile, "utf8")`) {
					t.Fatalf("OpenCode plugin does not read the exact instructions file: %s", plugin)
				}
			}
		})
	}
}

type playbookSetFixture struct {
	root, home, repo string
	project          store.Project
	service          *Service
}

func newPlaybookSetFixture(t *testing.T) playbookSetFixture {
	t.Helper()
	root := t.TempDir()
	repo, home := filepath.Join(root, "repo"), filepath.Join(root, "posse-home")
	initRepo(t, repo)
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return playbookSetFixture{root: root, home: home, repo: repo, project: project, service: testService(home, nil)}
}

func runPlaybookSetCLI(t *testing.T, service *Service, wantCode int, args ...string) string {
	t.Helper()
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run(args); code != wantCode {
		t.Fatalf("%v exit=%d want=%d:\n%s", args, code, wantCode, output.String())
	}
	return output.String()
}

func TestPlaybookSetWritesUserAndProjectLayersWithoutQuote(t *testing.T) {
	f := newPlaybookSetFixture(t)
	source := filepath.Join(f.root, "new-lead.md")
	const userContent = "Ask about the outcome before drafting.  Keep spacing.\n"
	if err := os.WriteFile(source, []byte(userContent), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(f.root)
	output := runPlaybookSetCLI(t, f.service, 0, "playbook", "set", "lead", "--file", source)
	globalPath := filepath.Join(f.home, "playbook", "lead.md")
	written, err := os.ReadFile(globalPath)
	if err != nil || string(written) != userContent || !strings.Contains(output, "approval_recorded: false") {
		t.Fatalf("User Playbook write = %q, err=%v, output=%s", written, err, output)
	}
	info, err := os.Stat(globalPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("User Playbook permissions = %v, err=%v", info, err)
	}

	const projectContent = "Project Rider instructions.\n"
	if err := os.WriteFile(source, []byte(projectContent), 0o600); err != nil {
		t.Fatal(err)
	}
	runPlaybookSetCLI(t, f.service, 0, "playbook", "set", "rider", "--file", source, "--project", "shop")
	projectPath := filepath.Join(f.home, "projects", "shop", "playbook", "rider.md")
	written, err = os.ReadFile(projectPath)
	if err != nil || string(written) != projectContent {
		t.Fatalf("Project Playbook write = %q, err=%v", written, err)
	}

	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var approvals int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM config_approvals`).Scan(&approvals); err != nil || approvals != 0 {
		t.Fatalf("User-shell Playbook writes recorded %d approvals, err=%v", approvals, err)
	}
}

func TestPlaybookSetRequiresAndRecordsLeadConsent(t *testing.T) {
	f := newPlaybookSetFixture(t)
	source := filepath.Join(f.root, "lead.md")
	if err := os.WriteFile(source, []byte("Approved instructions.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(f.repo)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	target := filepath.Join(f.home, "projects", "shop", "playbook", "lead.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("Previous instructions.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := runPlaybookSetCLI(t, f.service, 1, "playbook", "set", "lead", "--file", source)
	previous, err := os.ReadFile(target)
	if err != nil || string(previous) != "Previous instructions.\n" || !strings.Contains(output, "user_only") {
		t.Fatalf("unapproved Lead write changed the Playbook: contents=%q err=%v output=%s", previous, err, output)
	}

	const quote = "Please update the Lead instructions for this Project."
	output = runPlaybookSetCLI(t, f.service, 0, "playbook", "set", "lead", "--file", source, "--project", "shop", "--user-approved", quote)
	written, err := os.ReadFile(target)
	if err != nil || string(written) != "Approved instructions.\n" || !strings.Contains(output, "approval_recorded: true") {
		t.Fatalf("approved Lead write = %q, err=%v, output=%s", written, err, output)
	}
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var key, action, value, recordedQuote string
	if err := db.QueryRowContext(context.Background(), `SELECT key,action,value,user_quote FROM config_approvals WHERE project_id=?`, f.project.ID).Scan(&key, &action, &value, &recordedQuote); err != nil {
		t.Fatal(err)
	}
	if key != "playbook.lead" || action != "set" || value != target || recordedQuote != quote {
		t.Fatalf("Playbook approval = key %q action %q value %q quote %q", key, action, value, recordedQuote)
	}
}

func TestRiderCannotWritePlaybooks(t *testing.T) {
	f := newPlaybookSetFixture(t)
	source := filepath.Join(f.root, "rider.md")
	if err := os.WriteFile(source, []byte("forbidden\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(f.repo)
	t.Setenv(workerHomeEnv, f.home)
	output := runPlaybookSetCLI(t, f.service, 1, "playbook", "set", "rider", "--file", source)
	target := filepath.Join(f.home, "projects", "shop", "playbook", "rider.md")
	if _, err := os.Stat(target); !os.IsNotExist(err) || !strings.Contains(output, "worker_forbidden") {
		t.Fatalf("Rider wrote a Playbook: stat err=%v output=%s", err, output)
	}
}

func TestPlaybookContentHasNoSeparateLeadCap(t *testing.T) {
	home := t.TempDir()
	writePlaybook(t, home, "playbook/lead.md", strings.Repeat("x", playbookWarningBytes+100))
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	if _, err := service.prepareLeadLaunch(home, store.Project{Name: "shop"}, cfg, "pi"); err != nil {
		t.Fatalf("Playbook content below the existing Lead startup budget was refused: %v", err)
	}
	writePlaybook(t, home, "playbook/lead.md", strings.Repeat("x", leadInstructionMaxBytes))
	if _, err := service.prepareLeadLaunch(home, store.Project{Name: "shop"}, cfg, "pi"); err == nil || !strings.Contains(err.Error(), "lead_instructions_too_large") && !strings.Contains(err.Error(), "Lead instructions exceed") {
		t.Fatalf("Playbook content above the existing Lead startup budget was not refused: %v", err)
	}
}
