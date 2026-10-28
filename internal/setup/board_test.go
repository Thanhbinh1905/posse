package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// boardFixture writes the board page, the harness and one data file per
// source, with times relative to now so ages render the same across loads.
func boardFixture(t *testing.T) string {
	t.Helper()
	now := time.Now()
	ago := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }
	task := func(id, kind, state, ticket string, extra map[string]any) map[string]any {
		row := map[string]any{"id": id, "type": kind, "state": state, "title": "Task " + id, "name": id, "profile": "deep", "created_at": ago(90 * time.Minute)}
		if ticket != "" {
			row["ticket"] = ticket
		}
		maps.Copy(row, extra)
		return row
	}
	posse := map[string]any{
		"project": map[string]any{"name": "shop", "lead": "idle"},
		"pool":    map[string]any{"used": 3, "max": 4},
		"notices": []any{map[string]any{"id": 7, "task": "t5", "kind": "pr_checks_failed", "summary": "CI failed"}},
		"tasks": []any{
			task("t1", "change", "queued", "#21", map[string]any{"after": []string{"t2:landed"}, "wait_reason": "waiting for t2:landed"}),
			task("t2", "change", "working", "#20", nil),
			task("t3", "change", "spawning", "", nil),
			task("t4", "change", "needs-decision", "", map[string]any{"last_signal": "needs-decision: which flag?"}),
			task("t5", "change", "done", "#22", map[string]any{"pr_url": "https://example.test/pull/5", "ci": "failure", "review_round": 2}),
			task("t6", "change", "merging", "", nil),
			task("t7", "research", "done", "", nil),
			task("t8", "review", "working", "", nil),
			task("t9", "change", "blocked", "", nil),
			task("t10", "change", "stalled", "", nil),
		},
		"needs_you": []any{task("t11", "change", "failed", "", map[string]any{"reason": "exited"})},
	}
	issue := func(number int, title, state, body string, created, closed time.Duration) map[string]any {
		row := map[string]any{"number": number, "title": title, "state": state, "body": body, "url": fmt.Sprintf("https://example.test/issues/%d", number), "createdAt": ago(created), "closedAt": nil, "labels": []any{map[string]any{"name": "ready-for-agent"}}}
		if state == "CLOSED" {
			row["closedAt"] = ago(closed)
		}
		return row
	}
	issues := []any{
		issue(10, "Spec: board", "OPEN", "", 5*time.Hour, 0),
		issue(20, "Claimed by t2", "OPEN", "Parent spec: #10", 5*time.Hour, 0),
		issue(21, "Claimed by t1", "OPEN", "Parent spec: #10\n\n**Blocked by:** #20", 5*time.Hour, 0),
		issue(22, "Claimed by t5", "OPEN", "", 5*time.Hour, 0),
		issue(23, "Ready under the spec", "OPEN", "Parent spec: #10\n\nBlocked by: #20, #24", 4*time.Hour, 0),
		issue(24, "Merged today", "CLOSED", "Parent spec: #10", 4*time.Hour, time.Second),
		issue(25, "Merged days ago", "CLOSED", "", 400*time.Hour, 100*time.Hour),
		issue(26, "Loose and stale", "OPEN", "", 40*24*time.Hour, 0),
	}

	dir := t.TempDir()
	page, err := files.ReadFile("skills/posse-board/board.html")
	if err != nil {
		t.Fatal(err)
	}
	harness, err := os.ReadFile(filepath.Join("testdata", "board-harness.html"))
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]any{"posse": posse, "issues": issues} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		write(name+".js", fmt.Appendf(nil, "posseBoardData(%q, %s);\n", name, data))
	}
	write("index.html", page)
	write("harness.html", harness)
	return dir
}

func chromeBinary(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	if os.Getenv("CI") != "" {
		t.Fatal("the board browser tests need Chrome or Chromium on PATH")
	}
	t.Skip("Chrome or Chromium is not on PATH")
	return ""
}

func runChrome(t *testing.T, chrome string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	base := []string{"--headless=new", "--disable-gpu", "--no-sandbox", "--hide-scrollbars", "--no-first-run", "--user-data-dir=" + t.TempDir(), "--virtual-time-budget=8000"}
	output, err := exec.CommandContext(ctx, chrome, append(base, args...)...).Output()
	if err != nil {
		t.Fatalf("chrome %v: %v", args, err)
	}
	return output
}

type boardRun struct {
	Columns     map[string]string `json:"columns"`
	Lanes       []string          `json:"lanes"`
	PageScrolls bool              `json:"pageScrolls"`
	Selected    string            `json:"selected"`
	Dialog      bool              `json:"dialog"`
	Prompts     []struct {
		Text    string `json:"text"`
		Options struct {
			Tag  string         `json:"tag"`
			Data map[string]any `json:"data"`
		} `json:"options"`
	} `json:"prompts"`
	Sends    int    `json:"sends"`
	Activity string `json:"activity"`
}

func runBoard(t *testing.T, chrome, url string, width, height int) boardRun {
	t.Helper()
	dom := runChrome(t, chrome, "--window-size=1600,1000", "--dump-dom", fmt.Sprintf("%s/harness.html?w=%d&h=%d&issue=23", url, width, height))
	match := regexp.MustCompile(`(?s)<pre id="result">(.*?)</pre>`).FindSubmatch(dom)
	if match == nil || len(match[1]) == 0 {
		t.Fatalf("harness wrote no result: %s", dom)
	}
	var run boardRun
	if err := json.Unmarshal([]byte(html.UnescapeString(string(match[1]))), &run); err != nil {
		t.Fatalf("decode harness result %s: %v", match[1], err)
	}
	return run
}

func TestBoardMapsFixtureDataAndQueuesOneStartPrompt(t *testing.T) {
	chrome := chromeBinary(t)
	server := httptest.NewServer(http.FileServer(http.Dir(boardFixture(t))))
	defer server.Close()

	run := runBoard(t, chrome, server.URL, 1440, 900)
	want := map[string]string{
		"t1": "running", "t2": "running", "t3": "running",
		"t4": "needs", "t9": "needs", "t10": "needs", "t11": "needs",
		"t5": "pr", "t6": "pr", "t7": "merged",
		"#23": "ready", "#26": "ready", "#24": "merged",
	}
	if len(run.Columns) != len(want) {
		t.Errorf("cards = %v, want exactly %v", run.Columns, want)
	}
	for key, column := range want {
		if run.Columns[key] != column {
			t.Errorf("card %s in column %q, want %q", key, run.Columns[key], column)
		}
	}
	if strings.Join(run.Lanes, ",") != "10,loose" {
		t.Errorf("lanes = %v, want spec #10 then loose work", run.Lanes)
	}

	if run.Selected != "#23" || !run.Dialog {
		t.Fatalf("selected %q with dialog %v, want #23 with a confirmation", run.Selected, run.Dialog)
	}
	if len(run.Prompts) != 1 || run.Sends != 1 {
		t.Fatalf("prompts = %+v, sends = %d; want one prompt sent once", run.Prompts, run.Sends)
	}
	prompt := run.Prompts[0]
	if prompt.Options.Tag != "posse-board-start" || prompt.Options.Data["issue"] != float64(23) || !strings.Contains(prompt.Text, "#23") {
		t.Errorf("prompt = %+v, want posse-board-start for #23", prompt)
	}
	if !strings.Contains(run.Activity, "sent start #23 to the Lead") || !strings.Contains(run.Activity, "#23 was already sent") {
		t.Errorf("activity = %q, want the send and the refused second start", run.Activity)
	}
}

func TestBoardHasNoPageScrollAtDesktopAndNarrowWidths(t *testing.T) {
	chrome := chromeBinary(t)
	server := httptest.NewServer(http.FileServer(http.Dir(boardFixture(t))))
	defer server.Close()
	for _, size := range [][2]int{{1440, 900}, {1240, 860}, {390, 844}} {
		if run := runBoard(t, chrome, server.URL, size[0], size[1]); run.PageScrolls {
			t.Errorf("page scrolls at %dx%d", size[0], size[1])
		}
	}
}

func TestBoardRendersIdenticallyAcrossLoads(t *testing.T) {
	chrome := chromeBinary(t)
	server := httptest.NewServer(http.FileServer(http.Dir(boardFixture(t))))
	defer server.Close()
	var shots [][]byte
	for range 2 {
		path := filepath.Join(t.TempDir(), "board.png")
		runChrome(t, chrome, "--window-size=1440,900", "--screenshot="+path, server.URL+"/index.html")
		shot, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		shots = append(shots, shot)
	}
	if !bytes.Equal(shots[0], shots[1]) {
		t.Error("two loads of the same data gave different screenshots")
	}
}
