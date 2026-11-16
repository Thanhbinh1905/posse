//go:build linux

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/thanhbinh1905/posse/internal/store"
)

func systemdWatchUnitName(home string, projectID int64) string {
	sum := sha256.Sum256([]byte(filepath.Clean(home)))
	return fmt.Sprintf("posse-watch-%s-%d.service", hex.EncodeToString(sum[:6]), projectID)
}

func systemdUserManagerAvailable(ctx context.Context) bool {
	command := exec.CommandContext(ctx, "systemctl", "--user", "show-environment")
	return command.Run() == nil
}

var startSystemdWatchUnit = startSystemdWatch

func startSystemdWatch(ctx context.Context, home string, project store.Project, binary, token string, startedAt int64, logPath string) error {
	unit := systemdWatchUnitName(home, project.ID)
	args := []string{"--user", "--quiet", "--unit", unit, "--collect", "--property=Restart=always", "--property=RestartSec=1s", "--property=MemoryMax=2G", "--property=TasksMax=256", "--property=KillMode=control-group", "--property=WorkingDirectory=" + project.Root, "--property=StandardOutput=append:" + logPath, "--property=StandardError=append:" + logPath}
	for _, entry := range watchRuntimeEnvironment(os.Environ(), home, project, token, startedAt) {
		args = append(args, "--setenv="+entry)
	}
	args = append(args, "--", binary, "watch", "--poll-only")
	output, err := exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("start Project Watch systemd unit: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func stopSystemdWatch(ctx context.Context, home string, projectID int64) error {
	unit := systemdWatchUnitName(home, projectID)
	command := exec.CommandContext(ctx, "systemctl", "--user", "stop", unit)
	output, err := command.CombinedOutput()
	if err != nil {
		text := string(output)
		if strings.Contains(text, "not loaded") || strings.Contains(text, "not-found") {
			return nil
		}
		return fmt.Errorf("stop Project Watch systemd unit: %w: %s", err, strings.TrimSpace(text))
	}
	return nil
}

func systemdWatchUnitLoaded(ctx context.Context, home string, projectID int64) bool {
	output, err := exec.CommandContext(ctx, "systemctl", "--user", "show", "--property=LoadState", "--value", systemdWatchUnitName(home, projectID)).Output()
	return err == nil && strings.TrimSpace(string(output)) == "loaded"
}

func systemdWatchUnitActive(ctx context.Context, home string, projectID int64) bool {
	return exec.CommandContext(ctx, "systemctl", "--user", "is-active", "--quiet", systemdWatchUnitName(home, projectID)).Run() == nil
}
