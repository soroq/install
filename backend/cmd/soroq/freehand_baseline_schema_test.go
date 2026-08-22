package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Mirror image of the engine-lane guard: an ENGINE-LANE baseline that lands in a freehand release
// directory must be identified as the wrong artifact, not diagnosed as a freehand baseline with fields
// missing. Both errors name both schemas, so whichever side the reader is standing on, they learn which
// file they have and which one the command wants.
func TestFreehandReaderRejectsAnEngineLaneBaseline(t *testing.T) {
	for _, schema := range []string{"soroq.ios_engine_baseline.v1", "soroq.ios_engine_baseline.v2"} {
		dir := t.TempDir()
		body := `{"schema":"` + schema + `","release_id":"a2-r1","app_id":"dev.soroq.a1on",` +
			`"app_dill_sha256":"aad2d30b7f6c","framework_sha256":"4a038a611352"}`
		if err := os.WriteFile(filepath.Join(dir, "baseline.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := verifyExistingBaseline(dir)
		if err == nil {
			t.Fatalf("%s: an engine-lane baseline was accepted as a freehand baseline", schema)
		}
		msg := err.Error()
		for _, want := range []string{schema, freehandBaselineSchemaV2, "soroqctl release ios-engine"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: error does not name %q:\n%s", schema, want, msg)
			}
		}
		// It must not fall through to the generic field diagnostics, which describe the wrong problem.
		if strings.Contains(msg, "predates dependency-OTA support") ||
			strings.Contains(msg, "missing required field") {
			t.Errorf("%s: reported a missing-field problem instead of the wrong artifact:\n%s", schema, msg)
		}
	}
}
