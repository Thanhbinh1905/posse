package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/atomicfile"
	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

const handoffMaxBytes = 16 * 1024

func handoffPath(home, project string) string {
	return filepath.Join(home, "projects", project, "handoff.md")
}

func handoffArchiveDir(home, project string) string {
	return filepath.Join(home, "projects", project, "handoff-archive")
}

func (s *Service) handoffWrite(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("handoff write", args, map[string]flagSpec{"file": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 || parsed.Flags["file"] == "" {
		return axi.Usage("handoff write requires --file <file.md>")
	}
	body, err := os.ReadFile(parsed.Flags["file"])
	if err != nil {
		return fmt.Errorf("read handoff source %s: %w", parsed.Flags["file"], err)
	}
	if strings.TrimSpace(string(body)) == "" {
		return axi.Usage("handoff write requires a non-empty file")
	}
	if len(body) > handoffMaxBytes {
		return axi.Failure("handoff_too_large", fmt.Sprintf("handoff note is %d bytes; the limit is %d bytes", len(body), handoffMaxBytes), false, "Curate the note and retry `posse handoff write --file <file.md>`")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	pending := handoffPath(home, project.Name)
	if _, err := os.Lstat(pending); err == nil {
		return axi.Failure("handoff_pending", "a handoff note is already waiting for the next Lead", false, "Let the next Lead load it before writing another note")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect pending handoff %s: %w", pending, err)
	}
	lead, err := db.ProjectLeadAgent(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	generation, err := nextHandoffGeneration(home, project.Name)
	if err != nil {
		return err
	}
	session := lead.Session
	if session == "" {
		session = "unknown"
	}
	if lead.Kind == "" {
		lead.Kind = "unknown"
	}
	contents := fmt.Sprintf("<!-- posse-handoff\nwritten_at: %s\nharness: %q\nsession: %q\ngeneration: %d\n-->\n\n%s", time.Now().UTC().Format(time.RFC3339Nano), lead.Kind, session, generation, string(body))
	if len(contents) > handoffMaxBytes {
		return axi.Failure("handoff_too_large", fmt.Sprintf("handoff note with provenance is %d bytes; the limit is %d bytes", len(contents), handoffMaxBytes), false, "Curate the note and retry `posse handoff write --file <file.md>`")
	}
	if err := atomicfile.Write(pending, []byte(contents), 0o600); err != nil {
		return fmt.Errorf("write pending handoff %s: %w", pending, err)
	}
	return ctx.Print(axi.Object{
		{Key: "state", Value: "written"},
		{Key: "project", Value: project.Name},
		{Key: "generation", Value: generation},
		{Key: "help", Value: []any{"The next Lead start loads and archives this note once"}},
	})
}

func nextHandoffGeneration(home, project string) (int, error) {
	entries, err := os.ReadDir(handoffArchiveDir(home, project))
	if errors.Is(err, os.ErrNotExist) {
		return 1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read handoff archive: %w", err)
	}
	generation := 0
	for _, entry := range entries {
		name := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "handoff-"), ".md")
		value, err := strconv.Atoi(name)
		if err == nil && value > generation {
			generation = value
		}
	}
	return generation + 1, nil
}

func loadHandoff(home string, project store.Project) (string, string) {
	path := handoffPath(home, project.Name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ""
	}
	if err != nil {
		return "", handoffLoadWarning(path, err.Error())
	}
	if !info.Mode().IsRegular() || info.Size() > handoffMaxBytes {
		return "", handoffLoadWarning(path, "the note is not a regular file or exceeds the size limit")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", handoffLoadWarning(path, err.Error())
	}
	body, _, valid := parseHandoff(contents)
	if !valid {
		return "", handoffLoadWarning(path, "the note header is malformed or the body is empty")
	}
	return string(body), ""
}

func (s *Service) archiveStartedLeadHandoff(ctx context.Context, home string, project store.Project) error {
	if err := archiveHandoff(home, project); err != nil {
		message := handoffLoadWarning(handoffPath(home, project.Name), "the Lead started but archiving failed: "+err.Error())
		if _, notifyErr := s.herdrCall(ctx, "notification.show", map[string]any{"title": "Posse handoff needs attention", "body": message}); notifyErr != nil {
			return fmt.Errorf("archive Lead handoff: %w; show notification: %v", err, notifyErr)
		}
	}
	return nil
}

func archiveHandoff(home string, project store.Project) error {
	path := handoffPath(home, project.Name)
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read pending handoff for archive: %w", err)
	}
	_, generation, valid := parseHandoff(contents)
	if !valid {
		return fmt.Errorf("pending handoff changed before archive")
	}
	archiveDir := handoffArchiveDir(home, project.Name)
	if err := os.MkdirAll(archiveDir, 0o700); err != nil {
		return fmt.Errorf("create handoff archive: %w", err)
	}
	archivePath := filepath.Join(archiveDir, fmt.Sprintf("handoff-%04d.md", generation))
	if err := os.Link(path, archivePath); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("archive handoff: %w", err)
		}
		archived, readErr := os.ReadFile(archivePath)
		if readErr != nil || string(archived) != string(contents) {
			return fmt.Errorf("handoff archive generation %d already contains a different note", generation)
		}
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove pending handoff after archiving: %w", err)
	}
	return nil
}

func parseHandoff(contents []byte) (body []byte, generation int, valid bool) {
	if len(contents) > handoffMaxBytes {
		return nil, 0, false
	}
	const opening = "<!-- posse-handoff\n"
	if !strings.HasPrefix(string(contents), opening) {
		return nil, 0, false
	}
	header, rest, found := strings.Cut(string(contents[len(opening):]), "\n-->\n\n")
	if !found {
		return nil, 0, false
	}
	fields := map[string]string{}
	for _, line := range strings.Split(header, "\n") {
		key, value, found := strings.Cut(line, ": ")
		if !found || key == "" || value == "" || fields[key] != "" {
			return nil, 0, false
		}
		fields[key] = value
	}
	for _, key := range []string{"written_at", "harness", "session", "generation"} {
		if fields[key] == "" {
			return nil, 0, false
		}
	}
	if len(fields) != 4 {
		return nil, 0, false
	}
	if _, err := time.Parse(time.RFC3339Nano, fields["written_at"]); err != nil {
		return nil, 0, false
	}
	for _, key := range []string{"harness", "session"} {
		value, err := strconv.Unquote(fields[key])
		if err != nil || strings.TrimSpace(value) == "" {
			return nil, 0, false
		}
	}
	generation, err := strconv.Atoi(fields["generation"])
	if err != nil || generation < 1 || strings.TrimSpace(rest) == "" {
		return nil, 0, false
	}
	return []byte(rest), generation, true
}

func handoffLoadWarning(path, reason string) string {
	return fmt.Sprintf("The pending handoff at %s was not consumed: %s. Inspect or repair the file, then restart the Lead.", path, reason)
}
