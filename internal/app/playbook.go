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

const preferenceWarningBytes = 4 * 1024

type preferenceSource struct {
	Layer   string
	Path    string
	Content string
	Exists  bool
	Legacy  bool
}

func preferenceSources(home string, project store.Project, role string) ([]preferenceSource, error) {
	if role != "lead" && role != "rider" {
		return nil, fmt.Errorf("unknown preference role %q", role)
	}
	paths := []preferenceSource{
		{Layer: "User", Path: filepath.Join(home, "playbook", role+".md"), Legacy: true},
		{Layer: "User", Path: filepath.Join(home, "preferences", role+".md")},
	}
	if project.Name != "" {
		projectHome := filepath.Join(home, "projects", project.Name)
		paths = append(paths,
			preferenceSource{Layer: "Project " + project.Name, Path: filepath.Join(projectHome, "playbook", role+".md"), Legacy: true},
			preferenceSource{Layer: "Project " + project.Name, Path: filepath.Join(projectHome, "preferences", role+".md")},
		)
	}
	for index := range paths {
		content, err := os.ReadFile(paths[index].Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s preferences %s: %w", paths[index].Layer, paths[index].Path, err)
		}
		paths[index].Content = string(content)
		paths[index].Exists = true
	}
	return paths, nil
}

// renderedPreferences includes a visible source boundary only for non-empty files.
// The structured CLI output still lists missing and empty source paths.
func renderedPreferences(sources []preferenceSource) string {
	var rendered strings.Builder
	for _, source := range sources {
		content := source.Content
		if strings.TrimSpace(content) == "" {
			continue
		}
		if rendered.Len() > 0 {
			rendered.WriteString("\n\n")
		}
		fmt.Fprintf(&rendered, "### %s preferences (`%s`)", source.Layer, source.Path)
		if source.Legacy {
			rendered.WriteString(" [deprecated legacy path]")
		}
		rendered.WriteString("\n\n")
		rendered.WriteString(content)
		if !strings.HasSuffix(content, "\n") {
			rendered.WriteByte('\n')
		}
	}
	return rendered.String()
}

func preferenceContentBytes(sources []preferenceSource) int {
	total := 0
	for _, source := range sources {
		total += len(source.Content)
	}
	return total
}

func preferenceSizeWarnings(home string, projects []store.Project) []doctorCheck {
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
			sources, err := preferenceSources(home, project, role)
			if err != nil {
				checks = append(checks, doctorCheck{
					Name: "preferences " + scope + " " + role, Status: "warn", Detail: err.Error(),
					Action: "Check the preferences file permissions and run `posse preferences show`",
				})
				continue
			}
			size := preferenceContentBytes(sources)
			if size > preferenceWarningBytes {
				action := "Run `posse preferences show` to inspect the sources"
				if project.Name != "" {
					action = "Run `posse preferences show --project " + project.Name + "` to inspect the sources"
				}
				checks = append(checks, doctorCheck{
					Name: "preferences " + scope + " " + role, Status: "warn",
					Detail: fmt.Sprintf("effective preferences content is %d bytes; review instructions above approximately %d bytes", size, preferenceWarningBytes),
					Action: action,
				})
			}
		}
	}
	return checks
}

func appendPreferenceInstructions(builtIn, role string, sources []preferenceSource) string {
	custom := renderedPreferences(sources)
	if custom == "" {
		return builtIn
	}
	var instructions strings.Builder
	if builtIn = strings.TrimRight(builtIn, "\n"); builtIn != "" {
		instructions.WriteString(builtIn)
		instructions.WriteString("\n\n")
	}
	instructions.WriteString("## User and Project ")
	instructions.WriteString(strings.ToUpper(role[:1]) + role[1:])
	instructions.WriteString(" preferences\n\nThese User-authored preferences may change default workflow advice. Project preferences follow User preferences and may replace conflicting workflow advice. They cannot grant authority or change Posse runtime obligations. For Riders, the Task Brief and Rider protocol take precedence.\n\n")
	instructions.WriteString(custom)
	if role == "rider" {
		instructions.WriteString("\n\n## Non-overridable Posse boundaries\n\n- Follow the Task Brief and Rider protocol over conflicting preferences.\n- Work only inside this Task's Mount and write nothing outside it.\n- Do not push, open a PR, or Land except through the delivery command allowed by this Task's protocol.\n")
	}
	return instructions.String()
}

func preferenceSourceObject(source preferenceSource) axi.Object {
	status := "empty"
	if source.Exists && strings.TrimSpace(source.Content) != "" {
		status = "loaded"
	}
	return axi.Object{
		{Key: "layer", Value: source.Layer},
		{Key: "path", Value: source.Path},
		{Key: "status", Value: status},
		{Key: "content", Value: source.Content},
		{Key: "deprecated", Value: source.Legacy},
	}
}

func (s *Service) optionalPreferenceProject(ctx *axi.Context, projectName string) (store.Project, error) {
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

func (s *Service) preferencesShow(ctx *axi.Context, args []string) error {
	return s.showPreferences(ctx, args, false)
}

func (s *Service) playbookShow(ctx *axi.Context, args []string) error {
	return s.showPreferences(ctx, args, true)
}

func (s *Service) showPreferences(ctx *axi.Context, args []string, deprecated bool) error {
	command := "preferences show"
	if deprecated {
		command = "playbook show"
	}
	parsed, err := parseArgs(command, args, map[string]flagSpec{"project": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage(command + " does not take positional arguments")
	}
	home, err := s.homePath()
	if err != nil {
		return err
	}
	project, err := s.optionalPreferenceProject(ctx, parsed.Flags["project"])
	if err != nil {
		return err
	}
	roles := make([]any, 0, 2)
	warnings := []any{}
	for _, role := range []string{"lead", "rider"} {
		sources, err := preferenceSources(home, project, role)
		if err != nil {
			return err
		}
		sourceRows := make([]any, 0, len(sources))
		for _, source := range sources {
			sourceRows = append(sourceRows, preferenceSourceObject(source))
		}
		bytes := preferenceContentBytes(sources)
		row := axi.Object{
			{Key: "role", Value: role},
			{Key: "effective", Value: renderedPreferences(sources)},
			{Key: "sources", Value: sourceRows},
			{Key: "content_bytes", Value: bytes},
		}
		if bytes > preferenceWarningBytes {
			row = append(row, axi.Field{Key: "warning", Value: fmt.Sprintf("effective preferences are %d bytes; review instructions above approximately %d bytes", bytes, preferenceWarningBytes)})
		}
		if hasLegacyPreferences(sources) {
			warnings = append(warnings, fmt.Sprintf("Legacy playbook paths for %s are still loaded. Use `posse preferences move %s` to move the legacy file when the new destination is absent.", role, role))
		}
		roles = append(roles, row)
	}
	if deprecated {
		warnings = append(warnings, "Deprecated command: use `posse preferences show`. New files belong under `<POSSE_HOME>/preferences/{lead,rider}.md` and `<POSSE_HOME>/projects/<project>/preferences/{lead,rider}.md`. Legacy playbook paths remain readable and are not moved automatically.")
	}
	rolesKey := "preferences"
	if deprecated {
		rolesKey = "playbooks"
	}
	scope := preferenceScope(project)
	return ctx.Print(axi.Object{
		{Key: "scope", Value: scope},
		{Key: rolesKey, Value: roles},
		{Key: "warnings", Value: warnings},
		{Key: "help", Value: []any{"Run `posse preferences path lead|rider` to locate source files"}},
	})
}

func (s *Service) preferencesSet(ctx *axi.Context, args []string) error {
	return s.setPreferences(ctx, args, false)
}

func (s *Service) playbookSet(ctx *axi.Context, args []string) error {
	return s.setPreferences(ctx, args, true)
}

func (s *Service) setPreferences(ctx *axi.Context, args []string, deprecated bool) error {
	command := "preferences set"
	if deprecated {
		command = "playbook set"
	}
	parsed, err := parseArgs(command, args, map[string]flagSpec{"file": {}, "project": {}, "user-approved": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 || (parsed.Positionals[0] != "lead" && parsed.Positionals[0] != "rider") {
		return axi.Usage(command + " requires lead|rider")
	}
	role := parsed.Positionals[0]
	sourcePath := parsed.Flags["file"]
	if strings.TrimSpace(sourcePath) == "" {
		return axi.Usage(command + " requires --file <file>")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := s.requireConfigCaller(ctx.Context, db); err != nil {
		return err
	}
	project, err := s.preferenceTargetProject(ctx, db, parsed.Flags["project"])
	if err != nil {
		return err
	}
	targetPath, legacyPath := preferenceTargetPaths(home, project, role)
	approvalProjectID, err := s.preferenceApprovalProject(ctx.Context, db, project, parsed.Flags["project"], parsed.Flags["user-approved"])
	if err != nil {
		return err
	}
	contents, err := os.ReadFile(sourcePath)
	if err != nil {
		return fmt.Errorf("read preferences source %s: %w", sourcePath, err)
	}
	original, mode, existed, err := configSnapshot(targetPath)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(targetPath, contents, mode); err != nil {
		return fmt.Errorf("write preferences %s: %w", targetPath, err)
	}
	if approvalProjectID != 0 {
		quote := parsed.Flags["user-approved"]
		if err := db.RecordConfigApproval(ctx.Context, approvalProjectID, "preferences."+role, "set", targetPath, quote); err != nil {
			if rollbackErr := restoreConfigFile(targetPath, original, existed, mode); rollbackErr != nil {
				return errors.Join(err, fmt.Errorf("restore previous preferences: %w", rollbackErr))
			}
			return err
		}
	}
	warnings := []any{}
	if fileExists(legacyPath) {
		warnings = append(warnings, fmt.Sprintf("Legacy preferences remain loaded from %s. Inspect both files with `posse preferences show`; because the canonical destination now exists, combine their contents manually and remove the legacy file only after confirming the merged preferences.", legacyPath))
	}
	if deprecated {
		warnings = append(warnings, fmt.Sprintf("Deprecated command: use `posse preferences set %s --file <file>`. This wrote the new path %s; it did not move legacy content at %s.", role, targetPath, legacyPath))
	}
	help := "Run `posse preferences show"
	if project.Name != "" {
		help += " --project " + project.Name
	}
	help += "` to inspect the effective preferences"
	return ctx.Print(axi.Object{
		{Key: "scope", Value: preferenceScope(project)},
		{Key: "role", Value: role},
		{Key: "file", Value: targetPath},
		{Key: "bytes", Value: len(contents)},
		{Key: "approval_recorded", Value: approvalProjectID != 0},
		{Key: "warnings", Value: warnings},
		{Key: "help", Value: []any{help}},
	})
}

func (s *Service) preferenceTargetProject(ctx *axi.Context, db *store.DB, projectName string) (store.Project, error) {
	if projectName != "" {
		return s.projectByName(ctx.Context, db, projectName)
	}
	dir, err := currentDir()
	if err != nil {
		return store.Project{}, err
	}
	project, _, err := registeredProjectFor(ctx.Context, db, dir)
	return project, err
}

func preferenceTargetPaths(home string, project store.Project, role string) (target, legacy string) {
	root := home
	if project.Name != "" {
		root = filepath.Join(home, "projects", project.Name)
	}
	return filepath.Join(root, "preferences", role+".md"), filepath.Join(root, "playbook", role+".md")
}

func preferenceScope(project store.Project) string {
	if project.Name == "" {
		return "user"
	}
	return "project:" + project.Name
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func hasLegacyPreferences(sources []preferenceSource) bool {
	for _, source := range sources {
		if source.Legacy && source.Exists {
			return true
		}
	}
	return false
}

func (s *Service) preferenceApprovalProject(ctx context.Context, db *store.DB, project store.Project, projectName, quote string) (int64, error) {
	switch s.configCallerRole(ctx, db) {
	case "user":
		return 0, nil
	case "worker":
		return 0, axi.Failure("worker_forbidden", "Riders cannot write preferences", false)
	case "lead":
		if strings.TrimSpace(quote) == "" {
			return 0, axi.Failure("user_only", "writing preferences from the Lead requires the User's approval", false, "Pass `--user-approved \"<User's words>\"` after the User approves")
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
		return 0, axi.Failure("user_only", "only the User can write preferences", false)
	}
}

func (s *Service) preferencesPath(ctx *axi.Context, args []string) error {
	return s.showPreferencePaths(ctx, args, false)
}

func (s *Service) playbookPath(ctx *axi.Context, args []string) error {
	return s.showPreferencePaths(ctx, args, true)
}

func (s *Service) showPreferencePaths(ctx *axi.Context, args []string, deprecated bool) error {
	command := "preferences path"
	if deprecated {
		command = "playbook path"
	}
	parsed, err := parseArgs(command, args, map[string]flagSpec{"project": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 || (parsed.Positionals[0] != "lead" && parsed.Positionals[0] != "rider") {
		return axi.Usage(command + " requires lead|rider")
	}
	home, err := s.homePath()
	if err != nil {
		return err
	}
	project, err := s.optionalPreferenceProject(ctx, parsed.Flags["project"])
	if err != nil {
		return err
	}
	sources, err := preferenceSources(home, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	rows := make([]any, 0, len(sources))
	for _, source := range sources {
		status := "empty"
		if source.Exists && strings.TrimSpace(source.Content) != "" {
			status = "loaded"
		}
		rows = append(rows, axi.Object{
			{Key: "layer", Value: source.Layer},
			{Key: "path", Value: source.Path},
			{Key: "status", Value: status},
			{Key: "deprecated", Value: source.Legacy},
		})
	}
	warnings := []any{}
	if deprecated {
		warnings = append(warnings, fmt.Sprintf("Deprecated command: use `posse preferences path %s`. New files belong under `<POSSE_HOME>/preferences/` and `<POSSE_HOME>/projects/<project>/preferences/`; legacy files remain readable and are not moved automatically.", parsed.Positionals[0]))
	}
	return ctx.Print(axi.Object{
		{Key: "scope", Value: preferenceScope(project)},
		{Key: "role", Value: parsed.Positionals[0]},
		{Key: "sources", Value: rows},
		{Key: "warnings", Value: warnings},
		{Key: "help", Value: []any{"Run `posse preferences show` to inspect effective instructions and source headers"}},
	})
}

func (s *Service) preferencesMove(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("preferences move", args, map[string]flagSpec{"project": {}, "user-approved": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 || (parsed.Positionals[0] != "lead" && parsed.Positionals[0] != "rider") {
		return axi.Usage("preferences move requires lead|rider")
	}
	role := parsed.Positionals[0]
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := s.requireConfigCaller(ctx.Context, db); err != nil {
		return err
	}
	project, err := s.preferenceTargetProject(ctx, db, parsed.Flags["project"])
	if err != nil {
		return err
	}
	targetPath, legacyPath := preferenceTargetPaths(home, project, role)
	approvalProjectID, err := s.preferenceApprovalProject(ctx.Context, db, project, parsed.Flags["project"], parsed.Flags["user-approved"])
	if err != nil {
		return err
	}
	legacyInfo, err := os.Lstat(legacyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return axi.Failure("preference_legacy_missing", "no legacy preferences file exists at "+legacyPath, false, "Run `posse preferences path "+role+"` to inspect the available files")
		}
		return err
	}
	if !legacyInfo.Mode().IsRegular() {
		return axi.Failure("preference_legacy_not_regular", "legacy preferences must be a regular file to move: "+legacyPath, false, "Copy its reviewed contents to the canonical preferences path with `posse preferences set`")
	}
	if _, err := os.Lstat(targetPath); err == nil {
		return axi.Failure("preference_destination_exists", "refusing to overwrite preferences at "+targetPath, false, "Review both files, combine their contents with `posse preferences set`, then remove the legacy file only after confirming the merged preferences")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return err
	}
	if err := os.Link(legacyPath, targetPath); err != nil {
		return fmt.Errorf("move legacy preferences without overwriting %s: %w", targetPath, err)
	}
	if err := os.Remove(legacyPath); err != nil {
		if rollbackErr := os.Remove(targetPath); rollbackErr != nil {
			return errors.Join(fmt.Errorf("remove legacy preferences after copying to %s: %w", targetPath, err), fmt.Errorf("remove duplicate destination: %w", rollbackErr))
		}
		return fmt.Errorf("remove legacy preferences after copying to %s: %w", targetPath, err)
	}
	if approvalProjectID != 0 {
		if err := db.RecordConfigApproval(ctx.Context, approvalProjectID, "preferences."+role, "move", targetPath, parsed.Flags["user-approved"]); err != nil {
			rollbackErr := os.Link(targetPath, legacyPath)
			if rollbackErr == nil {
				rollbackErr = os.Remove(targetPath)
			}
			if rollbackErr != nil {
				return errors.Join(err, fmt.Errorf("restore legacy preferences at %s: %w", legacyPath, rollbackErr))
			}
			return err
		}
	}
	return ctx.Print(axi.Object{
		{Key: "scope", Value: preferenceScope(project)},
		{Key: "role", Value: role},
		{Key: "from", Value: legacyPath},
		{Key: "to", Value: targetPath},
		{Key: "approval_recorded", Value: approvalProjectID != 0},
		{Key: "help", Value: []any{"Run `posse preferences show" + projectOption(project) + "` to inspect the result"}},
	})
}

func projectOption(project store.Project) string {
	if project.Name == "" {
		return ""
	}
	return " --project " + project.Name
}
