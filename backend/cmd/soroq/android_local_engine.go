package main

// Android cached-toolchain -> Flutter `--local-engine` layout composition (T015).
//
// The hosted Android toolchain archive is deliberately MINIMAL: it carries only the SOROQ-under-test
// engine artifacts (the stripped libflutter.so + the universal host gen_snapshot) plus the patch-lane
// tools, stored FLAT under ~/.soroq/toolchains/<version>/android/. Flutter's `--local-engine` build,
// however, consumes an engine-source `out/{android_release_arm64,host_release_arm64}/...` tree PLUS the
// Android Gradle embedding Maven repo. T015 closes that gap WITHOUT hosting a 70-100GB engine checkout:
//
//   - `materializeAndroidLocalEngineLayout` (run by `soroq toolchain install`) maps the packed SOROQ
//     artifacts into the `out/` layout Flutter expects, and bakes the SOROQ libflutter.so into the
//     `arm64_v8a_release.jar` form Gradle's `--local-engine` Maven repo resolves. This is the owner's
//     "materialize a Flutter-compatible cached layout" fix. ONLY soroq-under-test bytes are placed here.
//   - `completeAndroidLocalEngineLayout` (run by the release/patch build path) overlays the
//     VERSION-MATCHED STOCK host tooling that the minimal pack omits — host dart-sdk + frontend_server,
//     flutter_patched_sdk{,_product}, icudtl.dat, const_finder, font-subset, and the stock
//     flutter_embedding_release Maven artifacts (pure Java embedding, NO native engine code) — sourced
//     from the resolved Soroq Flutter frontend's own bin/cache (and download.flutter.io for the
//     embedding, exactly as a normal Flutter build resolves it). The SOROQ engine bytes are NEVER taken
//     from the frontend cache, so the rendered/patched artifact is the toolchain under test, not stock.

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	androidLocalEngineTargetName = "android_release_arm64"
	androidLocalEngineHostName   = "host_release_arm64"
	androidLocalEngineABI        = "arm64_v8a"
)

// materializeAndroidLocalEngineLayout maps the FLAT packed soroq artifacts under androidBundleDir into
// the `out/` engine-source layout + the Gradle embedding jar. Idempotent. Only soroq-under-test bytes.
func materializeAndroidLocalEngineLayout(androidBundleDir string) error {
	flatLibflutter := filepath.Join(androidBundleDir, "libflutter.so")
	flatGenSnapshot := filepath.Join(androidBundleDir, "gen_snapshot")
	if _, err := os.Stat(flatLibflutter); err != nil {
		return fmt.Errorf("packed libflutter.so missing under %s: %w", androidBundleDir, err)
	}
	if _, err := os.Stat(flatGenSnapshot); err != nil {
		return fmt.Errorf("packed gen_snapshot missing under %s: %w", androidBundleDir, err)
	}
	targetOut := filepath.Join(androidBundleDir, "out", androidLocalEngineTargetName)
	// SOROQ device runtime: the stripped libflutter.so where Flutter's local-engine resolves it.
	if err := linkOrCopyFile(flatLibflutter, filepath.Join(targetOut, "lib.stripped", "libflutter.so")); err != nil {
		return fmt.Errorf("materialize lib.stripped/libflutter.so: %w", err)
	}
	if err := linkOrCopyFile(flatLibflutter, filepath.Join(targetOut, "libflutter.so")); err != nil {
		return fmt.Errorf("materialize libflutter.so: %w", err)
	}
	// SOROQ host AOT snapshotter (Flutter prefers universal/ for an android_ target on darwin).
	if err := linkOrCopyFile(flatGenSnapshot, filepath.Join(targetOut, "universal", "gen_snapshot")); err != nil {
		return fmt.Errorf("materialize universal/gen_snapshot: %w", err)
	}
	// SOROQ engine embedding jar (lib/<abi>/libflutter.so) for Gradle's --local-engine Maven repo.
	if err := writeAndroidEmbeddingJar(flatLibflutter, filepath.Join(targetOut, androidLocalEngineABI+"_release.jar")); err != nil {
		return fmt.Errorf("materialize %s_release.jar: %w", androidLocalEngineABI, err)
	}
	// T017: the SOROQ flutter_embedding_release.jar (the Java embedding carrying the setSoroq*
	// FlutterLoader override methods) placed at the FLAT location the Flutter tool reads when building
	// the --local-engine local Maven repo. `_getLocalEngineRepo` (flutter_tools/.../android/gradle.dart)
	// reads out/<target>/flutter_embedding_release.{pom,jar} and, in --local-engine mode, FlutterPlugin
	// wires ONLY that local repo (download.flutter.io is NOT added) — so this jar is what links into the
	// APK. T015 linked the STOCK jar here (no setSoroq*) => on-device apply failed
	// asset_bundle_override_api_missing. The jar is a SOROQ-under-test artifact from the PACK (SHA already
	// re-verified by verifyEngineBundle), so it belongs in this soroq-bytes-only materialize step.
	packedEmbeddingJar := filepath.Join(androidBundleDir, "flutter_embedding_release.jar")
	if _, err := os.Stat(packedEmbeddingJar); err != nil {
		return fmt.Errorf("packed flutter_embedding_release.jar missing under %s (re-pack the Android toolchain with the embedding jar): %w", androidBundleDir, err)
	}
	if err := assertSoroqEmbeddingJar(packedEmbeddingJar); err != nil {
		return err
	}
	if err := linkOrCopyFile(packedEmbeddingJar, filepath.Join(targetOut, "flutter_embedding_release.jar")); err != nil {
		return fmt.Errorf("materialize flutter_embedding_release.jar: %w", err)
	}
	return nil
}

// assertSoroqEmbeddingJar fails (fail-safe) unless the jar's FlutterLoader class carries the SOROQ
// override method the asset value-flip depends on. This catches a STOCK embedding being materialized
// (the exact T015 regression: a stock FlutterLoader has no setSoroq* and apply fails
// asset_bundle_override_api_missing). It decompresses the class entry (the names live in the class
// constant pool, not the raw deflate stream) and scans for the override method symbol.
func assertSoroqEmbeddingJar(jarPath string) error {
	const flutterLoaderClass = "io/flutter/embedding/engine/loader/FlutterLoader.class"
	const requiredOverride = "setSoroqStagedAssetBundlePath"
	zr, err := zip.OpenReader(jarPath)
	if err != nil {
		return fmt.Errorf("open embedding jar %s: %w", jarPath, err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != flutterLoaderClass {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("read %s in %s: %w", flutterLoaderClass, jarPath, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("read %s in %s: %w", flutterLoaderClass, jarPath, err)
		}
		if bytes.Contains(data, []byte(requiredOverride)) {
			return nil
		}
		return fmt.Errorf("embedding jar %s carries a FlutterLoader WITHOUT %s — this is a STOCK embedding, not the SOROQ override jar (on-device apply would fail asset_bundle_override_api_missing)", jarPath, requiredOverride)
	}
	return fmt.Errorf("embedding jar %s does not contain %s (not a valid Android embedding jar)", jarPath, flutterLoaderClass)
}

// completeAndroidLocalEngineLayout overlays the version-matched STOCK host tooling + stock embedding
// Maven artifacts that the minimal pack omits, sourced from the resolved Soroq Flutter frontend (and
// download.flutter.io for the embedding). It NEVER overwrites the soroq engine bytes. Returns nil only
// when the full Flutter-buildable layout is present.
func completeAndroidLocalEngineLayout(androidBundleDir, flutterBin string) error {
	if err := materializeAndroidLocalEngineLayout(androidBundleDir); err != nil {
		return err
	}
	flutterRoot, err := flutterRootFromBin(flutterBin)
	if err != nil {
		return err
	}
	cacheDir := filepath.Join(flutterRoot, "bin", "cache")
	commonEngine := filepath.Join(cacheDir, "artifacts", "engine", "common")
	hostEngine, err := resolveFrontendHostEngineDir(cacheDir)
	if err != nil {
		return err
	}
	targetOut := filepath.Join(androidBundleDir, "out", androidLocalEngineTargetName)
	hostOut := filepath.Join(androidBundleDir, "out", androidLocalEngineHostName)

	// Stock, version-matched host tooling (NOT soroq-specific; the pack deliberately omits these).
	stockLinks := map[string]string{
		filepath.Join(targetOut, "flutter_patched_sdk"):         filepath.Join(commonEngine, "flutter_patched_sdk"),
		filepath.Join(targetOut, "flutter_patched_sdk_product"): filepath.Join(commonEngine, "flutter_patched_sdk_product"),
		filepath.Join(targetOut, "icudtl.dat"):                  filepath.Join(hostEngine, "icudtl.dat"),
		filepath.Join(hostOut, "dart-sdk"):                      filepath.Join(cacheDir, "dart-sdk"),
		filepath.Join(hostOut, "flutter_patched_sdk"):           filepath.Join(commonEngine, "flutter_patched_sdk"),
		filepath.Join(hostOut, "gen", "const_finder.dart.snapshot"): filepath.Join(hostEngine, "const_finder.dart.snapshot"),
		filepath.Join(hostOut, "font-subset"):                       filepath.Join(hostEngine, "font-subset"),
	}
	for dst, src := range stockLinks {
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("stock frontend artifact missing (run the Soroq frontend's `flutter precache --android` once): %s: %w", src, err)
		}
		if err := symlinkForce(src, dst); err != nil {
			return fmt.Errorf("link stock %s: %w", filepath.Base(dst), err)
		}
	}

	// Stock Android Gradle embedding Maven repo (flutter_embedding_release: pure Java, no native engine).
	if err := materializeStockAndroidEmbedding(cacheDir, targetOut); err != nil {
		return err
	}
	return nil
}

// materializeStockAndroidEmbedding fills the VERSION + DEPENDENCY metadata around the SOROQ embedding
// jar: it fetches the version-matched flutter_embedding_release.pom from the engine Maven host
// (download.flutter.io) and writes the maven-metadata + arm64 pom so the Flutter tool's --local-engine
// local Maven repo resolves. ONLY the pom (the androidx dependency list `_getLocalArtifactVersion` reads
// the artifact version from) + maven-metadata are stock; the flutter_embedding_release.jar BYTES are the
// SOROQ embedding (setSoroq* overrides), placed + asserted by materializeAndroidLocalEngineLayout. The
// arm64_v8a_release.jar (soroq libflutter.so) is likewise already in place. T017: NO stock jar download —
// linking a stock embedding here is the exact T015 regression, so the jar is required to be the SOROQ one.
func materializeStockAndroidEmbedding(cacheDir, targetOut string) error {
	stamp, err := readTrimmedFile(filepath.Join(cacheDir, "engine.stamp"))
	if err != nil {
		return fmt.Errorf("read engine.stamp: %w", err)
	}
	realm, _ := readTrimmedFile(filepath.Join(cacheDir, "engine.realm"))
	version := "1.0.0-" + stamp
	host := strings.TrimSpace(os.Getenv("FLUTTER_STORAGE_BASE_URL"))
	if host == "" {
		host = "https://storage.googleapis.com"
	}
	base := strings.TrimRight(host, "/") + "/"
	if r := strings.TrimSpace(realm); r != "" {
		base += r + "/"
	}
	base += "download.flutter.io/io/flutter"

	embeddingPom := filepath.Join(targetOut, "flutter_embedding_release.pom")
	embeddingJar := filepath.Join(targetOut, "flutter_embedding_release.jar")
	if !fileExists(embeddingPom) {
		if err := downloadToFile(fmt.Sprintf("%s/flutter_embedding_release/%s/flutter_embedding_release-%s.pom", base, version, version), embeddingPom); err != nil {
			return fmt.Errorf("download flutter_embedding_release.pom (dependency metadata only): %w", err)
		}
	}
	// The jar MUST be the SOROQ embedding placed (and setSoroq*-asserted) by
	// materializeAndroidLocalEngineLayout, which runs first. Re-assert here as a fail-safe so a stale
	// stock jar can never silently win the --local-engine link (the T015 regression).
	if !fileExists(embeddingJar) {
		return fmt.Errorf("SOROQ flutter_embedding_release.jar missing at %s (materializeAndroidLocalEngineLayout must run first)", embeddingJar)
	}
	if err := assertSoroqEmbeddingJar(embeddingJar); err != nil {
		return err
	}
	// arm64 pom (small; synthesize to match the soroq jar's version).
	if err := os.WriteFile(filepath.Join(targetOut, androidLocalEngineABI+"_release.pom"), []byte(androidEmbeddingPOM(androidLocalEngineABI+"_release", version)), 0o644); err != nil {
		return err
	}
	for _, artifact := range []string{"flutter_embedding_release", androidLocalEngineABI + "_release"} {
		if err := os.WriteFile(filepath.Join(targetOut, artifact+".maven-metadata.xml"), []byte(androidEmbeddingMavenMetadata(artifact, version)), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func androidEmbeddingPOM(artifactID, version string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>io.flutter</groupId>
  <artifactId>` + artifactID + `</artifactId>
  <version>` + version + `</version>
  <packaging>jar</packaging>
</project>
`
}

func androidEmbeddingMavenMetadata(artifactID, version string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<metadata>
  <groupId>io.flutter</groupId>
  <artifactId>` + artifactID + `</artifactId>
  <versioning>
    <release>` + version + `</release>
    <versions>
      <version>` + version + `</version>
    </versions>
    <lastUpdated>` + time.Now().UTC().Format("20060102150405") + `</lastUpdated>
  </versioning>
</metadata>
`
}

// writeAndroidEmbeddingJar builds a JAR (zip) containing lib/<abi>/libflutter.so from the soroq .so.
func writeAndroidEmbeddingJar(libflutterPath, jarPath string) error {
	if err := os.MkdirAll(filepath.Dir(jarPath), 0o755); err != nil {
		return err
	}
	tmp := jarPath + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(out)
	w, err := zw.Create("lib/arm64-v8a/libflutter.so")
	if err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	in, err := os.Open(libflutterPath)
	if err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if _, err := io.Copy(w, in); err != nil {
		_ = in.Close()
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	_ = in.Close()
	if err := zw.Close(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, jarPath)
}

// --- low-level helpers ---

func flutterRootFromBin(flutterBin string) (string, error) {
	flutterBin = strings.TrimSpace(flutterBin)
	if flutterBin == "" {
		return "", fmt.Errorf("empty flutter bin path")
	}
	if resolved, err := filepath.EvalSymlinks(flutterBin); err == nil && strings.TrimSpace(resolved) != "" {
		flutterBin = resolved
	}
	return filepath.Dir(filepath.Dir(flutterBin)), nil
}

// resolveFrontendHostEngineDir finds the frontend's host engine artifact dir (the one carrying the host
// gen_snapshot/const_finder, e.g. darwin-x64) under bin/cache/artifacts/engine.
func resolveFrontendHostEngineDir(cacheDir string) (string, error) {
	engineRoot := filepath.Join(cacheDir, "artifacts", "engine")
	entries, err := os.ReadDir(engineRoot)
	if err != nil {
		return "", fmt.Errorf("read frontend engine cache %s: %w", engineRoot, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(engineRoot, entry.Name())
		if fileExists(filepath.Join(candidate, "const_finder.dart.snapshot")) && fileExists(filepath.Join(candidate, "icudtl.dat")) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not find a host engine dir (with const_finder.dart.snapshot + icudtl.dat) under %s", engineRoot)
}

func linkOrCopyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if info, err := os.Stat(dst); err == nil && !info.IsDir() {
		if same, _ := sameFileContents(src, dst); same {
			return nil
		}
		_ = os.Remove(dst)
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFileContents(src, dst)
}

func sameFileContents(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return ai.Size() == bi.Size(), nil
}

func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	srcInfo, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func symlinkForce(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if existing, err := os.Readlink(dst); err == nil {
		if existing == src {
			return nil
		}
		_ = os.Remove(dst)
	} else if _, err := os.Lstat(dst); err == nil {
		_ = os.RemoveAll(dst)
	}
	return os.Symlink(src, dst)
}

func readTrimmedFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func downloadToFile(url, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
