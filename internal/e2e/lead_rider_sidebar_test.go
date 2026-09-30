//go:build e2e

package e2e

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLeadHarnessAndRepositoryRiderSidebar(t *testing.T) {
	f := newRiderTabsFixture(t)
	first := f.ride(t, "t1", "Sidebar first", "sidebar-first")
	second := f.ride(t, "t2", "Sidebar second", "sidebar-second")
	assertLeadRiderSidebarSnapshot(t, f, first, second)

	output := assertNativeWorktreeSidebar(t, f, first.ShortName, second.ShortName)
	if len(output) > 0 {
		assertRenderedLeadRiderRows(t, output, first.ShortName, second.ShortName)
	}
	if f.snapshot(t).FocusedPaneID != f.leadPaneID {
		t.Fatal("sidebar client stole focus")
	}

	// Reconcile an existing session with stale Lead metadata and displaced
	// Riders, without relying on a fresh launch or changing any User label.
	if _, err := f.client.Call(context.Background(), "pane.report_metadata", map[string]any{
		"pane_id": f.leadPaneID, "source": "posse", "display_agent": "Lead",
	}); err != nil {
		t.Fatal(err)
	}
	for _, task := range []store.Task{first, second} {
		if _, err := f.client.Call(context.Background(), "pane.report_metadata", map[string]any{
			"pane_id": task.PaneID, "source": "posse", "tokens": map[string]string{"posse_row": task.ShortName},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.client.Call(context.Background(), "workspace.move_block", map[string]any{
		"workspace_ids":       []string{first.HerdrWorkspaceID, second.HerdrWorkspaceID},
		"before_workspace_id": f.userBefore.Workspace.WorkspaceID,
	}); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "roster")
	assertLeadRiderSidebarSnapshot(t, f, first, second)

	f.fail(t, "t2")
	runPosse(t, f.binary, f.repo, f.leadEnv, "unsaddle", "t2", "--discard", "--user-approved", "User approved the sidebar teardown test")
	assertLeadRiderSidebarSnapshot(t, f, first)
	f.assertGone(t, f.snapshot(t), second.PaneID)
}

func assertLeadRiderSidebarSnapshot(t *testing.T, f *riderTabsFixture, tasks ...store.Task) {
	t.Helper()
	snap := f.snapshot(t)
	leadFound := false
	for _, pane := range snap.Panes {
		if pane.PaneID == f.leadPaneID {
			leadFound = true
			if pane.DisplayAgent != "claude" || pane.Title != "Lead: shop" || pane.Tokens["posse_row"] != "Lead:shop" || pane.Label != "posse:shop:lead" {
				t.Errorf("Lead sidebar metadata: harness=%q title=%q row=%q label=%q; want claude, Lead: shop, Lead:shop, posse:shop:lead", pane.DisplayAgent, pane.Title, pane.Tokens["posse_row"], pane.Label)
			}
		}
		for _, task := range tasks {
			if pane.PaneID == task.PaneID && (pane.DisplayAgent != "claude" || pane.Tokens["posse_row"] != "") {
				t.Errorf("Rider sidebar must show its harness without repeating its workspace name: %#v", pane)
			}
		}
	}
	if !leadFound {
		t.Fatal("Lead disappeared")
	}
	var order []string
	for _, workspace := range snap.Workspaces {
		order = append(order, workspace.WorkspaceID+"="+workspace.Label)
	}
	want := []string{f.userBefore.Workspace.WorkspaceID + "=notes", f.leadWorkspaceID + "=Lead:shop"}
	for _, task := range tasks {
		want = append(want, task.HerdrWorkspaceID+"="+task.ShortName)
	}
	want = append(want, f.userAfter.Workspace.WorkspaceID+"=scratch")
	t.Logf("native workspace order: %v", order)
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("Riders are not directly below their Lead or User layout changed: got %v, want %v", order, want)
	}
	f.assertAlive(t, snap, f.leadPaneID, f.userBefore.RootPane.PaneID, f.userAfter.RootPane.PaneID, f.userTab.RootPane.PaneID)
}

// The capture uses a 160-column client with a 26-column sidebar. Replay
// cursor-addressed updates: the first frame can precede the server snapshot.
func assertRenderedLeadRiderRows(t *testing.T, output []byte, names ...string) {
	t.Helper()
	rows := renderedSidebarLines(output)
	agentsRow, leadRow := 0, 0
	for row := 1; row <= 45; row++ {
		if strings.Contains(strings.ToLower(rows[row]), "agents") {
			agentsRow = row
		}
		if agentsRow > 0 && row > agentsRow && strings.Contains(rows[row], "Lead:shop") {
			leadRow = row
		}
	}
	if agentsRow == 0 || leadRow == 0 || !strings.HasPrefix(rows[leadRow+1], "claude") {
		t.Fatalf("rendered Agents panel does not show the Lead's harness: %#v", rows)
	}
	var proof []string
	proof = append(proof, rows[leadRow], rows[leadRow+1])
	for i, name := range names {
		row := leadRow + 2*(i+1)
		if strings.Count(rows[row], name) != 1 || !strings.HasPrefix(rows[row+1], "claude") {
			t.Errorf("rendered Rider %s is not directly below Lead with one name and its harness: %q / %q", name, rows[row], rows[row+1])
		}
		proof = append(proof, rows[row], rows[row+1])
	}
	t.Logf("Rendered Agents panel:\n%s", strings.Join(proof, "\n"))
}

func TestSidebarCaptureReplaysSparseUpdatesAfterEmptyBootstrap(t *testing.T) {
	output := "\x1b[25;1Hagents grouped\x1b[25;27H" +
		"\x1b[27;1H                         \x1b[27;27H" +
		"\x1b[27;4H\x1b[1mLead:shop\x1b[0m\x1b[28;4Hclaude" +
		"\x1b[29;4Hfirst\x1b[30;4Hclaude" +
		"\x1b[31;4Hsecond\x1b[32;4Hclaude" +
		"\x1b]0;Lead:shop\x07\x1b[?1049l\x1b[27;1Hshell prompt"
	assertRenderedLeadRiderRows(t, []byte(output), "first", "second")
}

// Herdr's ratatui output uses absolute cursor positions and SGR styling. Only
// sidebar cells matter here; stop before detaching restores the shell screen.
func renderedSidebarLines(output []byte) map[int]string {
	text := strings.SplitN(string(output), "\x1b[?1049l", 2)[0]
	osc := regexp.MustCompile(`(?s)\x1b\].*?(?:\x07|\x1b\\)`)
	text = osc.ReplaceAllString(text, "")
	styling := regexp.MustCompile(`\x1b\[[0-9;?]*[A-GI-Za-z]`)
	text = styling.ReplaceAllString(text, "")
	cursor := regexp.MustCompile(`\x1b\[([0-9]+);([0-9]+)H([^\x1b]*)`)
	cells := make(map[int][]rune)
	for _, match := range cursor.FindAllStringSubmatch(text, -1) {
		row, _ := strconv.Atoi(match[1])
		col, _ := strconv.Atoi(match[2])
		if row < 1 || row > 45 || col < 1 || col > 26 {
			continue
		}
		if cells[row] == nil {
			cells[row] = []rune(strings.Repeat(" ", 26))
		}
		for _, character := range match[3] {
			if character < ' ' {
				continue
			}
			if col > 26 {
				break
			}
			cells[row][col-1] = character
			col++
		}
	}
	rows := make(map[int]string)
	for row, line := range cells {
		rows[row] = strings.TrimSpace(string(line))
	}
	return rows
}
