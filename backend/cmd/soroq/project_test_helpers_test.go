package main

import (
	"os"
	"path/filepath"
	"testing"
)

const testSoroqFlutterPubspec = "dependencies:\n  soroq_flutter: any\nflutter:\n  assets:\n    - soroq.yaml\n    - soroq/auto_update_config.json\n"

func writeSoroqFlutterPubspec(t *testing.T, projectDir string) {
	t.Helper()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), testSoroqFlutterPubspec)
	writeSoroqAutoUpdateConfig(t, projectDir)
}

func writeSoroqAutoUpdateConfig(t *testing.T, projectDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(projectDir, "soroq"), 0o755); err != nil {
		t.Fatalf("MkdirAll(soroq) error = %v", err)
	}
	writeFile(t, filepath.Join(projectDir, "soroq", "auto_update_config.json"), `{
  "base_url": "https://api.soroq.dev",
  "track": "stable",
  "enabled": true
}
`)
}

func testSoroqYAML(appID string, channel string) string {
	return "app_id: " + appID + "\n" +
		"channel: " + channel + "\n" +
		"runtime_id_strategy: manifest_trust_v1\n" +
		"manifest_trust:\n" +
		"  keyset_version: 1\n" +
		"  keys:\n" +
		"    - id: prod-primary\n" +
		"      public_key: test-public-key\n"
}
