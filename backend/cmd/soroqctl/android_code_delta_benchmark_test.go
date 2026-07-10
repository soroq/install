package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRunBenchmarkAndroidCodeDeltaWritesJSONReport(t *testing.T) {
	t.Parallel()

	basePath, candidatePath := writeCodeDeltaBenchmarkFixture(t)
	outputPath := filepath.Join(t.TempDir(), "benchmark.json")

	if err := runBenchmarkAndroidCodeDelta([]string{
		"--base", basePath,
		"--candidate", candidatePath,
		"--name", "synthetic",
		"--strategies", "v6,v7,v8,v10,v11,v12,v13",
		"--out", outputPath,
	}); err != nil {
		t.Fatalf("runBenchmarkAndroidCodeDelta() error = %v", err)
	}

	var report androidCodeDeltaBenchmarkReport
	bytes, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile(outputPath) error = %v", err)
	}
	if err := json.Unmarshal(bytes, &report); err != nil {
		t.Fatalf("Unmarshal(report) error = %v", err)
	}

	if report.SchemaVersion != codeDeltaBenchmarkSchemaVersion {
		t.Fatalf("unexpected schema version %d", report.SchemaVersion)
	}
	if report.Surface != codeDeltaBenchmarkSurfaceLibapp {
		t.Fatalf("unexpected report surface %q", report.Surface)
	}
	if len(report.Fixtures) != 1 {
		t.Fatalf("expected 1 fixture, got %d", len(report.Fixtures))
	}
	if report.Fixtures[0].Name != "synthetic" {
		t.Fatalf("unexpected fixture name %q", report.Fixtures[0].Name)
	}
	if report.Fixtures[0].Surface != codeDeltaBenchmarkSurfaceLibapp {
		t.Fatalf("unexpected fixture surface %q", report.Fixtures[0].Surface)
	}
	if len(report.Fixtures[0].Strategies) != 7 {
		t.Fatalf("expected 7 strategy results, got %d", len(report.Fixtures[0].Strategies))
	}
	if report.Fixtures[0].Winner == nil {
		t.Fatalf("expected fixture winner")
	}
	if report.Aggregate.FixtureCount != 1 || report.Aggregate.Winner == nil {
		t.Fatalf("expected aggregate winner, got %#v", report.Aggregate)
	}
	for _, strategy := range report.Fixtures[0].Strategies {
		if !strategy.Verified {
			t.Fatalf("expected strategy %s to verify", strategy.Strategy)
		}
		if strategy.RawDeltaBytes == 0 || strategy.DeflatedDeltaBytes == 0 {
			t.Fatalf("expected non-zero strategy sizes: %#v", strategy)
		}
		if strategy.Strategy == codeDeltaStrategyV11 || strategy.Strategy == codeDeltaStrategyV12 || strategy.Strategy == codeDeltaStrategyV13 {
			if strategy.Streams == nil {
				t.Fatalf("expected stream diagnostics for %s", strategy.Strategy)
			}
			if strategy.Streams.CompressedPayloadBytes == 0 || strategy.Streams.AddCompressedPayloadSharePercent == 0 {
				t.Fatalf("expected useful stream diagnostics for %s: %#v", strategy.Strategy, strategy.Streams)
			}
		}
	}
}

func TestRunBenchmarkAndroidCodeDeltaSupportsIsolateSnapshotSurface(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	baseData := []byte("base isolate snapshot data")
	candidateData := []byte("base isolate snapshot DATA")
	baseInstructions := []byte{0x10, 0x20, 0x30, 0x40, 0x50}
	candidateInstructions := []byte{0x10, 0x20, 0xaa, 0x40, 0x50}
	basePath := filepath.Join(root, "base-libapp.so")
	candidatePath := filepath.Join(root, "candidate-libapp.so")
	outputPath := filepath.Join(root, "benchmark.json")
	if err := os.WriteFile(basePath, buildSnapshotBenchmarkELF64(t, baseData, baseInstructions), 0o644); err != nil {
		t.Fatalf("WriteFile(basePath) error = %v", err)
	}
	if err := os.WriteFile(candidatePath, buildSnapshotBenchmarkELF64(t, candidateData, candidateInstructions), 0o644); err != nil {
		t.Fatalf("WriteFile(candidatePath) error = %v", err)
	}

	if err := runBenchmarkAndroidCodeDelta([]string{
		"--base", basePath,
		"--candidate", candidatePath,
		"--name", "snapshot-surface",
		"--surface", codeDeltaBenchmarkSurfaceIsolateSnapshot,
		"--strategies", "v14",
		"--out", outputPath,
	}); err != nil {
		t.Fatalf("runBenchmarkAndroidCodeDelta() error = %v", err)
	}

	var report androidCodeDeltaBenchmarkReport
	reportBytes, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile(outputPath) error = %v", err)
	}
	if err := json.Unmarshal(reportBytes, &report); err != nil {
		t.Fatalf("Unmarshal(report) error = %v", err)
	}

	if report.Surface != codeDeltaBenchmarkSurfaceIsolateSnapshot {
		t.Fatalf("unexpected report surface %q", report.Surface)
	}
	if len(report.Fixtures) != 1 {
		t.Fatalf("expected 1 fixture, got %d", len(report.Fixtures))
	}
	fixture := report.Fixtures[0]
	if fixture.Surface != codeDeltaBenchmarkSurfaceIsolateSnapshot {
		t.Fatalf("unexpected fixture surface %q", fixture.Surface)
	}
	if fixture.BaseSizeBytes != uint64(len(baseData)+len(baseInstructions)) {
		t.Fatalf("unexpected base surface size %d", fixture.BaseSizeBytes)
	}
	if fixture.CandidateSizeBytes != uint64(len(candidateData)+len(candidateInstructions)) {
		t.Fatalf("unexpected candidate surface size %d", fixture.CandidateSizeBytes)
	}
	if len(fixture.Strategies) != 1 || !fixture.Strategies[0].Verified {
		t.Fatalf("expected one verified strategy, got %#v", fixture.Strategies)
	}
	if fixture.Strategies[0].Sections != nil {
		t.Fatalf("expected no ELF section diagnostics for extracted snapshot bytes")
	}
}

func TestDiagnoseCodeDeltaStreamsReportsSplitComponents(t *testing.T) {
	t.Parallel()

	baseBytes := buildPatternedCodePayload(900)
	candidateBytes := append([]byte(nil), baseBytes...)
	for offset := 2048; offset < minInt(len(candidateBytes), 10000); offset++ {
		candidateBytes[offset] = candidateBytes[offset] + 1
	}

	deltaBytes, _, err := buildCodeDeltaV12(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV12() error = %v", err)
	}
	streams, err := diagnoseCodeDeltaStreams(deltaBytes)
	if err != nil {
		t.Fatalf("diagnoseCodeDeltaStreams() error = %v", err)
	}
	if streams == nil {
		t.Fatalf("expected stream diagnostics")
	}
	if streams.Magic != codeDeltaMagicV12 {
		t.Fatalf("unexpected magic %q", streams.Magic)
	}
	if streams.AddTransform == nil {
		t.Fatalf("expected v12 add transform")
	}
	if streams.HeaderBytes == 0 || streams.CompressedPayloadBytes == 0 {
		t.Fatalf("expected non-zero stream sizes: %#v", streams)
	}
}

func TestDiagnoseCodeDeltaSectionsMapsAddBytes(t *testing.T) {
	t.Parallel()

	baseBytes := buildMinimalBenchmarkELF64(t)
	candidateBytes := append([]byte(nil), baseBytes...)
	for offset := 0x200; offset < 0x210; offset++ {
		candidateBytes[offset] = candidateBytes[offset] + 1
	}
	ops := []codeDeltaOp{{
		Kind:       codeDeltaOpAdd,
		BaseOffset: 0x200,
		Length:     16,
		Literal:    bytes.Repeat([]byte{1}, 16),
	}}
	deltaBytes, _, err := encodeSplitContextCodeDelta(
		codeDeltaMagicV11,
		baseBytes,
		ops,
		summarizeCodeDelta(baseBytes, ops),
	)
	if err != nil {
		t.Fatalf("encodeSplitContextCodeDelta() error = %v", err)
	}

	sections, err := diagnoseCodeDeltaSections(baseBytes, candidateBytes, deltaBytes)
	if err != nil {
		t.Fatalf("diagnoseCodeDeltaSections() error = %v", err)
	}
	if sections == nil {
		t.Fatalf("expected ELF section diagnostics")
	}
	if sections.Format != "elf" || sections.AddBytes != 16 || sections.AddOps != 1 {
		t.Fatalf("unexpected section totals: %#v", sections)
	}
	if len(sections.Sections) == 0 {
		t.Fatalf("expected section summaries")
	}
	rodata := sections.Sections[0]
	if rodata.Name != ".rodata" {
		t.Fatalf("expected .rodata first, got %#v", rodata)
	}
	if rodata.AddBytes != 16 || rodata.AddOps != 1 || rodata.SameOffsetChangedBytes != 16 {
		t.Fatalf("unexpected .rodata attribution: %#v", rodata)
	}
}

func TestRunBenchmarkAndroidCodeDeltaReadsFixturesFile(t *testing.T) {
	t.Parallel()

	basePath, candidatePath := writeCodeDeltaBenchmarkFixture(t)
	fixturesPath := filepath.Join(t.TempDir(), "fixtures.json")
	if err := os.WriteFile(fixturesPath, []byte(`{
		"fixtures": [
			{
				"name": "from-file",
				"base_path": `+quoteJSONString(basePath)+`,
				"candidate_path": `+quoteJSONString(candidatePath)+`
			}
		]
	}`), 0o644); err != nil {
		t.Fatalf("WriteFile(fixturesPath) error = %v", err)
	}

	report, err := benchmarkAndroidCodeDeltaFixturesFromCLIForTest(fixturesPath, "v8")
	if err != nil {
		t.Fatalf("benchmarkAndroidCodeDeltaFixturesFromCLIForTest() error = %v", err)
	}
	if len(report.Fixtures) != 1 || report.Fixtures[0].Name != "from-file" {
		t.Fatalf("unexpected fixtures %#v", report.Fixtures)
	}
	if report.Fixtures[0].Winner == nil || report.Fixtures[0].Winner.Strategy != codeDeltaStrategyV8 {
		t.Fatalf("expected v8 winner, got %#v", report.Fixtures[0].Winner)
	}
}

func TestParseCodeDeltaBenchmarkPair(t *testing.T) {
	t.Parallel()

	fixture, err := parseCodeDeltaBenchmarkPair("demo=/tmp/base.so,/tmp/candidate.so")
	if err != nil {
		t.Fatalf("parseCodeDeltaBenchmarkPair() error = %v", err)
	}
	if fixture.Name != "demo" || fixture.BasePath != "/tmp/base.so" || fixture.CandidatePath != "/tmp/candidate.so" {
		t.Fatalf("unexpected fixture %#v", fixture)
	}
	if _, err := parseCodeDeltaBenchmarkPair("bad-format"); err == nil {
		t.Fatalf("expected bad pair format to fail")
	}
}

func TestParseCodeDeltaBenchmarkStrategiesRejectsUnknown(t *testing.T) {
	t.Parallel()

	strategies, err := parseCodeDeltaBenchmarkStrategies("v8,v6,v10,v11,v12,v8")
	if err != nil {
		t.Fatalf("parseCodeDeltaBenchmarkStrategies() error = %v", err)
	}
	if len(strategies) != 5 {
		t.Fatalf("expected duplicate strategy to be removed, got %d", len(strategies))
	}
	aliasStrategies, err := parseCodeDeltaBenchmarkStrategies("v8,suffix_context_add_v8,indexed_output_copy_v11,v11,sparse_indexed_output_copy_v12,v12,bitplane_indexed_output_copy_v13,v13,split_add_streams_v14,v14,bsdiff_bzip2_v15,v15")
	if err != nil {
		t.Fatalf("parseCodeDeltaBenchmarkStrategies(alias) error = %v", err)
	}
	if len(aliasStrategies) != 6 {
		t.Fatalf("expected duplicate alias strategies to be removed, got %d", len(aliasStrategies))
	}
	defaultStrategies, err := parseCodeDeltaBenchmarkStrategies("default")
	if err != nil {
		t.Fatalf("parseCodeDeltaBenchmarkStrategies(default) error = %v", err)
	}
	if len(defaultStrategies) != 1 || defaultStrategies[0].name != codeDeltaStrategyV15 {
		t.Fatalf("expected default strategy to resolve to v15, got %#v", defaultStrategies)
	}
	if _, err := parseCodeDeltaBenchmarkStrategies("v8,v99"); err == nil {
		t.Fatalf("expected unknown strategy to fail")
	}
}

func TestParseCodeDeltaBenchmarkSurface(t *testing.T) {
	t.Parallel()

	defaultSurface, err := parseCodeDeltaBenchmarkSurface("")
	if err != nil {
		t.Fatalf("parseCodeDeltaBenchmarkSurface(default) error = %v", err)
	}
	if defaultSurface != codeDeltaBenchmarkSurfaceLibapp {
		t.Fatalf("expected default libapp surface, got %q", defaultSurface)
	}
	isolateSurface, err := parseCodeDeltaBenchmarkSurface("isolate")
	if err != nil {
		t.Fatalf("parseCodeDeltaBenchmarkSurface(isolate) error = %v", err)
	}
	if isolateSurface != codeDeltaBenchmarkSurfaceIsolateSnapshot {
		t.Fatalf("expected isolate snapshot surface, got %q", isolateSurface)
	}
	if _, err := parseCodeDeltaBenchmarkSurface("vm-snapshot"); err == nil {
		t.Fatalf("expected unknown surface to fail")
	}
}

func benchmarkAndroidCodeDeltaFixturesFromCLIForTest(fixturesPath string, strategiesRaw string) (*androidCodeDeltaBenchmarkReport, error) {
	fixtures, err := loadCodeDeltaBenchmarkFixtures("", "", "", fixturesPath, nil)
	if err != nil {
		return nil, err
	}
	strategies, err := parseCodeDeltaBenchmarkStrategies(strategiesRaw)
	if err != nil {
		return nil, err
	}
	return benchmarkAndroidCodeDeltaFixtures(fixtures, strategies, codeDeltaBenchmarkSurfaceLibapp)
}

func writeCodeDeltaBenchmarkFixture(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	baseBytes := buildPatternedCodePayload(900)
	candidateBytes := append([]byte(nil), baseBytes...)
	for offset := 2048; offset < minInt(len(candidateBytes), 10000); offset++ {
		candidateBytes[offset] = candidateBytes[offset] + 1
	}
	insertOffset := minInt(len(candidateBytes), 16000)
	candidateBytes = append(candidateBytes[:insertOffset], append([]byte("benchmark-fixture-insert"), candidateBytes[insertOffset:]...)...)

	basePath := filepath.Join(root, "base-libapp.so")
	candidatePath := filepath.Join(root, "candidate-libapp.so")
	if err := os.WriteFile(basePath, baseBytes, 0o644); err != nil {
		t.Fatalf("WriteFile(basePath) error = %v", err)
	}
	if err := os.WriteFile(candidatePath, candidateBytes, 0o644); err != nil {
		t.Fatalf("WriteFile(candidatePath) error = %v", err)
	}
	return basePath, candidatePath
}

func quoteJSONString(value string) string {
	bytes, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(bytes)
}

func buildMinimalBenchmarkELF64(t *testing.T) []byte {
	t.Helper()

	const (
		sectionHeaderOffset = 0x40
		sectionHeaderSize   = 64
		rodataOffset        = 0x200
		rodataSize          = 32
		shstrtabOffset      = 0x300
	)

	output := make([]byte, 0x400)
	copy(output[:16], []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(output[16:], 2)
	binary.LittleEndian.PutUint16(output[18:], 183)
	binary.LittleEndian.PutUint32(output[20:], 1)
	binary.LittleEndian.PutUint64(output[40:], sectionHeaderOffset)
	binary.LittleEndian.PutUint16(output[52:], 64)
	binary.LittleEndian.PutUint16(output[58:], sectionHeaderSize)
	binary.LittleEndian.PutUint16(output[60:], 3)
	binary.LittleEndian.PutUint16(output[62:], 2)

	names := []byte("\x00.rodata\x00.shstrtab\x00")
	copy(output[shstrtabOffset:], names)
	for index := 0; index < rodataSize; index++ {
		output[rodataOffset+index] = byte(index)
	}

	writeSectionHeader := func(index int, nameOffset uint32, sectionType uint32, flags uint64, offset uint64, size uint64) {
		start := sectionHeaderOffset + index*sectionHeaderSize
		binary.LittleEndian.PutUint32(output[start:], nameOffset)
		binary.LittleEndian.PutUint32(output[start+4:], sectionType)
		binary.LittleEndian.PutUint64(output[start+8:], flags)
		binary.LittleEndian.PutUint64(output[start+24:], offset)
		binary.LittleEndian.PutUint64(output[start+32:], size)
	}
	writeSectionHeader(1, 1, 1, 2, rodataOffset, rodataSize)
	writeSectionHeader(2, 9, 3, 0, shstrtabOffset, uint64(len(names)))

	return output
}

func buildSnapshotBenchmarkELF64(t *testing.T, isolateData []byte, isolateInstructions []byte) []byte {
	t.Helper()

	const (
		sectionHeaderOffset = 0x40
		sectionHeaderSize   = 64
		sectionCount        = 6
		rodataAddr          = 0x70000000
		textAddr            = 0x71000000
		dataSymbolOffset    = 8
		instrSymbolOffset   = 4
		symbolEntrySize     = 24
	)
	alignOffset := func(value int, alignment int) int {
		return (value + alignment - 1) &^ (alignment - 1)
	}

	rodataOffset := 0x200
	rodataSize := dataSymbolOffset + len(isolateData)
	textOffset := alignOffset(rodataOffset+rodataSize, 0x100)
	textSize := instrSymbolOffset + len(isolateInstructions)
	symtabOffset := alignOffset(textOffset+textSize, 0x100)
	symtabSize := 3 * symbolEntrySize
	strtab := []byte("\x00_kDartIsolateSnapshotData\x00_kDartIsolateSnapshotInstructions\x00")
	strtabOffset := alignOffset(symtabOffset+symtabSize, 0x80)
	shstrtab := []byte("\x00.rodata\x00.text\x00.symtab\x00.strtab\x00.shstrtab\x00")
	shstrtabOffset := alignOffset(strtabOffset+len(strtab), 0x80)
	outputSize := alignOffset(shstrtabOffset+len(shstrtab), 0x80)

	output := make([]byte, outputSize)
	copy(output[:16], []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(output[16:], 2)
	binary.LittleEndian.PutUint16(output[18:], 183)
	binary.LittleEndian.PutUint32(output[20:], 1)
	binary.LittleEndian.PutUint64(output[40:], sectionHeaderOffset)
	binary.LittleEndian.PutUint16(output[52:], 64)
	binary.LittleEndian.PutUint16(output[58:], sectionHeaderSize)
	binary.LittleEndian.PutUint16(output[60:], sectionCount)
	binary.LittleEndian.PutUint16(output[62:], 5)

	copy(output[rodataOffset+dataSymbolOffset:], isolateData)
	copy(output[textOffset+instrSymbolOffset:], isolateInstructions)
	copy(output[strtabOffset:], strtab)
	copy(output[shstrtabOffset:], shstrtab)

	writeSectionHeader := func(index int, nameOffset uint32, sectionType uint32, flags uint64, addr uint64, offset uint64, size uint64, link uint32, info uint32, addralign uint64, entsize uint64) {
		start := sectionHeaderOffset + index*sectionHeaderSize
		binary.LittleEndian.PutUint32(output[start:], nameOffset)
		binary.LittleEndian.PutUint32(output[start+4:], sectionType)
		binary.LittleEndian.PutUint64(output[start+8:], flags)
		binary.LittleEndian.PutUint64(output[start+16:], addr)
		binary.LittleEndian.PutUint64(output[start+24:], offset)
		binary.LittleEndian.PutUint64(output[start+32:], size)
		binary.LittleEndian.PutUint32(output[start+40:], link)
		binary.LittleEndian.PutUint32(output[start+44:], info)
		binary.LittleEndian.PutUint64(output[start+48:], addralign)
		binary.LittleEndian.PutUint64(output[start+56:], entsize)
	}
	shstrNameOffset := func(name string) uint32 {
		offset := bytes.Index(shstrtab, []byte(name))
		if offset < 0 {
			t.Fatalf("missing section name %s", name)
		}
		return uint32(offset)
	}
	strNameOffset := func(name string) uint32 {
		offset := bytes.Index(strtab, []byte(name))
		if offset < 0 {
			t.Fatalf("missing symbol name %s", name)
		}
		return uint32(offset)
	}

	writeSectionHeader(1, shstrNameOffset(".rodata"), 1, 2, rodataAddr, uint64(rodataOffset), uint64(rodataSize), 0, 0, 8, 0)
	writeSectionHeader(2, shstrNameOffset(".text"), 1, 6, textAddr, uint64(textOffset), uint64(textSize), 0, 0, 4, 0)
	writeSectionHeader(3, shstrNameOffset(".symtab"), 2, 0, 0, uint64(symtabOffset), uint64(symtabSize), 4, 1, 8, symbolEntrySize)
	writeSectionHeader(4, shstrNameOffset(".strtab"), 3, 0, 0, uint64(strtabOffset), uint64(len(strtab)), 0, 0, 1, 0)
	writeSectionHeader(5, shstrNameOffset(".shstrtab"), 3, 0, 0, uint64(shstrtabOffset), uint64(len(shstrtab)), 0, 0, 1, 0)

	writeSymbol := func(index int, nameOffset uint32, info byte, sectionIndex uint16, value uint64, size uint64) {
		start := symtabOffset + index*symbolEntrySize
		binary.LittleEndian.PutUint32(output[start:], nameOffset)
		output[start+4] = info
		binary.LittleEndian.PutUint16(output[start+6:], sectionIndex)
		binary.LittleEndian.PutUint64(output[start+8:], value)
		binary.LittleEndian.PutUint64(output[start+16:], size)
	}
	writeSymbol(1, strNameOffset("_kDartIsolateSnapshotData"), 0x11, 1, rodataAddr+dataSymbolOffset, uint64(len(isolateData)))
	writeSymbol(2, strNameOffset("_kDartIsolateSnapshotInstructions"), 0x12, 2, textAddr+instrSymbolOffset, uint64(len(isolateInstructions)))

	return output
}
