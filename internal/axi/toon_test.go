package axi

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOfficialEncodeFixtures(t *testing.T) {
	directory := filepath.Join("..", "..", "testdata", "toon", "encode")
	files, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	type fixture struct {
		Tests []struct {
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
			Expected string          `json:"expected"`
			Options  struct {
				Delimiter  string `json:"delimiter"`
				IndentSize int    `json:"indentSize"`
			} `json:"options"`
		} `json:"tests"`
	}
	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
			continue
		}
		t.Run(file.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(directory, file.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var cases fixture
			if err := json.Unmarshal(data, &cases); err != nil {
				t.Fatal(err)
			}
			for i, testCase := range cases.Tests {
				t.Run(testCase.Name, func(t *testing.T) {
					value, err := ParseOrderedJSON(bytes.NewReader(testCase.Input))
					if err != nil {
						t.Fatal(err)
					}
					options := ToonOptions{IndentSize: testCase.Options.IndentSize}
					if testCase.Options.Delimiter != "" {
						options.Delimiter = []rune(testCase.Options.Delimiter)[0]
					}
					actual, err := EncodeWithOptions(value, options)
					if err != nil {
						t.Fatalf("fixture %d: %v", i, err)
					}
					if actual != testCase.Expected {
						t.Fatalf("fixture %d mismatch\nwant:\n%s\ngot:\n%s", i, testCase.Expected, actual)
					}
					if strings.HasSuffix(actual, "\n") {
						t.Fatal("output has a trailing newline")
					}
				})
			}
		})
	}
}

func TestEncodeNamedPrimitivesAndTypedMaps(t *testing.T) {
	type name string
	type count int
	type enabled bool
	type key string
	type flag bool

	got, err := Encode(Object{
		{Key: "name", Value: name("lead")},
		{Key: "count", Value: count(2)},
		{Key: "enabled", Value: enabled(true)},
		{Key: "flags", Value: map[key]flag{"claude": true, "codex": false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "name: lead\ncount: 2\nenabled: true\nflags:\n  claude: true\n  codex: false"
	if got != want {
		t.Fatalf("TOON = %q, want %q", got, want)
	}
}

func TestEncodeStructsOmitEmptyFieldsLikeJSON(t *testing.T) {
	type row struct {
		ID      string   `json:"id"`
		Profile string   `json:"profile,omitempty"`
		Tags    []string `json:"tags,omitempty"`
		Count   int      `json:"count"`
	}
	got, err := Encode(Object{{Key: "tasks", Value: []row{{ID: "t1", Tags: []string{}}, {ID: "t2"}}}})
	if err != nil {
		t.Fatal(err)
	}
	want := "tasks[2]{id,count}:\n  t1,0\n  t2,0"
	if got != want {
		t.Fatalf("TOON = %q, want %q", got, want)
	}
}
