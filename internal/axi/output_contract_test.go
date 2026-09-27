package axi

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCLIOutputsSnakeCaseAndLocalISOTime(t *testing.T) {
	commands := &Command{Handler: func(ctx *Context, _ []string) error {
		return ctx.Print(Object{{Key: "TaskID", Value: "t8"}, {Key: "NestedRows", Value: []any{struct {
			TaskID    int64
			PRURL     string
			CreatedAt int64
		}{8, "https://example.org/8", 1735689600000}}}, {Key: "updated_at", Value: int64(1735689600000)}})
	}}
	for _, args := range [][]string{{}, {"--json"}} {
		var output bytes.Buffer
		app := App{Name: "test", Root: commands, Out: &output}
		if code := app.Run(args); code != 0 {
			t.Fatalf("%v: code %d", args, code)
		}
		text := output.String()
		for _, key := range []string{"task_id", "nested_rows", "pr_url", "created_at", "updated_at"} {
			if !strings.Contains(text, key) {
				t.Errorf("%v missing %s: %s", args, key, text)
			}
		}
		if strings.Contains(text, "TaskID") || strings.Contains(text, "CreatedAt") || strings.Contains(text, "1735689600000") {
			t.Errorf("%v not normalized: %s", args, text)
		}
		expected := time.UnixMilli(1735689600000).Local().Format(time.RFC3339Nano)
		if !strings.Contains(text, expected) {
			t.Errorf("%v local time %q missing: %s", args, expected, text)
		}
		if len(args) != 0 {
			var decoded map[string]any
			if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if _, ok := decoded["task_id"]; !ok {
				t.Fatal(decoded)
			}
		}
	}
}
