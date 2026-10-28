package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

var (
	ErrReviewFindingsRequired    = errors.New("review round findings are required")
	ErrReviewFindingsUnexpected  = errors.New("findings are only valid for a Review Round")
	ErrReviewFindingsInvalid     = errors.New("review findings are invalid")
	ErrReviewVerdictInconsistent = errors.New("review verdict contradicts the Finding Ledger")
)

var findingIDPattern = regexp.MustCompile(`^[Ff]-(\d+)$`)

// ReviewFindingsInput is the anchored, scope-classified representation of a
// Review Round findings file. Repository content is checked by the app before
// this data reaches the Signal transaction.
type ReviewFindingsInput struct {
	BlockAt     string
	Patterns    []ReviewFindingPatternInput
	Resolutions []ReviewFindingResolutionInput
}

type ReviewFindingPatternInput struct {
	Key         string
	Title       string
	Rule        string
	RootCause   string
	FixGuidance string
	Severity    string
	Origin      string
	Instances   []ReviewFindingInput
}

type ReviewFindingInput struct {
	Repository  string
	Path        string
	Symbol      string
	Excerpt     string
	Description string
	Line        int
	InScope     bool
	Matches     string
	Regresses   string
}

type ReviewFindingResolutionInput struct {
	Finding string
	Status  string
	Note    string
}

type reviewFindingLedgerRow struct {
	ID          int64
	Seq         int
	Fingerprint string
	State       string
	Severity    string
	Origin      string
	InScope     bool
}

type reviewFindingsResult struct {
	Round            int
	DerivedVerdict   string
	NewPatterns      int
	NewFindings      int
	SuppressedRepeat int
	OutOfScope       int
	BySeverity       map[string]int
	Regressed        []string
	// Reclassified names findings the Reviewer supplied as introduced that
	// Posse recorded pre_existing because they lie outside the round's scope.
	Reclassified []string
}

// ReviewRoundForTask returns the immutable round bindings for a Review Task.
func (db *DB) ReviewRoundForTask(ctx context.Context, reviewTaskID int64) ([]ReviewRound, error) {
	rows, err := db.QueryContext(ctx, `SELECT ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,COALESCE(verdict,''),created_at FROM review_rounds WHERE review_task_id=? ORDER BY repository`, reviewTaskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rounds []ReviewRound
	for rows.Next() {
		var round ReviewRound
		if err := rows.Scan(&round.ChangeTaskID, &round.Round, &round.ReviewTaskID, &round.Repository, &round.ScopeKind, &round.ScopeBaseSHA, &round.HeadSHA, &round.TargetCommitSHA, &round.BaselineTree, &round.DeltaSHA256, &round.ConflictedPaths, &round.Status, &round.Verdict, &round.CreatedAt); err != nil {
			return nil, err
		}
		rounds = append(rounds, round)
	}
	return rounds, rows.Err()
}

func normalizeReviewPatternKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	separator := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if separator && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			builder.WriteRune(r)
			separator = false
		} else {
			separator = true
		}
	}
	return builder.String()
}

func normalizeFindingExcerpt(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func findingFingerprint(key, repository, path, symbol, excerpt string) string {
	value := key + "\x00" + repository + "\x00" + path + "\x00" + symbol + "\x00" + normalizeFindingExcerpt(excerpt)
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func findingSequence(value string) (int, bool) {
	matches := findingIDPattern.FindStringSubmatch(strings.TrimSpace(value))
	if len(matches) != 2 {
		return 0, false
	}
	var sequence int
	if _, err := fmt.Sscanf(matches[1], "%d", &sequence); err != nil || sequence < 1 {
		return 0, false
	}
	return sequence, true
}

func validReviewFindingInput(input ReviewFindingsInput) error {
	if !oneOf(input.BlockAt, "P0", "P1", "P2") {
		return fmt.Errorf("%w: invalid frozen blocking threshold", ErrReviewFindingsInvalid)
	}
	for _, pattern := range input.Patterns {
		if strings.TrimSpace(pattern.Title) == "" || strings.TrimSpace(pattern.Rule) == "" || strings.TrimSpace(pattern.RootCause) == "" || strings.TrimSpace(pattern.FixGuidance) == "" || len(pattern.Instances) == 0 {
			return fmt.Errorf("%w: each pattern requires a title, rule, root cause, fix guidance and at least one instance", ErrReviewFindingsInvalid)
		}
		if pattern.Origin != "introduced" && pattern.Origin != "pre_existing" {
			return fmt.Errorf("%w: invalid finding origin %q", ErrReviewFindingsInvalid, pattern.Origin)
		}
		for _, finding := range pattern.Instances {
			if strings.TrimSpace(finding.Repository) == "" || strings.TrimSpace(finding.Path) == "" || strings.TrimSpace(finding.Excerpt) == "" || finding.Line < 1 {
				return fmt.Errorf("%w: each instance requires a repository, path, anchored excerpt and positive line", ErrReviewFindingsInvalid)
			}
		}
	}
	for _, resolution := range input.Resolutions {
		if _, ok := findingSequence(resolution.Finding); !ok || (resolution.Status != "resolved" && resolution.Status != "unresolved") {
			return fmt.Errorf("%w: invalid resolution for %q", ErrReviewFindingsInvalid, resolution.Finding)
		}
	}
	return nil
}

func recordReviewFindingsTx(ctx context.Context, tx *writeTx, changeTaskID, reviewTaskID int64, input ReviewFindingsInput) (reviewFindingsResult, error) {
	result := reviewFindingsResult{BySeverity: map[string]int{}}
	if err := validReviewFindingInput(input); err != nil {
		return result, err
	}
	var round int
	if err := tx.QueryRowContext(ctx, `SELECT round FROM review_rounds WHERE ship_task_id=? AND review_task_id=? LIMIT 1`, changeTaskID, reviewTaskID).Scan(&round); err != nil {
		return result, err
	}
	result.Round = round

	ledgerRows, err := tx.QueryContext(ctx, `SELECT id,seq,fingerprint,state,severity,origin,in_scope FROM review_findings WHERE ship_task_id=? ORDER BY seq`, changeTaskID)
	if err != nil {
		return result, err
	}
	ledgerByFingerprint := map[string]reviewFindingLedgerRow{}
	ledgerBySequence := map[int]reviewFindingLedgerRow{}
	for ledgerRows.Next() {
		var row reviewFindingLedgerRow
		var inScope int
		if err := ledgerRows.Scan(&row.ID, &row.Seq, &row.Fingerprint, &row.State, &row.Severity, &row.Origin, &inScope); err != nil {
			_ = ledgerRows.Close()
			return result, err
		}
		row.InScope = inScope == 1
		ledgerByFingerprint[row.Fingerprint] = row
		ledgerBySequence[row.Seq] = row
	}
	if err := ledgerRows.Err(); err != nil {
		_ = ledgerRows.Close()
		return result, err
	}
	if err := ledgerRows.Close(); err != nil {
		return result, err
	}

	patternIDs := map[string]int64{}
	for _, pattern := range input.Patterns {
		for _, finding := range pattern.Instances {
			key := normalizeReviewPatternKey(pattern.Key)
			if key == "" {
				seed := findingFingerprint("unclassified", finding.Repository, finding.Path, finding.Symbol, finding.Excerpt)
				key = "unclassified-" + seed[:12]
			}
			fingerprint := findingFingerprint(key, finding.Repository, finding.Path, finding.Symbol, finding.Excerpt)
			severity := pattern.Severity
			if !oneOf(severity, "P0", "P1", "P2", "P3") {
				severity = "unclassified"
			}
			origin := pattern.Origin
			if !finding.InScope {
				origin = "pre_existing"
			}

			if sequence, claimed := findingSequence(finding.Regresses); claimed {
				prior, exists := ledgerBySequence[sequence]
				if exists && prior.State == "fixed" && finding.InScope {
					if err := appendReviewFindingEventTx(ctx, tx, prior.ID, round, "regressed", "reviewer", finding.Description); err != nil {
						return result, err
					}
					if _, err := tx.ExecContext(ctx, `UPDATE review_findings SET state='regressed' WHERE id=?`, prior.ID); err != nil {
						return result, err
					}
					prior.State = "regressed"
					ledgerBySequence[sequence] = prior
					ledgerByFingerprint[prior.Fingerprint] = prior
					result.Regressed = append(result.Regressed, "F-"+fmt.Sprint(sequence))
					continue
				}
			}
			if prior, exists := ledgerByFingerprint[fingerprint]; exists {
				if prior.State == "fixed" && finding.InScope {
					if err := appendReviewFindingEventTx(ctx, tx, prior.ID, round, "regressed", "reviewer", finding.Description); err != nil {
						return result, err
					}
					if _, err := tx.ExecContext(ctx, `UPDATE review_findings SET state='regressed' WHERE id=?`, prior.ID); err != nil {
						return result, err
					}
					prior.State = "regressed"
					ledgerBySequence[prior.Seq] = prior
					ledgerByFingerprint[fingerprint] = prior
					result.Regressed = append(result.Regressed, "F-"+fmt.Sprint(prior.Seq))
				} else {
					result.SuppressedRepeat++
				}
				continue
			}
			if sequence, claimed := findingSequence(finding.Matches); claimed {
				if _, exists := ledgerBySequence[sequence]; exists {
					result.SuppressedRepeat++
					continue
				}
			}

			patternID, found := patternIDs[key]
			if !found {
				var existing int64
				err := tx.QueryRowContext(ctx, `SELECT id FROM review_patterns WHERE ship_task_id=? AND key=?`, changeTaskID, key).Scan(&existing)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return result, err
				}
				if errors.Is(err, sql.ErrNoRows) {
					var seq int
					if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM review_patterns WHERE ship_task_id=?`, changeTaskID).Scan(&seq); err != nil {
						return result, err
					}
					inserted, err := tx.ExecContext(ctx, `INSERT INTO review_patterns(ship_task_id,seq,key,title,rule,root_cause,fix_guidance,created_round) VALUES(?,?,?,?,?,?,?,?)`,
						changeTaskID, seq, key, pattern.Title, pattern.Rule, pattern.RootCause, pattern.FixGuidance, round)
					if err != nil {
						return result, err
					}
					result.NewPatterns++
					existing, err = inserted.LastInsertId()
					if err != nil {
						return result, err
					}
				}
				patternID = existing
				patternIDs[key] = patternID
			}
			var seq int
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM review_findings WHERE ship_task_id=?`, changeTaskID).Scan(&seq); err != nil {
				return result, err
			}
			initialState := "open"
			var head string
			if err := tx.QueryRowContext(ctx, `SELECT head_sha FROM review_rounds WHERE review_task_id=? AND repository=?`, reviewTaskID, finding.Repository).Scan(&head); err != nil {
				return result, err
			}
			resultInsert, err := tx.ExecContext(ctx, `INSERT INTO review_findings(ship_task_id,seq,pattern_id,fingerprint,round,repository,path,symbol,excerpt,line_at_report,head_sha,severity,origin,in_scope,state) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				changeTaskID, seq, patternID, fingerprint, round, finding.Repository, finding.Path, finding.Symbol, strings.TrimSpace(finding.Excerpt), finding.Line,
				head, severity, origin, finding.InScope, initialState)
			if err != nil {
				return result, err
			}
			findingID, err := resultInsert.LastInsertId()
			if err != nil {
				return result, err
			}
			if err := appendReviewFindingEventTx(ctx, tx, findingID, round, "reported", "reviewer", finding.Description); err != nil {
				return result, err
			}
			row := reviewFindingLedgerRow{ID: findingID, Seq: seq, Fingerprint: fingerprint, State: initialState, Severity: severity, Origin: origin, InScope: finding.InScope}
			ledgerByFingerprint[fingerprint] = row
			ledgerBySequence[seq] = row
			result.NewFindings++
			result.BySeverity[severity]++
			if !finding.InScope {
				result.OutOfScope++
				if pattern.Origin == "introduced" {
					result.Reclassified = append(result.Reclassified, fmt.Sprintf("%s:%d (%s)", finding.Path, finding.Line, key))
				}
			}
		}
	}

	if err := resolveClaimedFindingsTx(ctx, tx, changeTaskID, round, ledgerBySequence, input.Resolutions); err != nil {
		return result, err
	}
	blocking, err := hasBlockingReviewFindingTx(ctx, tx, changeTaskID, input.BlockAt)
	if err != nil {
		return result, err
	}
	result.DerivedVerdict = ReviewPass
	if blocking {
		result.DerivedVerdict = ReviewChangesRequested
	}
	return result, nil
}

func appendReviewFindingEventTx(ctx context.Context, tx *writeTx, findingID int64, round int, event, actor, note string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO review_finding_events(finding_id,round,event,actor,note,at) VALUES(?,?,?,?,?,?)`, findingID, round, event, actor, note, time.Now().UnixMilli())
	return err
}

func resolveClaimedFindingsTx(ctx context.Context, tx *writeTx, changeTaskID int64, round int, ledger map[int]reviewFindingLedgerRow, resolutions []ReviewFindingResolutionInput) error {
	claimed := map[int]bool{}
	for _, row := range ledger {
		if row.State == "fix_claimed" {
			claimed[row.Seq] = true
		}
	}
	answered := map[int]ReviewFindingResolutionInput{}
	for _, resolution := range resolutions {
		seq, _ := findingSequence(resolution.Finding)
		if !claimed[seq] {
			return fmt.Errorf("%w: resolution %s does not name a finding claimed fixed in this round", ErrReviewFindingsInvalid, resolution.Finding)
		}
		if _, duplicate := answered[seq]; duplicate {
			return fmt.Errorf("%w: duplicate resolution for %s", ErrReviewFindingsInvalid, resolution.Finding)
		}
		answered[seq] = resolution
	}
	sequences := make([]int, 0, len(claimed))
	for seq := range claimed {
		sequences = append(sequences, seq)
	}
	sort.Ints(sequences)
	for _, seq := range sequences {
		row := ledger[seq]
		resolution, found := answered[seq]
		event, state, note := "unverified", "unverified", ""
		if found {
			note = resolution.Note
			if resolution.Status == "resolved" {
				event, state = "resolved", "fixed"
			} else {
				event, state = "unresolved", "open"
			}
		}
		if err := appendReviewFindingEventTx(ctx, tx, row.ID, round, event, "reviewer", note); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_findings SET state=? WHERE id=? AND ship_task_id=?`, state, row.ID, changeTaskID); err != nil {
			return err
		}
		row.State = state
		ledger[seq] = row
	}
	return nil
}

func hasBlockingReviewFindingTx(ctx context.Context, tx *writeTx, changeTaskID int64, blockAt string) (bool, error) {
	threshold := map[string]int{"P0": 0, "P1": 1, "P2": 2}[blockAt]
	rows, err := tx.QueryContext(ctx, `SELECT state,severity,origin,in_scope FROM review_findings WHERE ship_task_id=?`, changeTaskID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var state, severity, origin string
		var inScope int
		if err := rows.Scan(&state, &severity, &origin, &inScope); err != nil {
			return false, err
		}
		if origin != "introduced" || inScope != 1 || !oneOf(state, "open", "fix_claimed", "unverified", "disputed", "regressed") {
			continue
		}
		if severity == "unclassified" {
			return true, nil
		}
		rank := map[string]int{"P0": 0, "P1": 1, "P2": 2, "P3": 3}[severity]
		if rank <= threshold {
			return true, nil
		}
	}
	return false, rows.Err()
}

func insertReviewRoundNoticesTx(ctx context.Context, tx *writeTx, projectID, changeTaskID int64, changeTitle string, result reviewFindingsResult, verdict string) error {
	data := map[string]any{
		"round": result.Round, "verdict": verdict, "new_patterns": result.NewPatterns,
		"new_findings": result.NewFindings, "suppressed_repeats": result.SuppressedRepeat,
		"out_of_scope": result.OutOfScope, "findings_by_severity": result.BySeverity,
	}
	if len(result.Regressed) > 0 {
		data["regressed"] = result.Regressed
	}
	if len(result.Reclassified) > 0 {
		data["origin_reclassified"] = result.Reclassified
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	summary := fmt.Sprintf("%s: Review Round %d recorded %s; %d repeated finding(s) suppressed, %d out-of-scope finding(s)", changeTitle, result.Round, verdict, result.SuppressedRepeat, result.OutOfScope)
	if len(result.Reclassified) > 0 {
		summary += "; " + reclassifiedFindingsText(result.Reclassified)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at) VALUES(?,?,'review_round_done',?,?,?)`, projectID, changeTaskID, summary, string(encoded), time.Now().UnixMilli()); err != nil {
		return err
	}
	if len(result.Regressed) > 0 {
		regressionData, err := json.Marshal(map[string]any{"round": result.Round, "findings": result.Regressed})
		if err != nil {
			return err
		}
		regressionSummary := fmt.Sprintf("%s: Review Round %d reintroduced fixed finding(s) %s", changeTitle, result.Round, strings.Join(result.Regressed, ", "))
		if _, err := tx.ExecContext(ctx, `INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at) VALUES(?,?,'review_regression',?,?,?)`, projectID, changeTaskID, regressionSummary, string(regressionData), time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

func reclassifiedFindingsText(findings []string) string {
	return fmt.Sprintf("%d finding(s) supplied as introduced recorded pre_existing because they lie outside this Review Round's scope: %s", len(findings), strings.Join(findings, ", "))
}

func oneOf(value string, values ...string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
