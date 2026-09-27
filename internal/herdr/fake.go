package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

type CallRecord struct {
	Method string
	Params map[string]any
}

type Fake struct {
	mu         sync.Mutex
	Calls      []CallRecord
	Results    map[string]json.RawMessage
	Errors     map[string]error
	ErrorQueue map[string][]error
	// BeforeCall runs before each Call is recorded, without holding the lock,
	// so a test can change state concurrently with the method.
	BeforeCall    func(method string)
	RunOut        map[string][]byte
	Runs          []string
	RunError      error
	RunErrors     map[string]error
	Server        Status
	SnapshotValue Snapshot
}

func (f *Fake) Snapshot(context.Context) (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, CallRecord{Method: "session.snapshot", Params: map[string]any{}})
	if err := f.Errors["session.snapshot"]; err != nil {
		return Snapshot{}, err
	}
	if raw, ok := f.Results["session.snapshot"]; ok {
		return decodeSnapshot(raw)
	}
	return f.SnapshotValue, nil
}

func NewFake() *Fake {
	return &Fake{Results: map[string]json.RawMessage{}, Errors: map[string]error{}, ErrorQueue: map[string][]error{}, RunOut: map[string][]byte{"--version": []byte("herdr 0.9.1")}, RunErrors: map[string]error{}, Server: Status{Running: true, Protocol: MinimumProtocol, Compatible: true}}
}

func (f *Fake) Call(_ context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if f.BeforeCall != nil {
		f.BeforeCall(method)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	copyParams := make(map[string]any, len(params))
	for key, value := range params {
		copyParams[key] = value
	}
	f.Calls = append(f.Calls, CallRecord{Method: method, Params: copyParams})
	if !APIMethods[method] {
		return nil, &Error{Code: "invalid_request", Message: "unknown Herdr method " + method}
	}
	if queue := f.ErrorQueue[method]; len(queue) > 0 {
		f.ErrorQueue[method] = queue[1:]
		if queue[0] != nil {
			return nil, queue[0]
		}
	} else if err := f.Errors[method]; err != nil {
		return nil, err
	}
	if result, ok := f.Results[method]; ok {
		return append(json.RawMessage(nil), result...), nil
	}
	if method == "tab.create" {
		return json.RawMessage(`{"tab":{"tab_id":"fake:lookout"},"root_pane":{"pane_id":"fake:lookout-pane","tab_id":"fake:lookout"}}`), nil
	}
	if method == "worktree.open" {
		return json.RawMessage(`{"workspace":{"workspace_id":"fake:child"},"tab":{"tab_id":"fake:child:t1"},"root_pane":{"pane_id":"fake:child:p1","tab_id":"fake:child:t1"}}`), nil
	}
	return json.RawMessage(`{}`), nil
}

func (f *Fake) Status(context.Context) (Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Server, nil
}

func (f *Fake) CheckProtocol(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Server.Running {
		return &Error{Code: "herdr_unavailable", Message: "Herdr server is not running"}
	}
	if f.Server.Protocol < MinimumProtocol || !f.Server.Compatible {
		return &Error{Code: "herdr_protocol_incompatible", Message: "Herdr protocol is incompatible"}
	}
	return nil
}

func (f *Fake) Run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	command := strings.Join(args, " ")
	f.Runs = append(f.Runs, command)
	if f.RunError != nil {
		return nil, f.RunError
	}
	if err := f.RunErrors[command]; err != nil {
		return nil, err
	}
	if output, ok := f.RunOut[strings.Join(args, " ")]; ok {
		return append([]byte(nil), output...), nil
	}
	return nil, fmt.Errorf("unexpected fake Herdr CLI: %s", strings.Join(args, " "))
}

// RunCount counts CLI invocations whose joined arguments equal command.
func (f *Fake) RunCount(command string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, run := range f.Runs {
		if run == command {
			count++
		}
	}
	return count
}

func (f *Fake) CallCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.Calls {
		if call.Method == method {
			count++
		}
	}
	return count
}
