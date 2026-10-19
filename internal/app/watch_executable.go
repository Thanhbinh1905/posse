package app

import (
	"os"
	"syscall"
)

func inspectWatchExecutable(pid int) (watchExecutableIdentity, bool) {
	path, ok := processExecutablePath(pid)
	if !ok || !watchExecutableVerifier(path) {
		return watchExecutableIdentity{}, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return watchExecutableIdentity{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 {
		return watchExecutableIdentity{}, false
	}
	return watchExecutableIdentity{Path: path, Device: uint64(stat.Dev), Inode: stat.Ino}, true
}
