package app

import (
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

const leadMessageNoticeKind = "lead_message"

func (s *Service) leadSend(ctx *axi.Context, args []string) error {
	if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
		return axi.Usage("lead send requires a Project name and a non-empty message")
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	source, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	actor := s.configCaller(ctx.Context, db)
	if actor.role != "lead" || actor.projectID != source.ID || cwdInsideSandbox(ctx.Context, db) {
		return axi.Failure("lead_only", "only a Lead can send messages from its own Project", false, "Run from your Project Lead pane in its Project folder")
	}
	target, err := s.projectByName(ctx.Context, db, args[0])
	if err != nil {
		return err
	}
	if target.ID == source.ID {
		return axi.Usage("lead send requires another Project")
	}
	id, err := db.CreateNotice(ctx.Context, store.Notice{
		ProjectID: target.ID,
		Kind:      leadMessageNoticeKind,
		Summary:   fmt.Sprintf("Message from the %s Lead: %s", source.Name, args[1]),
	})
	if err != nil {
		return err
	}
	return ctx.Print(axi.Object{
		{Key: "state", Value: "queued"},
		{Key: "from_project", Value: source.Name},
		{Key: "to_project", Value: target.Name},
		{Key: "notice_id", Value: id},
		{Key: "help", Value: []string{"The Notice stays queued until the target Project's Lead can receive it"}},
	})
}
