//go:build !linux && !darwin

package app

func processExecutablePath(int) (string, bool) {
	return "", false
}
