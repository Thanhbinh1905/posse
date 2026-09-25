//go:build !linux

package herdr

import "context"

func serverStartedAt(context.Context, string) string { return "" }
