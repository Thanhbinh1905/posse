//go:build darwin

package app

import (
	"os/exec"
	"strconv"
	"strings"
)

func processExecutablePath(pid int) (string, bool) {
	output, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-Fn").Output()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "n") && len(line) > 1 {
			return line[1:], true
		}
	}
	return "", false
}
