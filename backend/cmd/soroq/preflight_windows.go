//go:build windows

package main

import "errors"

// availableBytesStatfs is a dependency-free stub on windows (a PENDING platform): the free-disk
// preflight is skipped there. runInstallPreflight treats a statfs error as non-fatal (it prints
// "free: unknown" and does NOT abort), so windows setup simply proceeds without the pre-download
// disk check. Adding the real GetDiskFreeSpaceEx path would require golang.org/x/sys/windows, which
// the public export module must NOT carry (only kr/binarydist), so this returns an error instead.
func availableBytesStatfs(dir string) (int64, error) {
	return 0, errors.New("free-disk preflight unavailable on windows")
}
