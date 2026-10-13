package herdr

import (
	"fmt"
	"os"
	"runtime"
	"testing"
)

func shortTestSocketDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS != "linux" {
		return dir
	}
	handle, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	return fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), handle.Fd())
}
