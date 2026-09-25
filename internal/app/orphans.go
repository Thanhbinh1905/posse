package app

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

var taskPaneLabel = regexp.MustCompile(`^posse:([a-z0-9_-]+):t([1-9][0-9]*)$`)

func (s *Service) reconcileTaskPanes(ctx context.Context, db *store.DB, project store.Project, snapshot herdr.Snapshot) error {
	for _, pane := range snapshot.Panes {
		match := taskPaneLabel.FindStringSubmatch(pane.Label)
		if len(match) != 3 || match[1] != project.Name {
			continue
		}
		sequence, err := strconv.Atoi(match[2])
		if err != nil {
			continue
		}
		task, err := db.Task(ctx, project.ID, taskIDString(sequence))
		if store.IsNotFound(err) {
			if err := createNoticeOnce(ctx, db, store.Notice{ProjectID: project.ID, Kind: "orphan_pane", Summary: "Unknown Task pane needs inspection", DataJSON: marshalJSON(map[string]any{"pane_label": pane.Label})}); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if task.State != store.StateTornDown {
			continue
		}
		if _, err := s.closeTaskPanes(ctx, project, task); err != nil {
			if incompleteErr := s.recordUnsaddleIncomplete(ctx, db, project, task, err); incompleteErr != nil {
				return incompleteErr
			}
		}
	}
	return nil
}

func createNoticeOnce(ctx context.Context, db *store.DB, notice store.Notice) error {
	notices, err := db.Notices(ctx, notice.ProjectID, false)
	if err != nil {
		return err
	}
	for _, existing := range notices {
		if existing.Kind == notice.Kind && existing.TaskID == notice.TaskID && existing.Summary == notice.Summary {
			return nil
		}
	}
	_, err = db.CreateNotice(ctx, notice)
	return err
}

func (s *Service) sweep(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("sweep", args, map[string]flagSpec{"yes": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("sweep does not take positional arguments")
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if err := s.requireLead(ctx.Context, db, project); err != nil {
		return err
	}
	if _, err := s.prepareProject(ctx.Context, db, project); err != nil {
		return err
	}
	snapshot, err := s.snapshot(ctx.Context)
	if err != nil {
		return err
	}
	mounts, err := db.Mounts(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	unknown := make(map[string][]herdr.Pane)
	for _, pane := range snapshot.Panes {
		match := taskPaneLabel.FindStringSubmatch(pane.Label)
		if len(match) != 3 || match[1] != project.Name {
			continue
		}
		sequence, err := strconv.Atoi(match[2])
		if err != nil {
			continue
		}
		if _, err := db.Task(ctx.Context, project.ID, taskIDString(sequence)); err == nil {
			continue
		} else if !store.IsNotFound(err) {
			return err
		}
		if !ownsUnknownPane(snapshot, pane, project.Name, sequence, mounts) {
			continue
		}
		unknown[pane.Label] = append(unknown[pane.Label], pane)
	}
	labels := make([]string, 0, len(unknown))
	for label := range unknown {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	rows := make([]any, 0)
	closed := []string{}
	for _, label := range labels {
		paneIDs := make([]string, 0, len(unknown[label]))
		for _, pane := range unknown[label] {
			paneIDs = append(paneIDs, pane.PaneID)
		}
		sort.Strings(paneIDs)
		rows = append(rows, map[string]any{"label": label, "panes": paneIDs})
		if !parsed.Bool("yes") {
			continue
		}
		for _, paneID := range paneIDs {
			if _, err := s.herdrCall(ctx.Context, "pane.close", map[string]any{"pane_id": paneID}); err != nil && !missingPaneError(err) {
				return err
			}
			closed = append(closed, paneID)
		}
	}
	if parsed.Bool("yes") {
		verified, err := s.snapshot(ctx.Context)
		if err != nil {
			return err
		}
		closedSet := make(map[string]bool, len(closed))
		for _, paneID := range closed {
			closedSet[paneID] = true
		}
		for _, pane := range verified.Panes {
			if closedSet[pane.PaneID] {
				return axi.Failure("sweep_incomplete", fmt.Sprintf("orphan pane %s remains open", pane.PaneID), true)
			}
			if _, selected := unknown[pane.Label]; selected {
				return axi.Failure("sweep_incomplete", fmt.Sprintf("orphan label %s remains", pane.Label), true)
			}
		}
	}
	return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "orphans", Value: rows}, {Key: "closed_panes", Value: closed}, {Key: "dry_run", Value: !parsed.Bool("yes")}, {Key: "help", Value: []any{"Run `posse sweep --yes` to close these orphan panes"}}})
}

func ownsUnknownPane(snapshot herdr.Snapshot, pane herdr.Pane, project string, sequence int, mounts []store.Mount) bool {
	if pane.Agent != "" {
		for _, agent := range snapshot.Agents {
			if agent.PaneID == pane.PaneID {
				return agentNameMatchesTask(agent.Name, project, sequence)
			}
		}
		return false
	}
	for _, mount := range mounts {
		if pathInside(pane.CWD, mount.Path) {
			return true
		}
	}
	return false
}
