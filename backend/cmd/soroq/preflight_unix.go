//go:build !windows

package main

import "syscall"

// availableBytesStatfs returns Bavail*Bsize for the filesystem backing dir (darwin/linux, dep-free
// syscall). The uint64 casts keep it portable across the two platforms (darwin Bsize is uint32, linux
// int64); this repo targets macOS + Linux only.
func availableBytesStatfs(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize)), nil
}
