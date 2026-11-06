package quotaadvice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/thanhbinh1905/posse/internal/execgroup"
)

type Window struct {
	Kind      string   `json:"kind"`
	Scope     string   `json:"scope"`
	Remaining *float64 `json:"percent_remaining,omitempty"`
	Reset     string   `json:"resets_at,omitempty"`
}
type Observation struct {
	Provider         string   `json:"provider"`
	Status           string   `json:"status"`
	RefreshedAt      string   `json:"refreshed_at,omitempty"`
	IdentityVerified bool     `json:"identity_verified"`
	Windows          []Window `json:"windows,omitempty"`
}
type Evidence struct {
	Status       string        `json:"status"`
	Codex        string        `json:"codex"`
	ModelScope   string        `json:"model_scope"`
	Reason       string        `json:"reason"`
	GeneratedAt  string        `json:"generated_at,omitempty"`
	Observations []Observation `json:"observations,omitempty"`
}

func Unknown(reason string) Evidence {
	return Evidence{Status: "unknown", Reason: reason, Codex: "unknown: excluded because RPC fallback does not enforce no credential refresh", ModelScope: "unbound: windows are provider observations, not Profile model allowance"}
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 256*1024 {
		return 0, errors.New("quota output limit")
	}
	return b.Buffer.Write(p)
}

func Read(ctx context.Context) Evidence {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := execgroup.CommandContext(ctx, "quota-axi", "--json", "--full", "--no-credential-refresh", "--provider", "claude")
	var output boundedBuffer
	cmd.Stdout = &output
	cmd.WaitDelay = 100 * time.Millisecond
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Unknown("collection_timeout")
		}
		return Unknown("collection_unavailable")
	}
	return Parse(output.Bytes(), time.Now())
}

func Parse(data []byte, now time.Time) Evidence {
	var report struct {
		Schema    int    `json:"schemaVersion"`
		Generated string `json:"generatedAt"`
		Providers []struct {
			Provider string `json:"provider"`
			Account  struct {
				Identity string `json:"identityStatus"`
			} `json:"account"`
			State struct {
				Status    string `json:"status"`
				Stale     bool   `json:"stale"`
				Refreshed string `json:"refreshedAt"`
			} `json:"state"`
			Windows []struct {
				ID        string   `json:"id"`
				Kind      string   `json:"kind"`
				Remaining *float64 `json:"percentRemaining"`
				Reset     string   `json:"resetsAt"`
			} `json:"windows"`
		} `json:"providers"`
	}
	if len(data) > 256*1024 || json.Unmarshal(data, &report) != nil {
		return Unknown("invalid_output")
	}
	if report.Schema != 5 {
		return Unknown("unsupported_schema")
	}
	generated, err := time.Parse(time.RFC3339, report.Generated)
	if err != nil || generated.After(now.Add(time.Minute)) || now.Sub(generated) > 5*time.Minute {
		return Unknown("stale_report")
	}
	result := Unknown("profile_account_and_route_unverified")
	result.GeneratedAt = generated.UTC().Format(time.RFC3339)
	for _, row := range report.Providers {
		if len(result.Observations) >= 32 {
			break
		}
		if row.Provider != "claude" {
			continue
		}
		observation := Observation{Provider: row.Provider, Status: "unavailable", IdentityVerified: row.Account.Identity == "verified"}
		refreshed, e := time.Parse(time.RFC3339, row.State.Refreshed)
		if e == nil {
			observation.RefreshedAt = refreshed.UTC().Format(time.RFC3339)
		}
		if oneOf(row.State.Status, "auth_required", "rate_limited", "error") {
			observation.Status = row.State.Status
		}
		if row.State.Status == "fresh" && !row.State.Stale && e == nil && now.Sub(refreshed) <= 5*time.Minute && !refreshed.After(now.Add(time.Minute)) {
			observation.Status = "fresh"
			for _, w := range row.Windows {
				if len(observation.Windows) >= 16 {
					break
				}
				if !oneOf(w.Kind, "session", "weekly", "monthly", "model", "credits", "unknown") {
					continue
				}
				window := Window{Kind: w.Kind, Scope: "unknown"}
				switch w.ID {
				case "five_hour", "seven_day":
					window.Scope = "all_models"
				case "seven_day_opus", "seven_day_sonnet":
					window.Scope = w.ID
				}
				if w.Remaining != nil && *w.Remaining >= 0 && *w.Remaining <= 100 {
					window.Remaining = w.Remaining
				}
				if reset, err := time.Parse(time.RFC3339, w.Reset); err == nil && reset.After(now) {
					window.Reset = reset.UTC().Format(time.RFC3339)
				}
				observation.Windows = append(observation.Windows, window)
			}
		} else if row.State.Stale || row.State.Status == "stale" || row.State.Status == "fresh" {
			observation.Status = "stale"
		}
		result.Observations = append(result.Observations, observation)
	}
	return result
}
func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}
