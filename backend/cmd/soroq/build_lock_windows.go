//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Windows has no flock(2). syscall on Windows does not export LockFileEx either, and pulling in
// golang.org/x/sys purely for a build mutex would add a dependency to the public CLI module for one
// call site. Instead the lock is an exclusive-create sentinel: O_CREATE|O_EXCL succeeds for exactly
// one process, and the file is removed on release.
//
// The tradeoff is stated rather than hidden: a hard-killed process can leave the sentinel behind,
// where the unix flock would have been dropped by the kernel. The wait is therefore bounded and the
// timeout message names the file to delete, so the failure mode is a legible instruction rather than
// an indefinite hang. Windows remains an experimental target for Soroq.
func acquireProjectBuildLock(projectDir string) (func(), error) {
	dir := filepath.Join(projectDir, ".soroq")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "build.lock")

	const timeout = 10 * time.Minute
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("open build lock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf(
				"another Soroq build has held the lock in %s for over %s; if no build is running, delete %s",
				projectDir, timeout, path)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
