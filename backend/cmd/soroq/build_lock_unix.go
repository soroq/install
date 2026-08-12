//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireProjectBuildLock takes an exclusive, advisory lock for projectDir, held for the whole
// injection + overlay-install sequence. It returns a release function.
//
// The lock file lives under .soroq/ (git-ignored) and is released by close, so a crashed process does
// NOT leave a stale lock behind — the kernel drops the flock when the file descriptor closes.
func acquireProjectBuildLock(projectDir string) (func(), error) {
	f, err := openProjectBuildLockFile(projectDir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("another Soroq build is already running in %s (%w)", projectDir, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func openProjectBuildLockFile(projectDir string) (*os.File, error) {
	dir := filepath.Join(projectDir, ".soroq")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "build.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open build lock %s: %w", path, err)
	}
	return f, nil
}
