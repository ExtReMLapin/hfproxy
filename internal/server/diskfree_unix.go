//go:build unix

package server

import "syscall"

// diskFree reports the bytes available to unprivileged writers under dir.
func diskFree(dir string) (uint64, bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, false
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), true
}
