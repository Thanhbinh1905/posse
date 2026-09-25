package execgroup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const WaitDelay = 2 * time.Second

func Command(name string, args ...string) *exec.Cmd {
	return CommandContext(context.Background(), name, args...)
}

func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, name, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = WaitDelay
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return command
}
