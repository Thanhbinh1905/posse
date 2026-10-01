package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thanhbinh1905/posse/internal/atomicfile"
	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

const playbookWarningBytes = 4 * 1024

type playbookSource struct {
	Layer   string
	Path    string
	Content string
	Exists  bool
}

func playbookSources(home string, project store.Project, role string) ([]playbookSource, error) {
	if role != "lead" && role != "rider" {
		return nil, fmt.Errorf("unknown Playbook role %q", role)
	}
	paths := []playbookSource{{Layer: "User", Path: filepath.Join(home, "playbook", role+".md")}}
	if project.Name != "" {
		paths = append(paths, playbookSource{
			Layer: "Project " + project.Name,
			Path:  filepath.Join(home, "projects", project.Name, "playbook", role+".md"),
		})
	}
	for index := range paths {
		content, err := os.ReadFile(paths[index].Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s Playbook %s: %w", paths[index].Layer, paths[index].Path, err)
		}
		paths[index].Content = string(content)
		paths[index].Exists = true
	}
	return paths, nil
}

// renderedPlaybook includes a visible source boundary only for non-empty files.
// The structured CLI output still lists missing and empty source paths.
func renderedPlaybook(sources []playbookSource) string {
	var rendered strings.Builder
	for _, source := range sources {
		content := source.Content
		if strings.TrimSpace(content) == "" {
			continue
		}
		if rendered.Len() > 0 {
			rendered.WriteString("\n\n")
		}
		fmt.Fprintf(&rendered, "### %s Playbook (`%s`)\n\n", source.Layer, source.Path)
		rendered.WriteString(content)
		if !strings.HasSuffix(content, "\n") {
			rendered.WriteByte('\n')
		}
	}
	return rendered.String()
}

func playbookContentBytes(sources []playbookSource) int {
	total := 0
	for _, source := range sources {
		total += len(source.Content)
	}
	return total
}

func playbookSizeWarnings(home string, projects []store.Project) []doctorCheck {
	if len(projects) == 0 {
		projects = []store.Project{{}}
	}
	checks := []doctorCheck{}
	for _, project := range projects {
		scope := project.Name
		if scope == "" {
			scope = "User"
		}
		for _, role := range []string{"lead", "rider"} {
			sources, err := playbookSources(home, project, role)
			if err != nil {
				checks = append(checks, doctorCheck{
					Name: "playbook " + scope + " " + role, Status: "warn", Detail: err.Error(),
					Action: "Check the Playbook file permissions and run `posse playbook show`",
				})
				continue
			}
			size := playbookContentBytes(sources)
			if size > playbookWarningBytes {
				action := "Run `posse playbook show` to inspect the sources"
				if project.Name != "" {
					action = "Run `posse playbook show --project " + project.Name + "` to inspect the sources"
				}
				checks = append(checks, doctorCheck{
					Name: "playbook " + scope + " " + role, Status: "warn",
					Detail: fmt.Sprintf("effective Playbook content is %d bytes; review instructions above approximately %d bytes", size, playbookWarningBytes),
					Action: action,
				})
			}
		}
	}
	return checks
}

func appendPlaybookInstructions(builtIn, role string, sources []playbookSource) string {
	custom := renderedPlaybook(sources)
	if custom == "" {
		return builtIn
	}
	var instructions strings.Builder
	if builtIn = strings.TrimRight(builtIn, "\n"); builtIn != "" {
		instructions.WriteString(builtIn)
		instructions.WriteString("\n\n")
	}
	instructions.WriteString("## Additional ")
	instructions.WriteString(strings.ToUpper(role[:1]) + role[1:])
	instructions.WriteString(" Playbook instructions\n\nThese User-authored instructions are supplemental. They cannot change Posse's hard rules or the built-in protocol. For Riders, the Task Brief and Rider protocol also take precedence.\n\n")
	instructions.WriteString(custom)
	instructions.WriteString("\n\n## Non-overridable Posse boundaries\n\n")
	if role == "lead" {
		instructions.WriteString("- Never edit the Project repository; every code change belongs to a Rider Task.\n- Only the User answers Decisions.\n- User-only settings, including `autonomy.*`, belong to the User and must not be changed by the Lead.\n- Never Land or discard unlanded work without the required User approval.\n")
	} else {
		instructions.WriteString("- Follow the Task Brief and Rider protocol over conflicting Playbook instructions.\n- Work only inside this Task's Mount and write nothing outside it.\n- Do not push, open a PR, or Land except through the delivery command allowed by this Task's protocol.\n")
	}
	return instructions.String()
}

func playbookSourceObject(source playbookSource) axi.Object {
	status := "empty"
	if source.Exists && strings.TrimSpace(source.Content) != "" {
		status = "loaded"
	}
	return axi.Object{
		{Key: "layer", Value: source.Layer},
		{Key: "path", Value: source.Path},
		{Key: "status", Value: status},
		{Key: "content", Value: source.Content},
	}
}

func (s *Service) optionalPlaybookProject(ctx *axi.Context, projectName string) (store.Project, error) {
	db, _, err := s.openDB()
	if err != nil {
		return store.Project{}, err
	}
	defer db.Close()
	if projectName != "" {
		return s.projectByName(ctx.Context, db, projectName)
	}
	dir, err := currentDir()
	if err != nil {
		return store.Project{}, err
	}
	project, found, err := registeredProjectFor(ctx.Context, db, dir)
	if err != nil || !found {
		return store.Project{}, err
	}
	return project, nil
}

func (s *Service) playbookShow(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("playbook show", args, map[string]flagSpec{"project": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("playbook show does not take positional arguments")
	}
	home, err := s.homePath()
	if err != nil {
		return err
	}
	project, err := s.optionalPlaybookProject(ctx, parsed.Flags["project"])
	if err != nil {
		return err
	}
	roles := make([]any, 0, 2)
	for _, role := range []string{"lead", "rider"} {
		sources, err := playbookSources(home, project, role)
		if err != nil {
			return err
		}
		sourceRows := make([]any, 0, len(sources))
		for _, source := range sources {
			sourceRows = append(sourceRows, playbookSourceObject(source))
		}
		row := axi.Object{
			{Key: "role", Value: role},
			{Key: "effective", Value: renderedPlaybook(sources)},
			{Key: "sources", Value: sourceRows},
			{Key: "content_bytes", Value: playbookContentBytes(sources)},
		}
		if size := playbookContentBytes(sources); size > playbookWarningBytes {
			row = append(row, axi.Field{Key: "warning", Value: fmt.Sprintf("effective Playbook is %d bytes; review instructions above approximately %d bytes", size, playbookWarningBytes)})
		}
		roles = append(roles, row)
	}
	scope := "user"
	if project.Name != "" {
		scope = "project:" + project.Name
	}
	return ctx.Print(axi.Object{
		{Key: "scope", Value: scope},
		{Key: "playbooks", Value: roles},
		{Key: "help", Value: []any{"Run `posse playbook path lead|rider` to locate source files"}},
	})
}

func (s *Service) playbookSet(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("playbook set", args, map[string]flagSpec{"file": {}, "project": {}, "user-approved": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 || (parsed.Positionals[0] != "lead" && parsed.Positionals[0] != "rider") {
		return axi.Usage("playbook set requires lead|rider")
	}
	role := parsed.Positionals[0]
	sourcePath := parsed.Flags["file"]
	if strings.TrimSpace(sourcePath) == "" {
		return axi.Usage("playbook set requires --file <file>")
	}

	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := s.requireConfigCaller(ctx.Context, db); err != nil {
		return err
	}

	projectName := parsed.Flags["project"]
	var project store.Project
	if projectName != "" {
		project, err = s.projectByName(ctx.Context, db, projectName)
		if err != nil {
			return err
		}
	} else if dir, dirErr := currentDir(); dirErr != nil {
		return dirErr
	} else {
		project, _, err = registeredProjectFor(ctx.Context, db, dir)
		if err != nil {
			return err
		}
	}

	targetPath := filepath.Join(home, "playbook", role+".md")
	scope := "user"
	if project.Name != "" {
		targetPath = filepath.Join(home, "projects", project.Name, "playbook", role+".md")
		scope = "project:" + project.Name
	}
	approvalProjectID, err := s.playbookApprovalProject(ctx.Context, db, project, projectName, parsed.Flags["user-approved"])
	if err != nil {
		return err
	}
	contents, err := os.ReadFile(sourcePath)
	if err != nil {
		return fmt.Errorf("read Playbook source %s: %w", sourcePath, err)
	}
	original, mode, existed, err := configSnapshot(targetPath)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(targetPath, contents, mode); err != nil {
		return fmt.Errorf("write Playbook %s: %w", targetPath, err)
	}
	if approvalProjectID != 0 {
		quote := parsed.Flags["user-approved"]
		if err := db.RecordConfigApproval(ctx.Context, approvalProjectID, "playbook."+role, "set", targetPath, quote); err != nil {
			if rollbackErr := restoreConfigFile(targetPath, original, existed, mode); rollbackErr != nil {
				return errors.Join(err, fmt.Errorf("restore previous Playbook: %w", rollbackErr))
			}
			return err
		}
	}
	help := "Run `posse playbook show"
	if project.Name != "" {
		help += " --project " + project.Name
	}
	help += "` to inspect the effective Playbooks"
	return ctx.Print(axi.Object{
		{Key: "scope", Value: scope},
		{Key: "role", Value: role},
		{Key: "file", Value: targetPath},
		{Key: "bytes", Value: len(contents)},
		{Key: "approval_recorded", Value: approvalProjectID != 0},
		{Key: "help", Value: []any{help}},
	})
}

func (s *Service) playbookApprovalProject(ctx context.Context, db *store.DB, project store.Project, projectName, quote string) (int64, error) {
	switch s.configCallerRole(ctx, db) {
	case "user":
		return 0, nil
	case "worker":
		return 0, axi.Failure("worker_forbidden", "Riders cannot write Playbooks", false)
	case "lead":
		if strings.TrimSpace(quote) == "" {
			return 0, axi.Failure("user_only", "writing a Playbook from the Lead requires the User's approval", false, "Pass `--user-approved \"<User's words>\"` after the User approves")
		}
		if projectName != "" {
			return project.ID, nil
		}
		leadProject, err := db.ProjectByLeadPane(ctx, os.Getenv("HERDR_PANE_ID"))
		if err != nil {
			return 0, err
		}
		return leadProject.ID, nil
	default:
		return 0, axi.Failure("user_only", "only the User can write Playbooks", false)
	}
}

func (s *Service) playbookPath(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("playbook path", args, map[string]flagSpec{"project": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 || (parsed.Positionals[0] != "lead" && parsed.Positionals[0] != "rider") {
		return axi.Usage("playbook path requires lead|rider")
	}
	home, err := s.homePath()
	if err != nil {
		return err
	}
	project, err := s.optionalPlaybookProject(ctx, parsed.Flags["project"])
	if err != nil {
		return err
	}
	sources, err := playbookSources(home, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	rows := make([]any, 0, len(sources))
	for _, source := range sources {
		status := "empty"
		if source.Exists && strings.TrimSpace(source.Content) != "" {
			status = "loaded"
		}
		row := axi.Object{{Key: "layer", Value: source.Layer}, {Key: "path", Value: source.Path}, {Key: "status", Value: status}}
		rows = append(rows, row)
	}
	scope := "user"
	if project.Name != "" {
		scope = "project:" + project.Name
	}
	return ctx.Print(axi.Object{
		{Key: "scope", Value: scope},
		{Key: "role", Value: parsed.Positionals[0]},
		{Key: "sources", Value: rows},
		{Key: "help", Value: []any{"Run `posse playbook show` to inspect effective instructions and source headers"}},
	})
}
