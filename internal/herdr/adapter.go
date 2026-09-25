package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thanhbinh1905/posse/internal/execgroup"
)

const MinimumProtocol = 22

type Adapter interface {
	Call(context.Context, string, map[string]any) (json.RawMessage, error)
	Snapshot(context.Context) (Snapshot, error)
	Status(context.Context) (Status, error)
	CheckProtocol(context.Context) error
	Run(context.Context, ...string) ([]byte, error)
}

type Client struct {
	Binary       string
	Env          []string
	serial       atomic.Uint64
	protocolOnce sync.Once
	protocolErr  error
}

func New() *Client {
	return &Client{Binary: os.Getenv("HERDR_BIN_PATH")}
}

func NewWithEnv(binary string, env []string) *Client {
	return &Client{Binary: binary, Env: append([]string(nil), env...)}
}

type Status struct {
	Running    bool   `json:"running"`
	Version    string `json:"version"`
	Protocol   int    `json:"protocol"`
	Compatible bool   `json:"compatible"`
	Socket     string `json:"socket"`
}

type Snapshot struct {
	Type               string      `json:"type"`
	Version            string      `json:"version"`
	ServerStartedAt    string      `json:"server_started_at"`
	Protocol           int         `json:"protocol"`
	ProtocolVersion    int         `json:"protocol_version"`
	FocusedWorkspaceID string      `json:"focused_workspace_id"`
	FocusedTabID       string      `json:"focused_tab_id"`
	FocusedPaneID      string      `json:"focused_pane_id"`
	Workspaces         []Workspace `json:"workspaces"`
	Tabs               []Tab       `json:"tabs"`
	Panes              []Pane      `json:"panes"`
	Agents             []Agent     `json:"agents"`
}

type Workspace struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Root        string `json:"cwd"`
	Focused     bool   `json:"focused"`
	Worktree    struct {
		CheckoutPath string `json:"checkout_path"`
	} `json:"worktree"`
}

type Tab struct {
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
}

type Pane struct {
	PaneID       string            `json:"pane_id"`
	WorkspaceID  string            `json:"workspace_id"`
	TabID        string            `json:"tab_id"`
	Label        string            `json:"label"`
	CWD          string            `json:"cwd"`
	Title        string            `json:"title"`
	DisplayAgent string            `json:"display_agent"`
	Tokens       map[string]string `json:"tokens"`
	Focused      bool              `json:"focused"`
	Agent        string            `json:"agent"`
	AgentStatus  string            `json:"agent_status"`
	AgentSession json.RawMessage   `json:"agent_session"`
}

type Agent struct {
	Name        string `json:"name"`
	PaneID      string `json:"pane_id"`
	Kind        string `json:"kind"`
	Agent       string `json:"agent"`
	Status      string `json:"status"`
	AgentStatus string `json:"agent_status"`
	SessionID   string `json:"session_id"`
	SessionPath string `json:"session_path"`
}

type APIRequest struct {
	ID     string         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

type APIResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *APIError       `json:"error"`
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Error struct {
	Code    string
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("Herdr %s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("Herdr %s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

func (c *Client) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method == "" {
		return nil, &Error{Code: "invalid_method", Message: "method cannot be empty"}
	}
	path := c.socketPath()
	if runtime.GOOS == "windows" {
		return nil, &Error{Code: "unsupported_transport", Message: "Windows named pipe transport is not implemented"}
	}
	callCtx, cancel := requestContext(ctx, method)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(callCtx, "unix", path)
	if err != nil {
		return nil, normalizeError(err)
	}
	defer connection.Close()
	if deadline, ok := callCtx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	id := fmt.Sprintf("posse-%x-%x", time.Now().UnixNano(), c.serial.Add(1))
	if params == nil {
		params = map[string]any{}
	}
	if err := json.NewEncoder(connection).Encode(APIRequest{ID: id, Method: method, Params: params}); err != nil {
		return nil, normalizeError(err)
	}
	line, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		return nil, normalizeError(err)
	}
	var response APIResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return nil, &Error{Code: "invalid_response", Message: "Herdr returned invalid JSON", Cause: err}
	}
	if response.Error != nil {
		return nil, &Error{Code: response.Error.Code, Message: response.Error.Message}
	}
	if response.ID != id {
		return nil, &Error{Code: "invalid_response", Message: "Herdr response id did not match request"}
	}
	if len(response.Result) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return response.Result, nil
}

func requestContext(ctx context.Context, method string) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, methodTimeout(method))
}

func methodTimeout(method string) time.Duration {
	switch method {
	case "agent.start", "worktree.open", "workspace.create", "workspace.close":
		return 45 * time.Second
	default:
		return 3 * time.Second
	}
}

func (c *Client) Snapshot(ctx context.Context) (Snapshot, error) {
	result, err := c.Call(ctx, "session.snapshot", nil)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot, err := decodeSnapshot(result)
	if err != nil {
		return Snapshot{}, err
	}
	if snapshot.ServerStartedAt == "" {
		snapshot.ServerStartedAt = serverStartedAt(ctx, c.socketPath())
	}
	return snapshot, nil
}

func decodeSnapshot(result json.RawMessage) (Snapshot, error) {
	var snapshot Snapshot
	var envelope struct {
		Snapshot json.RawMessage `json:"snapshot"`
	}
	if err := json.Unmarshal(result, &envelope); err != nil {
		return Snapshot{}, &Error{Code: "invalid_response", Message: "could not decode session.snapshot", Cause: err}
	}
	if len(envelope.Snapshot) > 0 {
		result = envelope.Snapshot
	}
	if err := json.Unmarshal(result, &snapshot); err != nil {
		return Snapshot{}, &Error{Code: "invalid_response", Message: "could not decode session.snapshot", Cause: err}
	}
	if snapshot.Protocol == 0 {
		snapshot.Protocol = snapshot.ProtocolVersion
	}
	for i := range snapshot.Panes {
		if snapshot.Panes[i].AgentStatus == "" || snapshot.Panes[i].Agent == "" {
			for _, agent := range snapshot.Agents {
				if agent.PaneID == snapshot.Panes[i].PaneID {
					if snapshot.Panes[i].AgentStatus == "" {
						snapshot.Panes[i].AgentStatus = firstNonempty(agent.AgentStatus, agent.Status)
					}
					if snapshot.Panes[i].Agent == "" {
						snapshot.Panes[i].Agent = firstNonempty(agent.Agent, agent.Kind)
					}
					if len(snapshot.Panes[i].AgentSession) == 0 && agent.SessionID != "" {
						snapshot.Panes[i].AgentSession, _ = json.Marshal(map[string]string{"session_id": agent.SessionID, "session_path": agent.SessionPath})
					}
				}
			}
		}
	}
	return snapshot, nil
}

func ReadSnapshot(ctx context.Context, adapter Adapter) (Snapshot, error) {
	return adapter.Snapshot(ctx)
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	output, err := c.Run(ctx, "status", "server", "--json")
	if err != nil {
		return Status{}, err
	}
	var status Status
	if err := json.Unmarshal(output, &status); err != nil {
		return Status{}, &Error{Code: "invalid_response", Message: "could not decode herdr status", Cause: err}
	}
	return status, nil
}

func (c *Client) CheckProtocol(ctx context.Context) error {
	c.protocolOnce.Do(func() {
		status, err := c.Status(ctx)
		if err != nil {
			c.protocolErr = err
			return
		}
		if !status.Running {
			c.protocolErr = &Error{Code: "herdr_unavailable", Message: "Herdr server is not running"}
			return
		}
		if status.Protocol < MinimumProtocol || !status.Compatible {
			c.protocolErr = &Error{Code: "herdr_protocol_incompatible", Message: fmt.Sprintf("posse requires Herdr protocol %d or newer; server reports %d", MinimumProtocol, status.Protocol)}
		}
	})
	return c.protocolErr
}

func (c *Client) Run(ctx context.Context, args ...string) ([]byte, error) {
	binary := c.Binary
	if binary == "" {
		binary = "herdr"
	}
	command := execgroup.CommandContext(ctx, binary, args...)
	if c.Env != nil {
		command.Env = append([]string(nil), c.Env...)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, normalizeError(fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output))))
	}
	return output, nil
}

// SocketPath resolves the Herdr socket a herdr command reaches with env.
func SocketPath(env []string) string {
	return (&Client{Env: append([]string{}, env...)}).socketPath()
}

func (c *Client) socketPath() string {
	env := environmentMap(c.environment())
	if path := env["HERDR_SOCKET_PATH"]; path != "" {
		return path
	}
	configRoot := env["HERDR_CONFIG_PATH"]
	if configRoot != "" {
		configRoot = filepath.Dir(configRoot)
	} else if xdg := env["XDG_CONFIG_HOME"]; xdg != "" {
		configRoot = filepath.Join(xdg, "herdr")
	} else {
		configRoot = filepath.Join(env["HOME"], ".config", "herdr")
	}
	if session := env["HERDR_SESSION"]; session != "" && session != "default" {
		return filepath.Join(configRoot, "sessions", session, "herdr.sock")
	}
	return filepath.Join(configRoot, "herdr.sock")
}

func (c *Client) environment() []string {
	if c.Env != nil {
		return c.Env
	}
	return os.Environ()
}

func (c *Client) StartIsolatedServer() (*ServerProcess, error) {
	if _, err := ValidateIsolatedEnvironment(c.environment()); err != nil {
		return nil, err
	}
	binary := c.Binary
	if binary == "" {
		binary = "herdr"
	}
	command := exec.Command(binary, "server")
	command.Env = append([]string(nil), c.environment()...)
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &ServerProcess{command: command}, nil
}

func (c *Client) StopIsolatedServer(ctx context.Context) error {
	if _, err := ValidateIsolatedEnvironment(c.environment()); err != nil {
		return err
	}
	_, err := c.Run(ctx, "server", "stop")
	return err
}

func environmentMap(values []string) map[string]string {
	result := map[string]string{}
	for _, item := range values {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}

func normalizeError(err error) error {
	if err == nil {
		return nil
	}
	var existing *Error
	if errors.As(err, &existing) {
		return err
	}
	return &Error{Code: "herdr_unavailable", Message: "Herdr request failed", Cause: err}
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
