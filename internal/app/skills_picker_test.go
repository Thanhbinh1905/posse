package app

import (
	"bytes"
	"strings"
	"testing"
)

func testPickerChoices() []skillChoice {
	return []skillChoice{
		{role: "lead", name: "posse-spec", summary: "Turns a lock card into a spec"},
		{role: "lead", name: "posse-catch-up", summary: "Catches the User up", configured: true},
		{role: "change", name: "posse-tdd", summary: "Implements test-first"},
		{role: "review", name: "posse-review", summary: "Reviews a Change"},
	}
}

func TestSkillPickerTogglesMovesAndSkipsConfiguredEntries(t *testing.T) {
	var out bytes.Buffer
	choices, saved, err := runSkillPicker(strings.NewReader(" j\x1b[B \r"), &out, skillPicker{choices: testPickerChoices(), target: "~/config.toml"})
	if err != nil || !saved {
		t.Fatalf("saved = %v, err = %v", saved, err)
	}
	var selected []string
	for _, choice := range choices {
		if choice.selected {
			selected = append(selected, choice.name)
		}
	}
	if strings.Join(selected, ",") != "posse-spec,posse-review" {
		t.Fatalf("selected = %v, want posse-spec,posse-review after skipping the configured and change rows", selected)
	}
	for _, want := range []string{"Add Posse skills to ~/config.toml", "[+] posse-catch-up", "> [ ] posse-spec", "2 selected, 1 already in config"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("frames lack %q:\n%q", want, out.String())
		}
	}
}

func TestSkillPickerAllToggleAndQuit(t *testing.T) {
	choices, saved, err := runSkillPicker(strings.NewReader("a\r"), &bytes.Buffer{}, skillPicker{choices: testPickerChoices()})
	if err != nil || !saved || !choices[0].selected || choices[1].selected || !choices[2].selected || !choices[3].selected {
		t.Fatalf("a then enter: saved=%v err=%v choices=%+v", saved, err, choices)
	}
	choices, saved, err = runSkillPicker(strings.NewReader("aaq"), &bytes.Buffer{}, skillPicker{choices: testPickerChoices()})
	if err != nil || saved || choices != nil {
		t.Fatalf("q must discard: saved=%v err=%v choices=%+v", saved, err, choices)
	}
}

func TestSkillPickerFrameFitsTheTerminal(t *testing.T) {
	var choices []skillChoice
	for i := 0; i < 40; i++ {
		choices = append(choices, skillChoice{role: "lead", name: "posse-skill-with-a-long-name", summary: strings.Repeat("long summary ", 20)})
	}
	picker := skillPicker{choices: choices, cursor: 30, target: "~/config.toml", width: 60, height: 20}
	frame := picker.frame()
	if len(frame) > picker.height {
		t.Fatalf("frame has %d lines for a %d line terminal", len(frame), picker.height)
	}
	for _, line := range frame {
		if len([]rune(line)) > picker.width-1 {
			t.Fatalf("line wider than the terminal: %q", line)
		}
	}
	if !strings.Contains(strings.Join(frame, "\n"), "> [ ]") {
		t.Fatalf("cursor row scrolled out of view:\n%s", strings.Join(frame, "\n"))
	}
}
