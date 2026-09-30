package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"soroq/backend/internal/fontglyphs"
)

// ICON GLYPHS A PATCH CANNOT DELIVER.
//
// Store builds tree-shake icon fonts like any Flutter release build: the MaterialIcons font a device has
// draws only the icons the store build used. A patch carries code, never a font, so a patch that starts
// using an icon the store build did not would draw a blank box on every device. That is refused here, on
// both platforms, naming the code points -- look them up in Icons (e.g. U+E145 is Icons.add) -- so the
// change can ship with the next store release or use an icon the app already has.

func refuseMissingIconGlyphs(missing map[string][]rune) error {
	if len(missing) == 0 {
		return nil
	}
	paths := make([]string, 0, len(missing))
	for p := range missing {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var lines []string
	for _, p := range paths {
		lines = append(lines, fmt.Sprintf("%s: %s", p, fontglyphs.FormatCodepoints(missing[p], 12)))
	}
	return fmt.Errorf("patch refused: it uses icon(s) the store build's font does not contain, and a patch "+
		"cannot deliver a font, so they would render as blank boxes:\n  - %s\n"+
		"Use an icon the app already uses, or ship this change in a new store release (its build will "+
		"include the new icons)", strings.Join(lines, "\n  - "))
}

// soroqBuildFailure is a failed build that keeps its output, so a caller can tell why it failed.
type soroqBuildFailure struct {
	err    error
	output []byte
}

func (f *soroqBuildFailure) Error() string { return f.err.Error() }
func (f *soroqBuildFailure) Unwrap() error { return f.err }

// flutterIconTreeShakeRefusal is how Flutter refuses to shake icon fonts: the app builds IconData from
// non-constant values, so the icons it will draw are not knowable at build time
// (flutter_tools/lib/src/build_system/targets/icon_tree_shaker.dart).
const flutterIconTreeShakeRefusal = "cannot tree shake icons fonts"

// iconTreeShakeFallbackArgs reports whether a failed build failed only because Flutter could not shake
// its icon fonts, and if so returns the args to rebuild with the full fonts. A caller that chose a
// tree-shake policy explicitly gets its own failure back, unchanged.
func iconTreeShakeFallbackArgs(buildErr error, args []string) ([]string, bool) {
	var failure *soroqBuildFailure
	if buildErr == nil || !errors.As(buildErr, &failure) || !bytes.Contains(failure.output, []byte(flutterIconTreeShakeRefusal)) {
		return nil, false
	}
	if hasFlutterFlag(args, "--tree-shake-icons") || hasFlutterFlag(args, "--no-tree-shake-icons") {
		return nil, false
	}
	fmt.Fprintln(os.Stderr, "NOTICE: this app builds IconData from non-constant values, so Flutter cannot tree-shake "+
		"its icon fonts; building with the full fonts instead (larger download, and every icon stays available to patches)")
	return append(append([]string{}, args...), "--no-tree-shake-icons"), true
}

// freehandBaseIconFontsDir holds, beside an iOS baseline, the font files the store build shipped. It is
// local like the object graph: never uploaded, and not part of the immutable baseline's identity. A
// baseline without it predates tree-shaken icon fonts -- its store build shipped the full fonts.
const freehandBaseIconFontsDir = "base_icon_fonts"

// builtIOSFlutterAssets is where the built app keeps its Flutter assets.
func builtIOSFlutterAssets(projectDir string) (string, error) {
	app, err := locateBuiltIOSApp(projectDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(app, "Frameworks", "App.framework", "flutter_assets"), nil
}

// readFontFiles returns every .otf/.ttf under dir, keyed by its slash path relative to dir. A missing dir
// is not an error: it yields nil.
func readFontFiles(dir string) (map[string][]byte, error) {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		lower := strings.ToLower(path)
		if !strings.HasSuffix(lower, ".otf") && !strings.HasSuffix(lower, ".ttf") {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	return out, err
}

// keepFreehandBaseIconFonts copies the fonts the store build just shipped beside its baseline, for the
// patch-time icon check.
func keepFreehandBaseIconFonts(projectDir, relDir string) error {
	assets, err := builtIOSFlutterAssets(projectDir)
	if err != nil {
		return err
	}
	fonts, err := readFontFiles(assets)
	if err != nil {
		return err
	}
	dst := filepath.Join(relDir, freehandBaseIconFontsDir)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	for rel, b := range fonts {
		p := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
	}
	return os.MkdirAll(dst, 0o755) // present even for an app without fonts: the check then has a base
}

// missingIconGlyphs compares candidate fonts with base fonts by the code points they draw. A candidate
// font the base does not ship at all is missing every glyph it draws. A font either side cannot read is
// an error: a check that cannot see a font must not pass it.
func missingIconGlyphs(base, candidate map[string][]byte) (map[string][]rune, error) {
	out := map[string][]rune{}
	for rel, cb := range candidate {
		cand, err := fontglyphs.Codepoints(cb)
		if err != nil {
			return nil, fmt.Errorf("read candidate font %s: %w", rel, err)
		}
		baseCps := map[rune]bool{}
		if bb, ok := base[rel]; ok {
			if baseCps, err = fontglyphs.Codepoints(bb); err != nil {
				return nil, fmt.Errorf("read store-build font %s: %w", rel, err)
			}
		}
		if missing := fontglyphs.Missing(baseCps, cand); len(missing) > 0 {
			out[rel] = missing
		}
	}
	return out, nil
}

// assertFreehandIconGlyphs refuses an iOS patch whose app would draw an icon the store build's fonts do
// not contain. The candidate app is the one built during this plan (the machine-code comparison builds
// it); when none was, it is built here.
func assertFreehandIconGlyphs(projectDir, relDir string, since time.Time, opts []freehandPlanOptions) error {
	baseFonts, err := readFontFiles(filepath.Join(relDir, freehandBaseIconFontsDir))
	if err != nil {
		return fmt.Errorf("read the store build's fonts: %w", err)
	}
	if baseFonts == nil {
		return nil // the store build predates tree-shaken icon fonts: it ships every glyph
	}
	if len(opts) == 0 || strings.TrimSpace(opts[0].toolchain) == "" {
		return errors.New("this store build ships tree-shaken icon fonts, and checking the patch's icons " +
			"needs a candidate build (--toolchain)")
	}
	if !builtIOSAppNewerThan(projectDir, since) {
		build := opts[0].buildCandidateApp
		if build == nil {
			build = freehandBuildCandidateApp
		}
		if err := build(projectDir, opts[0].toolchain, relDir, opts[0].passthrough); err != nil {
			return err
		}
	}
	assets, err := builtIOSFlutterAssets(projectDir)
	if err != nil {
		return fmt.Errorf("find the candidate app's assets: %w", err)
	}
	candFonts, err := readFontFiles(assets)
	if err != nil {
		return fmt.Errorf("read the candidate app's fonts: %w", err)
	}
	missing, err := missingIconGlyphs(baseFonts, candFonts)
	if err != nil {
		return err
	}
	return refuseMissingIconGlyphs(missing)
}

// builtIOSAppNewerThan reports whether the built app's Dart code was produced after t.
func builtIOSAppNewerThan(projectDir string, t time.Time) bool {
	app, err := locateBuiltIOSApp(projectDir)
	if err != nil {
		return false
	}
	info, err := os.Stat(filepath.Join(app, "Frameworks", "App.framework", "App"))
	return err == nil && info.ModTime().After(t)
}
