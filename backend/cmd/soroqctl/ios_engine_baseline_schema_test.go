package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two different artifacts are called "the baseline":
//
//	FREEHAND     soroq.freehand.baseline.v2       .soroq/releases/<runtime-id>/baseline.json
//	ENGINE LANE  soroq.ios_engine_baseline.v2     wherever `release ios-engine --out` points
//
// Handed the freehand one, the engine-lane reader used to report
//
//	--baseline is missing release_id / framework_sha256 / app_dill_sha256
//
// which sends the reader hunting for fields that were never supposed to be in that file. These tests
// pin that each artifact is identified by its DECLARED SCHEMA, and that neither can overwrite the other.

const freehandBaselineFixture = `{
  "schema": "soroq.freehand.baseline.v2",
  "runtime_id": "4affe854cfdbc80f62029983d2a3e4dbf6393701956902f505427123448e0efa",
  "app_dill_sha256": "aad2d30b7f6c0000000000000000000000000000000000000000000000000000",
  "source_app_dill_sha256": "beef00000000000000000000000000000000000000000000000000000000beef"
}`

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEngineLaneReaderRejectsAFreehandBaselineByName(t *testing.T) {
	_, err := loadEngineLaneBaseline(writeTemp(t, "baseline.json", freehandBaselineFixture))
	if err == nil {
		t.Fatal("a freehand baseline was accepted as an engine-lane baseline")
	}
	msg := err.Error()
	// The whole point of the fix: the error must name BOTH schemas, so the reader learns which file
	// they actually have and which one the command wants.
	for _, want := range []string{
		"soroq.freehand.baseline.v2",
		engineLaneBaselineSchema,
		"soroq release ios-engine",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not name %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "missing release_id") {
		t.Errorf("still reporting absent fields instead of the wrong artifact:\n%s", msg)
	}
}

func TestEngineLaneReaderAcceptsBothEngineLaneSchemas(t *testing.T) {
	for _, schema := range []string{engineLaneBaselineSchema, engineLaneBaselineSchemaV1} {
		body := `{"schema":"` + schema + `","release_id":"r1","app_id":"a",` +
			`"app_dill_sha256":"aa","framework_sha256":"bb","flutter_compile_interface_sha256":"cc"}`
		if _, err := loadEngineLaneBaseline(writeTemp(t, "engine.json", body)); err != nil {
			t.Errorf("schema %s rejected: %v", schema, err)
		}
	}
}

// Backward compatibility is retained ONLY where it is unambiguous. Early engine-lane baselines predate
// the schema field; they are still readable because the engine-lane required fields are all present and
// no other artifact carries that combination.
func TestEngineLaneReaderStillReadsLegacySchemalessFiles(t *testing.T) {
	body := `{"release_id":"r1","app_id":"a","app_dill_sha256":"aa","framework_sha256":"bb"}`
	if _, err := loadEngineLaneBaseline(writeTemp(t, "legacy.json", body)); err != nil {
		t.Fatalf("legacy schemaless engine-lane baseline rejected: %v", err)
	}
}

func TestEngineLaneReaderRejectsAnUnknownSchema(t *testing.T) {
	body := `{"schema":"soroq.something_else.v9","release_id":"r1","app_id":"a",` +
		`"app_dill_sha256":"aa","framework_sha256":"bb","flutter_compile_interface_sha256":"cc"}`
	_, err := loadEngineLaneBaseline(writeTemp(t, "other.json", body))
	if err == nil {
		t.Fatal("an unknown schema was accepted")
	}
	if !strings.Contains(err.Error(), engineLaneBaselineSchema) {
		t.Errorf("error should say which schema was expected:\n%s", err)
	}
}

// --- the write side: neither artifact may be written over the other ---

func TestEngineLaneOutPathRefusesTheReservedFreehandFilename(t *testing.T) {
	// Not merely inside a release directory — the NAME is reserved anywhere, because a guard that only
	// triggers on an existing file cannot protect whichever artifact is written first.
	for _, p := range []string{
		"baseline.json",
		filepath.Join(t.TempDir(), "baseline.json"),
		".soroq/releases/4affe854/baseline.json",
	} {
		err := checkEngineLaneBaselineOutPath(p)
		if err == nil {
			t.Errorf("--out %s was accepted despite the reserved filename", p)
			continue
		}
		if !strings.Contains(err.Error(), engineLaneBaselineDefaultName) {
			t.Errorf("refusal should suggest the correct filename:\n%s", err)
		}
	}
}

func TestEngineLaneOutPathRefusesToOverwriteAFreehandBaselineUnderAnyName(t *testing.T) {
	// The dangerous case the old code missed: a freehand baseline unmarshals into engineLaneBaseline
	// "successfully" with an empty ReleaseID, so the immutability comparison was false and the write
	// went straight through.
	p := writeTemp(t, "renamed-freehand.json", freehandBaselineFixture)
	err := checkEngineLaneBaselineOutPath(p)
	if err == nil {
		t.Fatal("an engine-lane write over a freehand baseline was allowed")
	}
	for _, want := range []string{"soroq.freehand.baseline.v2", engineLaneBaselineSchema} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q:\n%s", want, err)
		}
	}
}

func TestEngineLaneOutPathAllowsItsOwnArtifactAndNewFiles(t *testing.T) {
	dir := t.TempDir()
	if err := checkEngineLaneBaselineOutPath(filepath.Join(dir, engineLaneBaselineDefaultName)); err != nil {
		t.Fatalf("a nonexistent engine-lane out path was refused: %v", err)
	}
	own := writeTemp(t, engineLaneBaselineDefaultName,
		`{"schema":"`+engineLaneBaselineSchema+`","release_id":"r1"}`)
	if err := checkEngineLaneBaselineOutPath(own); err != nil {
		t.Fatalf("re-writing the engine lane's OWN baseline was refused: %v", err)
	}
}

func TestTheTwoBaselineFilenamesAreDistinct(t *testing.T) {
	if engineLaneBaselineDefaultName == freehandBaselineReservedName {
		t.Fatal("the engine-lane and freehand baselines must not share a filename")
	}
}
