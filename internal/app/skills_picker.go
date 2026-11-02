package app

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

type skillChoice struct {
	role       string
	name       string
	summary    string
	configured bool
	selected   bool
}

type skillPicker struct {
	choices []skillChoice
	cursor  int
	target  string
	width   int
	height  int
}

const (
	pickerEscapeTimeout = 75 * time.Millisecond
	pickerNameWidth     = 26
	pickerChromeLines   = 6
	pickerMinListLines  = 5
	pickerDefaultWidth  = 80
	pickerDefaultHeight = 24
)

// runSkillPicker draws the picker on out and reads keys from in until the User
// saves or quits. It returns the toggled choices and whether they were saved.
func runSkillPicker(in io.Reader, out io.Writer, picker skillPicker) ([]skillChoice, bool, error) {
	if picker.width <= 0 {
		picker.width = pickerDefaultWidth
	}
	if picker.height <= 0 {
		picker.height = pickerDefaultHeight
	}
	picker.cursor = picker.firstToggleable()
	reader := bufio.NewReader(in)
	waitForEscape := func() (bool, error) { return true, nil }
	if waiter, ok := in.(interface{ waitForPickerEscape() (bool, error) }); ok {
		waitForEscape = waiter.waitForPickerEscape
	} else if file, ok := in.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		waitForEscape = func() (bool, error) { return waitForPickerByte(file.Fd(), pickerEscapeTimeout) }
	}
	frameLines := 0
	draw := func(final bool) error {
		var frame strings.Builder
		if frameLines > 0 {
			fmt.Fprintf(&frame, "\x1b[%dA\r\x1b[J", frameLines)
		}
		lines := []string{}
		if !final {
			lines = picker.frame()
		}
		for _, line := range lines {
			frame.WriteString(line + "\x1b[K\r\n")
		}
		frameLines = len(lines)
		_, err := io.WriteString(out, frame.String())
		return err
	}
	if _, err := io.WriteString(out, "\x1b[?25l"); err != nil {
		return nil, false, err
	}
	defer io.WriteString(out, "\x1b[?25h")
	for {
		if err := draw(false); err != nil {
			return nil, false, err
		}
		key, err := readPickerKey(reader, waitForEscape)
		if err != nil {
			if err == io.EOF {
				return nil, false, draw(true)
			}
			return nil, false, err
		}
		switch key {
		case "up", "k":
			picker.move(-1)
		case "down", "j":
			picker.move(1)
		case " ":
			picker.toggle()
		case "a":
			picker.toggleAll()
		case "enter":
			return picker.choices, true, draw(true)
		case "q", "ctrl-c", "ctrl-d", "esc":
			return nil, false, draw(true)
		}
	}
}

func readPickerKey(reader *bufio.Reader, waitForEscape func() (bool, error)) (string, error) {
	b, err := reader.ReadByte()
	if err != nil {
		return "", err
	}
	switch b {
	case '\r', '\n':
		return "enter", nil
	case 3:
		return "ctrl-c", nil
	case 4:
		return "ctrl-d", nil
	case 0x1b:
		next, ready, err := readPickerContinuation(reader, waitForEscape)
		if err != nil {
			if err == io.EOF {
				return "esc", nil
			}
			return "", err
		}
		if !ready || next != '[' && next != 'O' {
			return "esc", nil
		}
		final, ready, err := readPickerContinuation(reader, waitForEscape)
		if err != nil {
			if err == io.EOF {
				return "", nil
			}
			return "", err
		}
		if !ready {
			return "", nil
		}
		switch final {
		case 'A':
			return "up", nil
		case 'B':
			return "down", nil
		}
		return "", nil
	}
	return string(rune(b)), nil
}

func readPickerContinuation(reader *bufio.Reader, waitForEscape func() (bool, error)) (byte, bool, error) {
	if reader.Buffered() == 0 {
		ready, err := waitForEscape()
		if err != nil || !ready {
			return 0, ready, err
		}
	}
	value, err := reader.ReadByte()
	return value, err == nil, err
}

func (p *skillPicker) firstToggleable() int {
	for i, choice := range p.choices {
		if !choice.configured {
			return i
		}
	}
	return 0
}

func (p *skillPicker) move(step int) {
	for next := p.cursor + step; next >= 0 && next < len(p.choices); next += step {
		if !p.choices[next].configured {
			p.cursor = next
			return
		}
	}
}

func (p *skillPicker) toggle() {
	if p.cursor < len(p.choices) && !p.choices[p.cursor].configured {
		p.choices[p.cursor].selected = !p.choices[p.cursor].selected
	}
}

func (p *skillPicker) toggleAll() {
	all := true
	for _, choice := range p.choices {
		all = all && (choice.configured || choice.selected)
	}
	for i := range p.choices {
		if !p.choices[i].configured {
			p.choices[i].selected = !all
		}
	}
}

func (p *skillPicker) counts() (selected, configured int) {
	for _, choice := range p.choices {
		if choice.configured {
			configured++
		} else if choice.selected {
			selected++
		}
	}
	return selected, configured
}

func (p *skillPicker) frame() []string {
	list := []string{}
	cursorLine := 0
	role := ""
	for i, choice := range p.choices {
		if choice.role != role {
			role = choice.role
			list = append(list, role)
		}
		pointer, mark := "  ", "[ ]"
		switch {
		case choice.configured:
			mark = "[+]"
		case choice.selected:
			mark = "[x]"
		}
		if i == p.cursor {
			pointer = "> "
			cursorLine = len(list)
		}
		list = append(list, truncateLine(fmt.Sprintf("%s%s %-*s %s", pointer, mark, pickerNameWidth, choice.name, choice.summary), p.width-1))
	}
	room := max(p.height-pickerChromeLines, pickerMinListLines)
	if len(list) > room {
		start := min(max(cursorLine-room/2, 0), len(list)-room)
		list = list[start : start+room]
	}
	selected, configured := p.counts()
	frame := []string{truncateLine("Add Posse skills to "+p.target, p.width-1), truncateLine("up/down move  space toggle  a all  enter save  q quit  [+] already in config", p.width-1), ""}
	frame = append(frame, list...)
	return append(frame, "", fmt.Sprintf("%d selected, %d already in config", selected, configured))
}

func truncateLine(line string, width int) string {
	runes := []rune(line)
	if width <= 3 || len(runes) <= width {
		return line
	}
	return string(runes[:width-3]) + "..."
}
