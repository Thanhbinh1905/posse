package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClientCallUsesNewlineJSONSocketProtocol(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer connection.Close()
		var request APIRequest
		if err := json.NewDecoder(bufio.NewReader(connection)).Decode(&request); err != nil {
			done <- err
			return
		}
		response := map[string]any{"id": request.ID, "result": map[string]any{"type": "fake", "method": request.Method}}
		done <- json.NewEncoder(connection).Encode(response)
	}()
	env := []string{"HERDR_SOCKET_PATH=" + socket}
	client := NewWithEnv("herdr", env)
	result, err := client.Call(context.Background(), "session.snapshot", map[string]any{"sample": true})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["method"] != "session.snapshot" {
		t.Fatalf("response = %#v", decoded)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestClientMapsHerdrErrors(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer connection.Close()
		var request APIRequest
		if err := json.NewDecoder(bufio.NewReader(connection)).Decode(&request); err != nil {
			done <- err
			return
		}
		done <- json.NewEncoder(connection).Encode(map[string]any{
			"id": request.ID, "error": map[string]any{"code": "pane_not_found", "message": "missing pane"},
		})
	}()
	client := NewWithEnv("herdr", []string{"HERDR_SOCKET_PATH=" + socket})
	_, err = client.Call(context.Background(), "pane.get", map[string]any{"pane_id": "w0:p0"})
	herdrError, ok := err.(*Error)
	if !ok || herdrError.Code != "pane_not_found" || herdrError.Message != "missing pane" {
		t.Fatalf("mapped error = %#v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestProtocolCompatibilityIsCheckedAgainstStatus(t *testing.T) {
	fake := NewFake()
	fake.Server = Status{Running: true, Protocol: MinimumProtocol - 1, Compatible: false}
	if err := fake.CheckProtocol(context.Background()); err == nil {
		t.Fatal("accepted an older Herdr protocol")
	}
	fake.Server = Status{Running: true, Protocol: MinimumProtocol, Compatible: true}
	if err := fake.CheckProtocol(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestIsolatedTestEnvironmentRedirectsAllWritableState(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", TestRootName())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if _, err := WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_SOCKET_PATH", "/home/user/live.sock")
	t.Setenv("HERDR_PANE_ID", "w9:p9")
	t.Setenv("LANG", "C")
	t.Setenv("LC_ALL", "C")
	t.Setenv("TERM", "dumb")
	t.Setenv("SHELL", "/bin/sh")
	env := IsolatedTestEnvironment(root)
	for key, want := range map[string]string{"LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "TERM": "xterm-256color", "SHELL": "/bin/bash"} {
		if got := environmentMap(env)[key]; got != want {
			t.Errorf("isolated %s = %q, want %q", key, got, want)
		}
	}
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "HERDR_") {
			t.Fatalf("inherited Herdr setting survived: %s", key)
		}
	}
	validated, err := ValidateIsolatedEnvironment(env)
	if err != nil || validated != root {
		t.Fatalf("ValidateIsolatedEnvironment() = %q, %v", validated, err)
	}
	if !strings.Contains(strings.Join(env, "\n"), "HOME="+filepath.Join(root, "home")) || !strings.Contains(strings.Join(env, "\n"), "GIT_CONFIG_GLOBAL=/dev/null") || !strings.Contains(strings.Join(env, "\n"), "GIT_CONFIG_NOSYSTEM=1") {
		t.Fatal("test environment did not isolate HOME and Git config")
	}
	client := NewWithEnv("herdr", env)
	if socket := client.socketPath(); len(socket) >= 108 {
		t.Fatalf("test socket path too long (%d): %s", len(socket), socket)
	}
	unsafe := append(append([]string(nil), env...), "HERDR_SOCKET_PATH=/home/user/live.sock")
	if _, err := ValidateIsolatedEnvironment(unsafe); err == nil {
		t.Fatal("accepted inherited live socket override")
	}
}

func TestIsolatedTestEnvironmentDisablesHerdrNetworkUpdates(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", TestRootName())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	configPath, err := WriteIsolatedConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	env := IsolatedTestEnvironment(root)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range []string{"version_check", "manifest_check"} {
		updated := strings.Replace(string(config), setting+" = false", setting+" = true", 1)
		if updated == string(config) {
			t.Fatalf("isolated Herdr config omitted %s", setting)
		}
		if err := os.WriteFile(configPath, []byte(updated), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateIsolatedEnvironment(env); err == nil {
			t.Errorf("accepted enabled Herdr %s network check", setting)
		}
		if err := os.WriteFile(configPath, config, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSnapshotJoinsPaneAndAgentStatus(t *testing.T) {
	fake := NewFake()
	fake.Results["session.snapshot"] = json.RawMessage(`{
"protocol_version":22,
"panes":[{"pane_id":"w1:p1","workspace_id":"w1","label":"posse:p:lead"}],
"agents":[{"pane_id":"w1:p1","agent":"claude","status":"idle","session_id":"s1"}]
}`)
	snapshot, err := fake.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Protocol != 22 || len(snapshot.Panes) != 1 || snapshot.Panes[0].AgentStatus != "idle" || snapshot.Panes[0].Agent != "claude" || len(snapshot.Panes[0].AgentSession) == 0 {
		t.Fatalf("snapshot pane/agent join = %#v", snapshot)
	}
}

func TestSnapshotDecodesHerdrSessionSnapshotEnvelope(t *testing.T) {
	fake := NewFake()
	fake.Results["session.snapshot"] = json.RawMessage(`{"type":"session_snapshot","snapshot":{"protocol":22,"focused_pane_id":"w1:p1","panes":[{"pane_id":"w1:p1","workspace_id":"w1","agent":"claude","agent_status":"idle"}]}}`)
	snapshot, err := fake.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Protocol != MinimumProtocol || snapshot.FocusedPaneID != "w1:p1" || len(snapshot.Panes) != 1 {
		t.Fatalf("decoded snapshot envelope = %#v", snapshot)
	}
}

func TestMethodTimeoutsRespectCallerDeadline(t *testing.T) {
	if got := methodTimeout("session.snapshot"); got != 3*time.Second {
		t.Fatalf("short Herdr method timeout = %s", got)
	}
	for _, method := range []string{"agent.start", "worktree.open", "workspace.create", "workspace.close"} {
		if got := methodTimeout(method); got <= 3*time.Second {
			t.Errorf("long Herdr method %s timeout = %s", method, got)
		}
	}
	defaultContext, cancel := requestContext(context.Background(), "agent.start")
	defer cancel()
	if deadline, ok := defaultContext.Deadline(); !ok || time.Until(deadline) < 40*time.Second {
		t.Fatalf("agent.start default deadline = %v, %v", deadline, ok)
	}
	callerDeadline := time.Now().Add(time.Second)
	callerContext, cancelCaller := context.WithDeadline(context.Background(), callerDeadline)
	defer cancelCaller()
	boundedContext, cancelBounded := requestContext(callerContext, "agent.start")
	defer cancelBounded()
	got, ok := boundedContext.Deadline()
	if !ok || !got.Equal(callerDeadline) {
		t.Fatalf("caller deadline = %v, got %v, %v", callerDeadline, got, ok)
	}
}

func TestProcStartTicksReadsFieldAfterCommandName(t *testing.T) {
	fields := []string{"S"}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, "98765")
	got, err := procStartTicks("42 (herdr server (worker)) " + strings.Join(fields, " "))
	if err != nil || got != "98765" {
		t.Fatalf("proc start time = %q, %v", got, err)
	}
}

func TestFindPanePrefersLabelOverRecordedID(t *testing.T) {
	panes := []Pane{
		{PaneID: "w1:p2", Label: "posse:shop:t2"},
		{PaneID: "w1:p3", Label: "posse:shop:t1"},
		{PaneID: "w1:p4"},
	}
	// After a restart renumbers panes, t1's recorded id names t2's pane.
	if pane, found := FindPane(panes, "w1:p2", "posse:shop:t1"); !found || pane.PaneID != "w1:p3" {
		t.Fatalf("FindPane by label = %#v, %v", pane, found)
	}
	if pane, found := FindPane(panes, "w1:p2", "posse:shop:t9"); found {
		t.Fatalf("FindPane matched another labeled pane by id: %#v", pane)
	}
	if pane, found := FindPane(panes, "w1:p4", "posse:shop:t9"); !found || pane.PaneID != "w1:p4" {
		t.Fatalf("FindPane did not fall back to an unlabeled pane's id: %#v, %v", pane, found)
	}
}
