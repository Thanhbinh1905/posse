package taskplan

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const ScopeAmendmentSource = "task amend --user-approved"

type ScopeAmendment struct {
	Revision   int    `json:"revision"`
	Repository string `json:"repository"`
	Scope      string `json:"scope"`
	Quote      string `json:"quote"`
	Actor      string `json:"actor"`
	Source     string `json:"source"`
	PRURL      string `json:"pr_url"`
	OldHead    string `json:"old_head"`
	NewHead    string `json:"new_head"`
}

func (p *Plan) ScopeRevision() int {
	if p == nil || len(p.ScopeAmendments) == 0 {
		return 0
	}
	return p.ScopeAmendments[len(p.ScopeAmendments)-1].Revision
}

func (p *Plan) ScopePRURL() string {
	if p == nil || len(p.ScopeAmendments) == 0 {
		return ""
	}
	return p.ScopeAmendments[len(p.ScopeAmendments)-1].PRURL
}

func (p Plan) AmendScope(amendment ScopeAmendment) (Plan, error) {
	p.PreviousSHA256 = p.SHA256()
	p.AdmittedSHA256 = ""
	p.legacyJSON = ""
	p.Revision++
	amendment.Revision = p.Revision
	p.ScopeAmendments = append(append([]ScopeAmendment(nil), p.ScopeAmendments...), amendment)
	return p, p.Validate()
}

func (p *Plan) ScopeReviewIntent() string {
	if p == nil || len(p.ScopeAmendments) == 0 {
		return ""
	}
	var intent strings.Builder
	intent.WriteString("\n\nUser-approved scope additions for this repository (approval quotes are provenance):")
	for _, amendment := range p.ScopeAmendments {
		fmt.Fprintf(&intent, "\n- Scope: %s\n  User quote: %s", strconv.Quote(amendment.Scope), strconv.Quote(amendment.Quote))
	}
	return intent.String()
}

func (p Plan) validateScopeAmendments() error {
	previous := 1
	for _, amendment := range p.ScopeAmendments {
		if p.Type != "change" || p.Execution.Scope != "repo" || p.Execution.Mode != "sandbox" || p.Delivery != "pr" || p.Review != "full-pipeline" || p.FrozenReviewProvider() != "greenhouse" ||
			amendment.Revision <= previous || amendment.Revision > p.Revision || amendment.Repository == "" || amendment.PRURL == "" ||
			amendment.Actor != "lead" || amendment.Source != ScopeAmendmentSource || !validScopeText(amendment.Scope) || !validScopeText(amendment.Quote) ||
			!validScopeHead(amendment.OldHead) || !validScopeHead(amendment.NewHead) || amendment.OldHead == amendment.NewHead {
			return fmt.Errorf("invalid Task scope amendment")
		}
		if original := p.ScopeAmendments[0]; amendment.Repository != original.Repository || amendment.PRURL != original.PRURL {
			return fmt.Errorf("scope amendments must keep the original repository and PR or MR")
		}
		previous = amendment.Revision
	}
	if len(p.ScopeReviewIntent()) > 4096 {
		return fmt.Errorf("approved scope additions exceed 4096 bytes of review text; shorten the scope description without changing the User quote")
	}
	return nil
}

func validScopeText(text string) bool {
	return strings.TrimSpace(text) != "" && utf8.ValidString(text) && strings.IndexFunc(text, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\t'
	}) < 0
}

func validScopeHead(head string) bool {
	if len(head) != 40 && len(head) != 64 {
		return false
	}
	_, err := hex.DecodeString(head)
	return err == nil
}
