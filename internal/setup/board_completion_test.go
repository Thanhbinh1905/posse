package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBoardRefreshExportsStartCompletionMarkers(t *testing.T) {
	root := t.TempDir()
	boardDir := filepath.Join(root, "board")
	binDir := filepath.Join(root, "bin")
	markerDir := filepath.Join(boardDir, "start-completions")
	for _, dir := range []string{boardDir, binDir, markerDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
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
	script, err := files.ReadFile("skills/posse-board/refresh.sh")
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(root, "refresh.sh")
	if err := os.WriteFile(scriptPath, script, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"posse": "#!/bin/sh\nprintf '{\"project\":{}}\\n'\n",
		"gh":    "#!/bin/sh\nprintf '[]\\n'\n",
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(markerDir, "request_123"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", scriptPath, boardDir)
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("refresh: %v: %s", err, output)
	}
	starts, err := os.ReadFile(filepath.Join(boardDir, "starts.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(starts), `posseBoardData("starts", ["request_123"]);`) {
		t.Fatalf("starts.js = %q, want the completion token", starts)
	}
}
