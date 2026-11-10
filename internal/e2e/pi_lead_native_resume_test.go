//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// A Herdr server restart resumes a Pi Lead from its recorded session as a
// bare `pi`, without the `--extension` Posse passed at launch (issue #962).
// The resumed Lead must still receive fresh Notices.
func TestPiLeadReceivesNoticesAfterHerdrNativeResume(t *testing.T) {
	piLeadNativeResume(t, true)
}

// Without the global extension copy, for example before the User reruns
// `posse setup`, the resumed Lead gets Notices typed into its pane and one
// Notice with the recovery step, and the restart stays held.
func TestPiLeadWithoutReceiverGetsTypedNoticesAfterHerdrNativeResume(t *testing.T) {
	piLeadNativeResume(t, false)
}

func piLeadNativeResume(t *testing.T, globalCopy bool) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("real Pi executable is not installed")
	}
	root := newFixtureRoot(t, fixturePrefix("piresume-"))
	binDir := filepath.Join(root, "bin")
	piHome := filepath.Join(root, "pi")
	for _, directory := range []string{binDir, piHome, filepath.Join(root, "home"), filepath.Join(root, "posse"), filepath.Join(root, "claude"), filepath.Join(root, "codex")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := isolatedE2EEnv(t, root)
	binary := filepath.Join(binDir, "posse")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = moduleRoot(t)
	build.Env = env
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse: %v\n%s", err, output)
	}
	env = setEnv(env, "POSSE_TEST_ROOT", root)
	env = setEnv(env, "PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(typ string, value map[string]any) {
			value["type"] = typ
			encoded, _ := json.Marshal(value)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, encoded)
		}
		emit("response.created", map[string]any{"response": map[string]any{"id": "response"}})
		emit("response.output_item.done", map[string]any{"output_index": 0, "item": map[string]any{"type": "message", "id": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Noted."}}}})
		emit("response.completed", map[string]any{"response": map[string]any{"id": "response", "status": "completed", "output": []any{}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
	}))
	defer model.Close()
	models := fmt.Sprintf(`{"providers":{"local":{"baseUrl":%q,"api":"openai-responses","apiKey":"local-test-only","models":[{"id":"test","contextWindow":200000,"maxTokens":1024}]}}}`, model.URL)
	if err := os.WriteFile(filepath.Join(piHome, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(piHome, "settings.json"), []byte(`{"retry":{"enabled":false},"showImages":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	trust, _ := json.Marshal(map[string]bool{root: true})
	if err := os.WriteFile(filepath.Join(piHome, "trust.json"), trust, 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "posse")
	config := "[lead]\nkind = \"pi\"\n\n[lead.profiles]\npi = \"lead\"\n\n[profiles.lead]\nkind = \"pi\"\nmodel = \"test\"\nargs = [\"--provider\", \"local\"]\n\n[defaults]\nmerge_mode = \"local\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	initRepository(t, repo, filepath.Join(root, "origin.git"), env)

	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	client := herdr.NewWithEnv("herdr", env)
	server := startServer(t, client)
	if _, err := client.Run(context.Background(), "integration", "install", "pi"); err != nil {
		t.Fatalf("install Pi integration in isolated Herdr: %v", err)
	}
	runPosse(t, binary, root, env, "setup", "--binary", binary)
	if !globalCopy {
		if err := os.Remove(filepath.Join(piHome, "extensions", "posse-lead.ts")); err != nil {
			t.Fatal(err)
		}
	}
	workspace, err := createWorkspace(client, repo)
	if err != nil {
		t.Fatal(err)
	}
	// `posse up` execs the Lead in the pane that runs it, as for the User.
	if _, err := client.Run(context.Background(), "pane", "run", workspace.RootPane.PaneID, "posse up --name shop --yes"); err != nil {
		t.Fatal(err)
	}

	leadPane := func() (herdr.Pane, bool) {
		snapshot, err := client.Snapshot(context.Background())
		if err != nil {
			return herdr.Pane{}, false
		}
		for _, pane := range snapshot.Panes {
			if pane.Label == "posse:shop:lead" && pane.Agent == "pi" {
				return pane, true
			}
		}
		return herdr.Pane{}, false
	}
	if !waitForCondition(60*time.Second, func() bool { _, found := leadPane(); return found }) {
		snapshot, _ := client.Snapshot(context.Background())
		screen, _ := client.Run(context.Background(), "pane", "read", workspace.RootPane.PaneID)
		t.Fatalf("posse up did not start the Pi Lead: %#v\n%s", snapshot.Panes, screen)
	}

	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.ProjectByName(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	// Only the Pi Lead extension writes posse-notices custom messages.
	sessionNotices := func(marker string) int {
		count := 0
		files, _ := filepath.Glob(filepath.Join(piHome, "sessions", "*", "*.jsonl"))
		for _, file := range files {
			data, err := os.ReadFile(file)
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, marker) && strings.Contains(line, `"customType":"posse-notices"`) {
					count++
				}
			}
		}
		return count
	}
	assertOneReceiver := func(marker string) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if receivers := handoffReceivers(root); len(receivers) > 1 {
				t.Fatalf("the Pi Lead runs more than one Notice receiver: %v", receivers)
			}
		}
		if count := sessionNotices(marker); count != 1 {
			t.Fatalf("the Pi Lead session holds %d copies of the %s Notice, want 1", count, marker)
		}
	}
	if _, err := db.CreateNotice(context.Background(), store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "before-restart-marker"}); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(60*time.Second, func() bool { return sessionNotices("before-restart-marker") > 0 }) {
		pane, _ := leadPane()
		screen, _ := client.Run(context.Background(), "pane", "read", pane.PaneID)
		files, _ := filepath.Glob(filepath.Join(piHome, "sessions", "*", "*.jsonl"))
		notices, _ := db.UndeliveredNotices(context.Background(), project.ID)
		t.Fatalf("the launched Pi Lead extension did not deliver a Notice before the restart: sessions=%v undelivered=%d\n%s", files, len(notices), screen)
	}
	assertOneReceiver("before-restart-marker")

	stopServer(t, client, server)
	server = startServer(t, client)
	defer stopServer(t, client, server)
	if !waitForCondition(60*time.Second, func() bool { _, found := leadPane(); return found }) {
		t.Fatal("Herdr did not resume the Pi Lead after its restart")
	}

	if _, err := db.CreateNotice(context.Background(), store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "after-restart-marker"}); err != nil {
		t.Fatal(err)
	}
	if !globalCopy {
		// The typed fallback waits for an unfocused Lead pane.
		other, err := createWorkspace(client, root)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Call(context.Background(), "workspace.focus", map[string]any{"workspace_id": other.Workspace.WorkspaceID}); err != nil {
			t.Fatal(err)
		}
		typed := func(marker string) bool {
			files, _ := filepath.Glob(filepath.Join(piHome, "sessions", "*", "*.jsonl"))
			for _, file := range files {
				data, _ := os.ReadFile(file)
				for _, line := range strings.Split(string(data), "\n") {
					if strings.Contains(line, marker) && strings.Contains(line, `"role":"user"`) {
						return true
					}
				}
			}
			return false
		}
		if !waitForCondition(120*time.Second, func() bool {
			return typed("after-restart-marker") && typed("Run `posse setup`, then `/reload` in the shop Lead pane")
		}) {
			pane, _ := leadPane()
			screen, _ := client.Run(context.Background(), "pane", "read", pane.PaneID)
			notices, _ := db.UndeliveredNotices(context.Background(), project.ID)
			watchLog, _ := os.ReadFile(filepath.Join(home, "projects", "shop", "watch.log"))
			all, _ := db.Notices(context.Background(), project.ID, false)
			t.Logf("watch log:\n%s\nwatches=%v notices=%+v", watchLog, watchPIDs(root), all)
			snapshot, _ := client.Snapshot(context.Background())
			t.Fatalf("the Lead without a receiver did not get the Notice and the recovery step typed: undelivered=%d lead=%#v focused=%q\n%s", len(notices), pane, snapshot.FocusedPaneID, screen)
		}
		snapshot, err := client.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if generation, err := db.ProjectServerStartedAt(context.Background(), project.ID); err != nil || generation == snapshot.ServerStartedAt {
			t.Fatalf("the restart hold ended for a Lead without a Notice receiver: generation=%q err=%v", generation, err)
		}
		return
	}
	if !waitForCondition(60*time.Second, func() bool { return sessionNotices("after-restart-marker") > 0 }) {
		pane, _ := leadPane()
		screen, _ := client.Run(context.Background(), "pane", "read", pane.PaneID)
		notices, _ := db.UndeliveredNotices(context.Background(), project.ID)
		t.Fatalf("the natively resumed Pi Lead did not receive a fresh Notice: undelivered=%d receivers=%v\n%s", len(notices), handoffReceivers(root), screen)
	}
	assertOneReceiver("after-restart-marker")
}

// handoffReceivers lists the fixture's Lead-side `posse watch --handoff`
// processes, the receivers a Lead extension keeps running.
func handoffReceivers(root string) []string {
	entries, _ := os.ReadDir("/proc")
	var receivers []string
	for _, entry := range entries {
		processRoot := filepath.Join("/proc", entry.Name())
		cmdline, err := os.ReadFile(filepath.Join(processRoot, "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		if len(args) < 2 || args[1] != "watch" || !strings.Contains(string(cmdline), "--handoff") {
			continue
		}
		if environ, _ := os.ReadFile(filepath.Join(processRoot, "environ")); fixtureHasPOSSEHome(environ, root) {
			receivers = append(receivers, entry.Name()+" "+strings.Join(args, " "))
		}
	}
	return receivers
}
