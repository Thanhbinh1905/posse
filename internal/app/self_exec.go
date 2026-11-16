package app

import (
	"fmt"
	"os"
	"os/exec"
)

// selfExecCommand creates a subprocess running this Posse executable.
// Tests replace it to keep detached self-execs inside the test process.
var selfExecCommand = func(args ...string) (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate Posse executable for self-exec: %w", err)
	}
	return exec.Command(executable, args...), nil
}
