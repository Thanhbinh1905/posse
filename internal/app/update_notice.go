package app

import (
	"context"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) availableUpdate(ctx context.Context, db *store.DB, project *store.Project) (axi.Row, bool) {
	if s.currentVersion() == "dev" {
		return nil, false
	}
	release, ok := s.readCachedRelease()
	if !ok || !newerVersion(s.currentVersion(), release.Tag) {
		return nil, false
	}
	if project != nil && db != nil {
		_ = db.CreateUpdateNoticeOnce(ctx, project.ID, release.Tag, "Posse "+release.Tag+" is available. Run `posse update --check` to read changes. "+updateInstallHelp+".")
	}
	return axi.Row{{Key: "current", Value: s.currentVersion()}, {Key: "latest", Value: release.Tag}}, true
}

func addUpdateHelp(result axi.Object, update axi.Row) axi.Object {
	if update == nil {
		return result
	}
	result = append(result, axi.Field{Key: "update", Value: update})
	for i := range result {
		if result[i].Key == "help" {
			switch help := result[i].Value.(type) {
			case []any:
				result[i].Value = append(help, updateInstallHelp)
			case []axi.Object:
				result[i].Value = append(help, axi.Object{{Key: "check", Value: "update"}, {Key: "action", Value: updateInstallHelp}})
			}
			return result
		}
	}
	result = append(result, axi.Field{Key: "help", Value: []any{updateInstallHelp}})
	return result
}
