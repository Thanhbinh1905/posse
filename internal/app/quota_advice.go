package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/quotaadvice"
	"github.com/thanhbinh1905/posse/internal/store"
)

var readQuotaEvidence = quotaadvice.Read

type profileQuotaAdvice struct {
	Phase    string               `json:"phase"`
	Profiles []string             `json:"profiles"`
	Mapping  string               `json:"mapping"`
	Evidence quotaadvice.Evidence `json:"evidence"`
}

func quotaSelection(ctx context.Context, names []string) profileQuotaAdvice {
	return profileQuotaAdvice{Phase: "selection", Profiles: names, Mapping: "unknown: Profile route and serving account are not bound to native observations", Evidence: readQuotaEvidence(ctx)}
}
func (s *Service) launchQuotaAdvice(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task) {
	names := []string{task.Profile}
	if cfg, err := config.Load(home, project.Name); err == nil {
		seen := map[string]bool{task.Profile: true}
		for current := task.Profile; len(names) < 16; {
			next := cfg.Profiles[current].Fallback
			if next == "" || seen[next] {
				break
			}
			seen[next] = true
			names = append(names, next)
			current = next
		}
	}
	advice := quotaSelection(ctx, names)
	advice.Phase = "launch"
	data, _ := json.Marshal(advice)
	_, _ = db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "quota_advice", Summary: fmt.Sprintf("Launch quota advice for %s: account/route mapping unknown; proceeding with admitted Profile", task.Profile), DataJSON: string(data)})
}
