package main

import (
	"fmt"
	"runtime"
)

// currentHostOS is a test seam. The signed catalog currently resolves platform targets (android/iOS)
// but does not yet select a host-specific frontend/toolchain build. Until Windows-host artifacts are
// published, fail before fetching a multi-gigabyte macOS-host archive.
var currentHostOS = runtime.GOOS

func requireBuildArtifactsForHost() error {
	if currentHostOS != "windows" {
		return nil
	}
	return fmt.Errorf("Windows CLI beta does not yet include Windows-host Soroq Flutter frontend/toolchain artifacts; no download was started. Login, whoami, status, and control-plane commands are available, but release/patch builds still require a supported host. See https://docs.soroq.dev/compatibility")
}
