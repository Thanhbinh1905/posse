package herdr

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"
)

func TestFakeRejectsMethodsRealHerdrLacks(t *testing.T) {
	fake := NewFake()
	for _, method := range []string{"agent.interrupt", "agent.exit"} {
		_, err := fake.Call(context.Background(), method, map[string]any{})
		if failure, ok := err.(*Error); !ok || failure.Code != "invalid_request" {
			t.Errorf("fake %s error = %v, want invalid_request", method, err)
		}
	}
	if _, err := fake.Call(context.Background(), "pane.process_info", map[string]any{}); err != nil {
		t.Fatalf("fake rejected a real method: %v", err)
	}
}

func TestAPIMethodsExistInInstalledHerdr(t *testing.T) {
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr is not installed")
	}
	output, err := exec.Command("herdr", "api", "schema", "--json").Output()
	if err != nil {
		t.Fatalf("herdr api schema --json: %v", err)
	}
	var schema struct {
		Schemas struct {
			Request struct {
				OneOf []struct {
					Properties struct {
						Method struct {
							Const string `json:"const"`
						} `json:"method"`
					} `json:"properties"`
				} `json:"oneOf"`
			} `json:"request"`
		} `json:"schemas"`
	}
	if err := json.Unmarshal(output, &schema); err != nil {
		t.Fatalf("decode Herdr schema: %v", err)
	}
	real := map[string]bool{}
	for _, request := range schema.Schemas.Request.OneOf {
		real[request.Properties.Method.Const] = true
	}
	for method := range APIMethods {
		if !real[method] {
			t.Errorf("APIMethods lists %s, which the installed Herdr does not have", method)
		}
	}
}
