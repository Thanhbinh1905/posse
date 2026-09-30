package app

import (
	"os"
	"syscall"
)

func inspectLookoutExecutable(pid int) (lookoutExecutableIdentity, bool) {
	path, ok := processExecutablePath(pid)
	if !ok || !lookoutExecutableVerifier(path) {
		return lookoutExecutableIdentity{}, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return lookoutExecutableIdentity{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 {
		return lookoutExecutableIdentity{}, false
	}
	return lookoutExecutableIdentity{Path: path, Device: uint64(stat.Dev), Inode: stat.Ino}, true
}
