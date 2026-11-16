//go:build !linux

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/thanhbinh1905/posse/internal/store"
)

func systemdWatchUnitName(home string, projectID int64) string {
	sum := sha256.Sum256([]byte(filepath.Clean(home)))
	return fmt.Sprintf("posse-watch-%s-%d.service", hex.EncodeToString(sum[:6]), projectID)
}

func systemdUserManagerAvailable(context.Context) bool     { return false }
func systemdWatchPID(context.Context, string) (int, error) { return 0, nil }
func startSystemdWatch(context.Context, string, store.Project, string, string, int64, string) (int, error) {
	return 0, nil
}
func stopSystemdWatch(context.Context, string, int64) error      { return nil }
func systemdWatchUnitActive(context.Context, string, int64) bool { return false }
