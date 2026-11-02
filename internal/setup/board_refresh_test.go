package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBoardRefreshPreservesDataWhenPosseFails(t *testing.T) {
	root := t.TempDir()
	boardDir := filepath.Join(root, "board")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(boardDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"posse.js":  "previous posse data\n",
		"issues.js": "previous issues data\n",
	} {
		if err := os.WriteFile(filepath.Join(boardDir, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script, err := files.ReadFile("skills/posse-board/refresh.sh")
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(root, "refresh.sh")
	if err := os.WriteFile(scriptPath, script, 0o700); err != nil {
		t.Fatal(err)
	}
	page, err := files.ReadFile("skills/posse-board/board.html")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "board.html"), page, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(boardDir, "index.html"), page, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"posse": "#!/bin/sh\nexit 42\n",
		"gh":    "#!/bin/sh\nprintf '[]\\n'\n",
	} {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", scriptPath, boardDir)
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("refresh succeeded after posse failed: %s", output)
	}
	if !strings.Contains(string(output), "posse") {
		t.Fatalf("refresh error did not identify posse failure: %s", output)
	}
	for name, want := range map[string]string{
		"posse.js":  "previous posse data\n",
		"issues.js": "previous issues data\n",
	} {
		got, readErr := os.ReadFile(filepath.Join(boardDir, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want unchanged data %q", name, got, want)
		}
	}
}
