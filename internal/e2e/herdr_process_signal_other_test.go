//go:build e2e && !linux

package e2e

import "syscall"

func signalIsolatedHerdrServer(isolatedHerdrServerProcess, syscall.Signal) error {
	return nil
}
