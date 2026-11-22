package app

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) reviewDispute(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("review dispute", args, map[string]flagSpec{"evidence": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 2 || strings.TrimSpace(parsed.Flags["evidence"]) == "" {
		return axi.Usage("review dispute requires <change> <F-n> --evidence <file>")
	}
	data, err := readDisputeEvidence(parsed.Flags["evidence"])
	if err != nil {
		return err
	}
	seq, err := strconv.Atoi(strings.TrimPrefix(parsed.Positionals[1], "F-"))
	if err != nil || seq < 1 {
		return axi.Usage("finding id must be F-n")
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
	role := s.configCallerRole(ctx.Context, db)
	if role != "worker" && role != "lead" && role != "user" && role != "" {
		return axi.Failure("review_dispute_refused", "only a Worker or Lead may dispute a Greenhouse finding", false)
	}
	change, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	if change.Type != "change" {
		return axi.Failure("review_dispute_refused", "finding disputes require a Change Task", false)
	}
	finding, err := db.ProviderFindingForTask(ctx.Context, change.ID, seq)
	if err != nil {
		return axi.Failure("review_dispute_refused", "Greenhouse finding not found in the latest Verdict", false)
	}
	if finding.State != "open" && finding.State != "disputed" {
		return axi.Failure("review_dispute_refused", "only open or disputed Greenhouse findings may be disputed", false)
	}
	if finding.Severity != "P0" && finding.Severity != "P1" {
		return axi.Failure("review_dispute_refused", "only P0 and P1 Greenhouse findings may be disputed", false)
	}
	targets, err := changeCandidateTargets(ctx.Context, db, project, change)
	if err != nil {
		return err
	}
	head := ""
	for _, target := range targets {
		if target.Repository == finding.Repository {
			head = target.Commit
			break
		}
	}
	if finding.State == "withdrawn" || head == "" || finding.HeadSHA != head {
		return axi.Failure("review_dispute_refused", "Greenhouse finding is not bound to the current Task head", false)
	}
	if !strings.Contains(string(data), head) {
		return axi.Failure("review_dispute_evidence_unpinned", "evidence must name the exact Greenhouse finding head", false)
	}
	digest := sha256.Sum256(data)
	request := store.DecisionRequest{
		ProjectID: project.ID, TaskID: change.ID, Kind: "review_dispute",
		Origin:   fmt.Sprintf("review_dispute:%d:%d:%s", change.ID, seq, head),
		Question: fmt.Sprintf("Withdraw Greenhouse finding F-%d at %s?", seq, head),
		Options:  []string{"withdraw", "keep"}, Evidence: data,
	}
	decision, err := db.RaiseDecision(ctx.Context, request)
	if err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "decision", Value: decision.ID}, {Key: "finding", Value: "F-" + strconv.Itoa(seq)}, {Key: "head", Value: head}, {Key: "evidence_digest", Value: hex.EncodeToString(digest[:])}, {Key: "state", Value: "awaiting_user"}})
}

func readDisputeEvidence(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, axi.Failure("review_dispute_evidence_invalid", err.Error(), false)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 64*1024 {
		return nil, axi.Failure("review_dispute_evidence_too_large", "evidence exceeds the 64 KiB limit", false)
	}
	if len(data) == 0 {
		return nil, axi.Failure("review_dispute_evidence_invalid", "evidence file is empty", false)
	}
	text := string(data)
	if !strings.Contains(text, "Test:") && !strings.Contains(text, "Code:") {
		return nil, axi.Failure("review_dispute_evidence_invalid", "evidence must identify a test or code excerpt", false)
	}
	return data, nil
}
