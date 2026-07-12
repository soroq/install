package main

// Shared install preflight + download-progress UX for `soroq frontend install` and
// `soroq toolchain install` (P5 W-2). Everything here writes to STDERR only — never STDOUT — so a
// machine-readable `--json` report on stdout is never corrupted. Live progress is additionally
// suppressed when stdout is machine-mode (--json) or stderr is not a TTY.

import (
	"fmt"
	"io"
	"os"
	"time"
)

// --- free-disk preflight ---

// availableBytesFn resolves the bytes available to an unprivileged user on the filesystem backing dir.
// It is a var so a test can inject a tiny value to exercise the abort path without a real full disk.
// availableBytesStatfs is defined per-platform: a dep-free syscall.Statfs impl on darwin/linux
// (preflight_unix.go) and a skip-the-check stub on windows (preflight_windows.go).
var availableBytesFn = availableBytesStatfs

// requiredPeakBytes is the PEAK on-disk footprint of an install: the downloaded temp archive AND the
// extracted temp dir coexist (the swap into the version dir happens ONLY after verify passes), so the
// new free-space demand is compressed + uncompressed. When the manifest does not expose an uncompressed
// size we fall back to a conservative 3x the compressed size (these gzip'd bundles expand ~2-3x). NOTE:
// the frontend manifest (frontend_cmd.go:71) and the toolchain manifest (toolchain_cmd.go:148) both
// expose uncompressed_bytes, so the 3x branch is only hit by size-less fixtures, not the real registry.
func requiredPeakBytes(compressed, uncompressed int64) int64 {
	if compressed <= 0 {
		return 0 // unknown compressed size => cannot size the check; skip it (see runInstallPreflight)
	}
	if uncompressed > 0 {
		return compressed + uncompressed
	}
	return compressed * 3
}

// preflightInfo is the input to runInstallPreflight, gathered from the signed manifest BEFORE any
// download begins.
type preflightInfo struct {
	label             string // "frontend" | "toolchain" (for messages)
	version           string
	destDir           string // the versionDir the install will land in
	checkDir          string // the (already-created) install root to statfs
	compressedBytes   int64
	uncompressedBytes int64
	force             bool
}

// runInstallPreflight prints the preflight block to STDERR and ABORTS (returning an error, before any
// byte is downloaded) when the filesystem backing checkDir has less free space than the PEAK install
// footprint. A statfs failure is non-fatal (a working disk must not be blocked by an introspection error).
func runInstallPreflight(pi preflightInfo) error {
	required := requiredPeakBytes(pi.compressedBytes, pi.uncompressedBytes)

	fmt.Fprintf(os.Stderr, "%s install preflight:\n", pi.label)
	fmt.Fprintf(os.Stderr, "  version:   %s\n", pi.version)
	fmt.Fprintf(os.Stderr, "  dest:      %s%s\n", pi.destDir, forceSuffix(pi.force))
	fmt.Fprintf(os.Stderr, "  archive:   %s (compressed)\n", humanBytes(pi.compressedBytes))
	if pi.uncompressedBytes > 0 {
		fmt.Fprintf(os.Stderr, "  extracted: %s (uncompressed)\n", humanBytes(pi.uncompressedBytes))
	}
	if required > 0 {
		fmt.Fprintf(os.Stderr, "  needs:     ~%s free (peak: temp archive + extracted temp dir)\n", humanBytes(required))
	}

	avail, err := availableBytesFn(pi.checkDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  free:      unknown (%v)\n", err)
		return nil
	}
	fmt.Fprintf(os.Stderr, "  free:      %s at %s\n", humanBytes(avail), pi.checkDir)
	if required > 0 && avail < required {
		return fmt.Errorf("insufficient disk space to install %s %s: need ~%s, have ~%s free at %s",
			pi.label, pi.version, humanBytes(required), humanBytes(avail), pi.checkDir)
	}
	return nil
}

func forceSuffix(force bool) string {
	if force {
		return "  (--force: will re-download)"
	}
	return ""
}

// --- live download progress ---

// stderrIsTTY reports whether STDERR is an interactive character device. Non-TTY (a pipe/file, e.g. under
// tests or when stderr is redirected) => live progress is suppressed so nothing spams captured output.
func stderrIsTTY() bool {
	info, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// progressReporter is an io.Writer sink handed to streamDownloadToFile's MultiWriter. It counts bytes
// and, when live, emits throttled (~5/sec) carriage-return progress to STDERR (bytes / % / speed / ETA).
// When NOT live (under --json or a non-TTY) it stays silent DURING the copy; the final one-line summary
// is printed by finish() either way. It NEVER writes to STDOUT.
type progressReporter struct {
	w        io.Writer
	total    int64
	live     bool
	start    time.Time
	written  int64
	lastEmit time.Time
}

func newProgressReporter(total int64, live bool) *progressReporter {
	return &progressReporter{w: os.Stderr, total: total, live: live, start: time.Now()}
}

func (p *progressReporter) Write(b []byte) (int, error) {
	n := len(b)
	p.written += int64(n)
	if p.live {
		now := time.Now()
		if now.Sub(p.lastEmit) >= 200*time.Millisecond { // throttle to ~5 updates/sec
			p.lastEmit = now
			p.render(now, false)
		}
	}
	return n, nil
}

func (p *progressReporter) render(now time.Time, final bool) {
	elapsed := now.Sub(p.start).Seconds()
	var rate float64
	if elapsed > 0 {
		rate = float64(p.written) / elapsed
	}
	line := fmt.Sprintf("  downloading %s", humanBytes(p.written))
	if p.total > 0 {
		pct := float64(p.written) * 100 / float64(p.total)
		line += fmt.Sprintf(" / %s (%.0f%%)", humanBytes(p.total), pct)
		if rate > 0 {
			remain := float64(p.total-p.written) / rate
			line += fmt.Sprintf("  %s/s  ETA %s", humanBytes(int64(rate)), humanDuration(remain))
		}
	} else if rate > 0 {
		line += fmt.Sprintf("  %s/s", humanBytes(int64(rate)))
	}
	suffix := "\r"
	if final {
		suffix = "\n"
	}
	fmt.Fprintf(p.w, "\r%-72s%s", line, suffix)
}

// finish prints the final "downloaded X in Ys" summary to STDERR (called only on a successful download).
func (p *progressReporter) finish() {
	now := time.Now()
	if p.live {
		fmt.Fprintf(p.w, "\r%-72s\r", "") // clear the last \r-updated line
	}
	fmt.Fprintf(p.w, "  downloaded %s in %s\n", humanBytes(p.written), humanDuration(now.Sub(p.start).Seconds()))
}

// humanDuration renders a seconds count as ms / s / m + s for the ETA + final-summary lines.
func humanDuration(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	d := time.Duration(seconds * float64(time.Second))
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int((d%time.Minute)/time.Second))
	}
}
