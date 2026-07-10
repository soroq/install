package main

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	codeDeltaBenchmarkSchemaVersion = 1
	defaultCodeDeltaBenchmarkName   = "fixture-1"

	codeDeltaBenchmarkSurfaceLibapp          = "libapp"
	codeDeltaBenchmarkSurfaceIsolateSnapshot = "isolate-snapshot"
)

type androidCodeDeltaBenchmarkFixture struct {
	Name          string `json:"name"`
	BasePath      string `json:"base_path"`
	CandidatePath string `json:"candidate_path"`
}

type androidCodeDeltaBenchmarkFixturesFile struct {
	Fixtures []androidCodeDeltaBenchmarkFixture `json:"fixtures"`
}

type androidCodeDeltaBenchmarkReport struct {
	SchemaVersion int                                   `json:"schema_version"`
	GeneratedAt   time.Time                             `json:"generated_at"`
	Surface       string                                `json:"surface"`
	Strategies    []string                              `json:"strategies"`
	Fixtures      []androidCodeDeltaBenchmarkFixtureRun `json:"fixtures"`
	Aggregate     androidCodeDeltaBenchmarkAggregate    `json:"aggregate"`
}

type androidCodeDeltaBenchmarkFixtureRun struct {
	Name               string                                 `json:"name"`
	BasePath           string                                 `json:"base_path"`
	CandidatePath      string                                 `json:"candidate_path"`
	Surface            string                                 `json:"surface"`
	BaseSizeBytes      uint64                                 `json:"base_size_bytes"`
	CandidateSizeBytes uint64                                 `json:"candidate_size_bytes"`
	Strategies         []androidCodeDeltaBenchmarkStrategyRun `json:"strategies"`
	Winner             *androidCodeDeltaBenchmarkWinner       `json:"winner,omitempty"`
}

type androidCodeDeltaBenchmarkStrategyRun struct {
	Strategy           string                       `json:"strategy"`
	RawDeltaBytes      uint64                       `json:"raw_delta_bytes"`
	DeflatedDeltaBytes uint64                       `json:"deflated_delta_bytes"`
	Verified           bool                         `json:"verified"`
	Summary            codeDeltaSummary             `json:"summary"`
	Streams            *codeDeltaStreamDiagnostics  `json:"streams,omitempty"`
	Sections           *codeDeltaSectionDiagnostics `json:"sections,omitempty"`
}

type codeDeltaStreamDiagnostics struct {
	Magic                               string  `json:"magic"`
	HeaderBytes                         uint64  `json:"header_bytes"`
	ControlRawBytes                     uint64  `json:"control_raw_bytes"`
	ControlCompressedBytes              uint64  `json:"control_compressed_bytes"`
	InsertRawBytes                      uint64  `json:"insert_raw_bytes"`
	InsertCompressedBytes               uint64  `json:"insert_compressed_bytes"`
	AddRawBytes                         uint64  `json:"add_raw_bytes"`
	AddCompressedBytes                  uint64  `json:"add_compressed_bytes"`
	AddTransform                        *string `json:"add_transform,omitempty"`
	CompressedPayloadBytes              uint64  `json:"compressed_payload_bytes"`
	CompressedPayloadSharePercent       float64 `json:"compressed_payload_share_percent"`
	AddCompressedPayloadSharePercent    float64 `json:"add_compressed_payload_share_percent"`
	AddCompressedTotalDeltaSharePercent float64 `json:"add_compressed_total_delta_share_percent"`
}

type codeDeltaSectionDiagnostics struct {
	Format                 string                    `json:"format"`
	SectionCount           int                       `json:"section_count"`
	AddBytes               uint64                    `json:"add_bytes"`
	AddOps                 int                       `json:"add_ops"`
	SameOffsetChangedBytes uint64                    `json:"same_offset_changed_bytes"`
	Sections               []codeDeltaSectionSummary `json:"sections"`
}

type codeDeltaSectionSummary struct {
	Name                   string  `json:"name"`
	Offset                 uint64  `json:"offset"`
	Size                   uint64  `json:"size"`
	AddBytes               uint64  `json:"add_bytes"`
	AddOps                 int     `json:"add_ops"`
	SameOffsetChangedBytes uint64  `json:"same_offset_changed_bytes"`
	SameOffsetChangedPct   float64 `json:"same_offset_changed_percent"`
}

type codeDeltaELFSection struct {
	Name   string
	Offset uint64
	Size   uint64
}

type codeDeltaControlOp struct {
	Kind          byte
	BaseOffset    uint64
	Length        uint64
	PayloadLength uint64
}

type androidCodeDeltaBenchmarkWinner struct {
	Strategy           string `json:"strategy"`
	RawDeltaBytes      uint64 `json:"raw_delta_bytes"`
	DeflatedDeltaBytes uint64 `json:"deflated_delta_bytes"`
}

type androidCodeDeltaBenchmarkAggregate struct {
	FixtureCount int                                      `json:"fixture_count"`
	Strategies   []androidCodeDeltaBenchmarkStrategyTotal `json:"strategies"`
	Winner       *androidCodeDeltaBenchmarkWinner         `json:"winner,omitempty"`
}

type androidCodeDeltaBenchmarkStrategyTotal struct {
	Strategy                  string `json:"strategy"`
	RawDeltaBytes             uint64 `json:"raw_delta_bytes"`
	DeflatedDeltaBytes        uint64 `json:"deflated_delta_bytes"`
	Wins                      int    `json:"wins"`
	VerifiedFixtures          int    `json:"verified_fixtures"`
	AverageDeflatedDeltaBytes uint64 `json:"average_deflated_delta_bytes"`
}

type codeDeltaBenchmarkStrategyBuilder struct {
	name  string
	build func([]byte, []byte) ([]byte, codeDeltaSummary, error)
}

type repeatedStringFlag []string

func (flag *repeatedStringFlag) String() string {
	return strings.Join(*flag, ",")
}

func (flag *repeatedStringFlag) Set(value string) error {
	*flag = append(*flag, value)
	return nil
}

func runBenchmarkAndroidCodeDelta(args []string) error {
	fs := flag.NewFlagSet("benchmark-android-code-delta", flag.ContinueOnError)
	basePath := fs.String("base", "", "base libapp.so path for a single fixture")
	candidatePath := fs.String("candidate", "", "candidate libapp.so path for a single fixture")
	name := fs.String("name", defaultCodeDeltaBenchmarkName, "fixture name for --base/--candidate")
	fixturesPath := fs.String("fixtures-file", "", "JSON file containing benchmark fixtures")
	strategiesRaw := fs.String("strategies", "v8,v10,v11", "comma-separated strategies to compare: v1,v2,v3,v4,v5,v6,v7,v8,v10,v11,v12,v13,v14,v15")
	surfaceRaw := fs.String("surface", codeDeltaBenchmarkSurfaceLibapp, "benchmark input surface: libapp or isolate-snapshot")
	outputPath := fs.String("out", "", "optional path to write benchmark JSON")
	var pairFlags repeatedStringFlag
	fs.Var(&pairFlags, "pair", "additional fixture as name=base_path,candidate_path; may be repeated")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fixtures, err := loadCodeDeltaBenchmarkFixtures(*basePath, *candidatePath, *name, *fixturesPath, pairFlags)
	if err != nil {
		return err
	}
	strategies, err := parseCodeDeltaBenchmarkStrategies(*strategiesRaw)
	if err != nil {
		return err
	}
	surface, err := parseCodeDeltaBenchmarkSurface(*surfaceRaw)
	if err != nil {
		return err
	}
	report, err := benchmarkAndroidCodeDeltaFixtures(fixtures, strategies, surface)
	if err != nil {
		return err
	}
	return writeJSONOutput(report, *outputPath)
}

func loadCodeDeltaBenchmarkFixtures(basePath string, candidatePath string, name string, fixturesPath string, pairFlags []string) ([]androidCodeDeltaBenchmarkFixture, error) {
	fixtures := make([]androidCodeDeltaBenchmarkFixture, 0)
	if strings.TrimSpace(fixturesPath) != "" {
		fileFixtures, err := readCodeDeltaBenchmarkFixturesFile(fixturesPath)
		if err != nil {
			return nil, err
		}
		fixtures = append(fixtures, fileFixtures...)
	}
	if strings.TrimSpace(basePath) != "" || strings.TrimSpace(candidatePath) != "" {
		if strings.TrimSpace(basePath) == "" || strings.TrimSpace(candidatePath) == "" {
			return nil, errors.New("--base and --candidate must be provided together")
		}
		fixtures = append(fixtures, androidCodeDeltaBenchmarkFixture{
			Name:          strings.TrimSpace(name),
			BasePath:      strings.TrimSpace(basePath),
			CandidatePath: strings.TrimSpace(candidatePath),
		})
	}
	for _, rawPair := range pairFlags {
		fixture, err := parseCodeDeltaBenchmarkPair(rawPair)
		if err != nil {
			return nil, err
		}
		fixtures = append(fixtures, fixture)
	}
	if len(fixtures) == 0 {
		return nil, errors.New("provide --base/--candidate, --pair, or --fixtures-file")
	}
	for index := range fixtures {
		fixtures[index].Name = strings.TrimSpace(fixtures[index].Name)
		fixtures[index].BasePath = strings.TrimSpace(fixtures[index].BasePath)
		fixtures[index].CandidatePath = strings.TrimSpace(fixtures[index].CandidatePath)
		if fixtures[index].Name == "" {
			fixtures[index].Name = fmt.Sprintf("fixture-%d", index+1)
		}
		if fixtures[index].BasePath == "" || fixtures[index].CandidatePath == "" {
			return nil, fmt.Errorf("fixture %q requires base_path and candidate_path", fixtures[index].Name)
		}
	}
	return fixtures, nil
}

func readCodeDeltaBenchmarkFixturesFile(path string) ([]androidCodeDeltaBenchmarkFixture, error) {
	bytes, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read fixtures file: %w", err)
	}
	var wrapped androidCodeDeltaBenchmarkFixturesFile
	if err := json.Unmarshal(bytes, &wrapped); err == nil && len(wrapped.Fixtures) > 0 {
		return wrapped.Fixtures, nil
	}
	var fixtures []androidCodeDeltaBenchmarkFixture
	if err := json.Unmarshal(bytes, &fixtures); err != nil {
		return nil, fmt.Errorf("parse fixtures file: %w", err)
	}
	return fixtures, nil
}

func parseCodeDeltaBenchmarkPair(raw string) (androidCodeDeltaBenchmarkFixture, error) {
	nameAndPaths := strings.SplitN(raw, "=", 2)
	if len(nameAndPaths) != 2 {
		return androidCodeDeltaBenchmarkFixture{}, fmt.Errorf("invalid --pair %q, expected name=base_path,candidate_path", raw)
	}
	paths := strings.SplitN(nameAndPaths[1], ",", 2)
	if len(paths) != 2 {
		return androidCodeDeltaBenchmarkFixture{}, fmt.Errorf("invalid --pair %q, expected name=base_path,candidate_path", raw)
	}
	return androidCodeDeltaBenchmarkFixture{
		Name:          strings.TrimSpace(nameAndPaths[0]),
		BasePath:      strings.TrimSpace(paths[0]),
		CandidatePath: strings.TrimSpace(paths[1]),
	}, nil
}

func parseCodeDeltaBenchmarkStrategies(raw string) ([]codeDeltaBenchmarkStrategyBuilder, error) {
	available := availableCodeDeltaBenchmarkStrategies()
	parts := strings.Split(raw, ",")
	strategies := make([]codeDeltaBenchmarkStrategyBuilder, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		key := strings.ToLower(strings.TrimSpace(part))
		if key == "" {
			continue
		}
		strategy, ok := available[key]
		if !ok {
			return nil, fmt.Errorf("unknown strategy %q", part)
		}
		if seen[strategy.name] {
			continue
		}
		seen[strategy.name] = true
		strategies = append(strategies, strategy)
	}
	if len(strategies) == 0 {
		return nil, errors.New("at least one strategy is required")
	}
	return strategies, nil
}

func parseCodeDeltaBenchmarkSurface(raw string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(raw))
	switch key {
	case "", codeDeltaBenchmarkSurfaceLibapp:
		return codeDeltaBenchmarkSurfaceLibapp, nil
	case "isolate", "isolate-snapshots", codeDeltaBenchmarkSurfaceIsolateSnapshot:
		return codeDeltaBenchmarkSurfaceIsolateSnapshot, nil
	default:
		return "", fmt.Errorf("unknown benchmark surface %q", raw)
	}
}

func availableCodeDeltaBenchmarkStrategies() map[string]codeDeltaBenchmarkStrategyBuilder {
	v8 := codeDeltaBenchmarkStrategyBuilder{name: codeDeltaStrategyV8, build: buildCodeDeltaV8}
	v10 := codeDeltaBenchmarkStrategyBuilder{name: codeDeltaStrategyV10, build: buildCodeDeltaV10}
	v11 := codeDeltaBenchmarkStrategyBuilder{name: codeDeltaStrategyV11, build: buildCodeDeltaV11}
	v12 := codeDeltaBenchmarkStrategyBuilder{name: codeDeltaStrategyV12, build: buildCodeDeltaV12}
	v13 := codeDeltaBenchmarkStrategyBuilder{name: codeDeltaStrategyV13, build: buildCodeDeltaV13}
	v14 := codeDeltaBenchmarkStrategyBuilder{name: codeDeltaStrategyV14, build: buildCodeDeltaV14}
	v15 := codeDeltaBenchmarkStrategyBuilder{name: codeDeltaStrategyV15, build: buildCodeDeltaV15}
	return map[string]codeDeltaBenchmarkStrategyBuilder{
		"v1":                 {name: codeDeltaStrategyV1, build: buildCodeDeltaV1},
		"v2":                 {name: codeDeltaStrategyV2, build: buildCodeDeltaV2},
		"v3":                 {name: codeDeltaStrategyV3, build: buildCodeDeltaV3},
		"v4":                 {name: codeDeltaStrategyV4, build: buildCodeDeltaV4},
		"v5":                 {name: codeDeltaStrategyV5, build: buildCodeDeltaV5},
		"v6":                 {name: codeDeltaStrategyV6, build: buildCodeDeltaV6},
		"v7":                 {name: codeDeltaStrategyV7, build: buildCodeDeltaV7},
		"v8":                 v8,
		"v10":                v10,
		"v11":                v11,
		"v12":                v12,
		"v13":                v13,
		"v14":                v14,
		"v15":                v15,
		codeDeltaStrategyV8:  v8,
		codeDeltaStrategyV10: v10,
		codeDeltaStrategyV11: v11,
		codeDeltaStrategyV12: v12,
		codeDeltaStrategyV13: v13,
		codeDeltaStrategyV14: v14,
		codeDeltaStrategyV15: v15,
		"default":            v15,
	}
}

func benchmarkAndroidCodeDeltaFixtures(fixtures []androidCodeDeltaBenchmarkFixture, strategies []codeDeltaBenchmarkStrategyBuilder, surface string) (*androidCodeDeltaBenchmarkReport, error) {
	report := &androidCodeDeltaBenchmarkReport{
		SchemaVersion: codeDeltaBenchmarkSchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		Surface:       surface,
		Strategies:    make([]string, 0, len(strategies)),
		Fixtures:      make([]androidCodeDeltaBenchmarkFixtureRun, 0, len(fixtures)),
	}
	for _, strategy := range strategies {
		report.Strategies = append(report.Strategies, strategy.name)
	}
	for _, fixture := range fixtures {
		run, err := benchmarkAndroidCodeDeltaFixture(fixture, strategies, surface)
		if err != nil {
			return nil, err
		}
		report.Fixtures = append(report.Fixtures, run)
	}
	report.Aggregate = aggregateAndroidCodeDeltaBenchmark(report.Fixtures, report.Strategies)
	return report, nil
}

func benchmarkAndroidCodeDeltaFixture(fixture androidCodeDeltaBenchmarkFixture, strategies []codeDeltaBenchmarkStrategyBuilder, surface string) (androidCodeDeltaBenchmarkFixtureRun, error) {
	baseBytes, err := readAndroidCodeDeltaBenchmarkSurface(fixture.BasePath, surface)
	if err != nil {
		return androidCodeDeltaBenchmarkFixtureRun{}, fmt.Errorf("read fixture %q base: %w", fixture.Name, err)
	}
	candidateBytes, err := readAndroidCodeDeltaBenchmarkSurface(fixture.CandidatePath, surface)
	if err != nil {
		return androidCodeDeltaBenchmarkFixtureRun{}, fmt.Errorf("read fixture %q candidate: %w", fixture.Name, err)
	}
	run := androidCodeDeltaBenchmarkFixtureRun{
		Name:               fixture.Name,
		BasePath:           filepath.Clean(fixture.BasePath),
		CandidatePath:      filepath.Clean(fixture.CandidatePath),
		Surface:            surface,
		BaseSizeBytes:      uint64(len(baseBytes)),
		CandidateSizeBytes: uint64(len(candidateBytes)),
		Strategies:         make([]androidCodeDeltaBenchmarkStrategyRun, 0, len(strategies)),
	}
	for _, strategy := range strategies {
		strategyRun, err := benchmarkAndroidCodeDeltaStrategy(baseBytes, candidateBytes, strategy)
		if err != nil {
			return androidCodeDeltaBenchmarkFixtureRun{}, fmt.Errorf("benchmark fixture %q strategy %s: %w", fixture.Name, strategy.name, err)
		}
		run.Strategies = append(run.Strategies, strategyRun)
		if run.Winner == nil || strategyRun.DeflatedDeltaBytes < run.Winner.DeflatedDeltaBytes {
			run.Winner = &androidCodeDeltaBenchmarkWinner{
				Strategy:           strategyRun.Strategy,
				RawDeltaBytes:      strategyRun.RawDeltaBytes,
				DeflatedDeltaBytes: strategyRun.DeflatedDeltaBytes,
			}
		}
	}
	return run, nil
}

func readAndroidCodeDeltaBenchmarkSurface(path string, surface string) ([]byte, error) {
	bytes, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	switch surface {
	case codeDeltaBenchmarkSurfaceLibapp:
		return bytes, nil
	case codeDeltaBenchmarkSurfaceIsolateSnapshot:
		return extractAndroidSnapshotSurface(bytes, []string{
			"kDartIsolateSnapshotData",
			"kDartIsolateSnapshotInstructions",
		})
	default:
		return nil, fmt.Errorf("unsupported benchmark surface %q", surface)
	}
}

func extractAndroidSnapshotSurface(elfBytes []byte, symbols []string) ([]byte, error) {
	if len(elfBytes) < 4 || !bytes.Equal(elfBytes[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return nil, errors.New("snapshot surface requires an ELF libapp.so")
	}
	file, err := elf.NewFile(bytes.NewReader(elfBytes))
	if err != nil {
		return nil, fmt.Errorf("parse ELF for snapshot surface: %w", err)
	}
	defer file.Close()

	elfSymbols, err := file.Symbols()
	if err != nil {
		return nil, fmt.Errorf("read ELF symbols: %w", err)
	}

	output := make([]byte, 0)
	for _, symbolName := range symbols {
		blob, err := extractAndroidSnapshotSymbolBytes(elfBytes, file, elfSymbols, symbolName)
		if err != nil {
			return nil, err
		}
		output = append(output, blob...)
	}
	return output, nil
}

func extractAndroidSnapshotSymbolBytes(elfBytes []byte, file *elf.File, symbols []elf.Symbol, name string) ([]byte, error) {
	symbol, ok := findAndroidSnapshotSymbol(symbols, name)
	if !ok {
		return nil, fmt.Errorf("ELF symbol %q not found", name)
	}
	if symbol.Size == 0 {
		return nil, fmt.Errorf("ELF symbol %q has zero size", symbol.Name)
	}
	for _, section := range file.Sections {
		if section == nil || section.Type == elf.SHT_NOBITS || section.Size == 0 {
			continue
		}
		if symbol.Value < section.Addr || symbol.Value >= section.Addr+section.Size {
			continue
		}
		relativeOffset := symbol.Value - section.Addr
		if relativeOffset+symbol.Size > section.Size {
			return nil, fmt.Errorf("ELF symbol %q exceeds containing section %q", symbol.Name, section.Name)
		}
		fileOffset := section.Offset + relativeOffset
		if fileOffset+symbol.Size > uint64(len(elfBytes)) {
			return nil, fmt.Errorf("ELF symbol %q exceeds file bounds", symbol.Name)
		}
		return append([]byte(nil), elfBytes[fileOffset:fileOffset+symbol.Size]...), nil
	}
	return nil, fmt.Errorf("ELF symbol %q did not map to a file-backed section", symbol.Name)
}

func findAndroidSnapshotSymbol(symbols []elf.Symbol, name string) (elf.Symbol, bool) {
	for _, symbol := range symbols {
		if symbol.Name == name || symbol.Name == "_"+name {
			return symbol, true
		}
	}
	return elf.Symbol{}, false
}

func benchmarkAndroidCodeDeltaStrategy(baseBytes []byte, candidateBytes []byte, strategy codeDeltaBenchmarkStrategyBuilder) (androidCodeDeltaBenchmarkStrategyRun, error) {
	deltaBytes, summary, err := strategy.build(baseBytes, candidateBytes)
	if err != nil {
		return androidCodeDeltaBenchmarkStrategyRun{}, err
	}
	reconstructed, err := applyCodeDelta(baseBytes, deltaBytes)
	if err != nil {
		return androidCodeDeltaBenchmarkStrategyRun{}, fmt.Errorf("verify delta: %w", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		return androidCodeDeltaBenchmarkStrategyRun{}, errors.New("verified reconstruction does not match candidate")
	}
	streams, err := diagnoseCodeDeltaStreams(deltaBytes)
	if err != nil {
		return androidCodeDeltaBenchmarkStrategyRun{}, fmt.Errorf("diagnose delta streams: %w", err)
	}
	sections, err := diagnoseCodeDeltaSections(baseBytes, candidateBytes, deltaBytes)
	if err != nil {
		return androidCodeDeltaBenchmarkStrategyRun{}, fmt.Errorf("diagnose delta sections: %w", err)
	}
	return androidCodeDeltaBenchmarkStrategyRun{
		Strategy:           strategy.name,
		RawDeltaBytes:      uint64(len(deltaBytes)),
		DeflatedDeltaBytes: uint64(compressedCodeDeltaBlockSize(deltaBytes)),
		Verified:           true,
		Summary:            summary,
		Streams:            streams,
		Sections:           sections,
	}, nil
}

func diagnoseCodeDeltaStreams(deltaBytes []byte) (*codeDeltaStreamDiagnostics, error) {
	reader := bytes.NewReader(deltaBytes)
	magicBytes := make([]byte, len(codeDeltaMagicV1))
	if _, err := reader.Read(magicBytes); err != nil {
		return nil, fmt.Errorf("read delta magic: %w", err)
	}
	magic := string(magicBytes)

	var addTransform *string
	if magic == codeDeltaMagicV14 {
		transformName := "split_one_other_gaps"
		addTransform = &transformName
	} else if magic == codeDeltaMagicV12 || magic == codeDeltaMagicV13 {
		transform, err := binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read transformed add stream marker: %w", err)
		}
		transformName := codeDeltaAddTransformName(byte(transform))
		addTransform = &transformName
	}
	if magic != codeDeltaMagicV7 &&
		magic != codeDeltaMagicV8 &&
		magic != codeDeltaMagicV10 &&
		magic != codeDeltaMagicV11 &&
		magic != codeDeltaMagicV12 &&
		magic != codeDeltaMagicV13 &&
		magic != codeDeltaMagicV14 {
		return nil, nil
	}

	controlRawBytes, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read control stream length: %w", err)
	}
	controlCompressedBytes, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed control stream length: %w", err)
	}
	insertRawBytes, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read insert stream length: %w", err)
	}
	insertCompressedBytes, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed insert stream length: %w", err)
	}
	addRawBytes, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read add stream length: %w", err)
	}
	var addCompressedBytes uint64
	var oneGapRawBytes uint64
	var oneGapCompressedBytes uint64
	var otherGapRawBytes uint64
	var otherGapCompressedBytes uint64
	var otherValueRawBytes uint64
	var otherValueCompressedBytes uint64
	if magic == codeDeltaMagicV14 {
		oneGapRawBytes, err = binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read one-gap add stream length: %w", err)
		}
		oneGapCompressedBytes, err = binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read compressed one-gap add stream length: %w", err)
		}
		otherGapRawBytes, err = binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read other-gap add stream length: %w", err)
		}
		otherGapCompressedBytes, err = binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read compressed other-gap add stream length: %w", err)
		}
		otherValueRawBytes, err = binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read other-value add stream length: %w", err)
		}
		otherValueCompressedBytes, err = binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read compressed other-value add stream length: %w", err)
		}
		addCompressedBytes = oneGapCompressedBytes + otherGapCompressedBytes + otherValueCompressedBytes
	} else {
		addCompressedBytes, err = binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read compressed add stream length: %w", err)
		}
	}
	headerBytes := uint64(len(deltaBytes) - reader.Len())
	if _, err := readCodeDeltaStream(reader, controlCompressedBytes); err != nil {
		return nil, fmt.Errorf("read compressed control stream: %w", err)
	}
	if _, err := readCodeDeltaStream(reader, insertCompressedBytes); err != nil {
		return nil, fmt.Errorf("read compressed insert stream: %w", err)
	}
	if magic == codeDeltaMagicV14 {
		if _, err := readCodeDeltaStream(reader, oneGapCompressedBytes); err != nil {
			return nil, fmt.Errorf("read compressed one-gap add stream: %w", err)
		}
		if _, err := readCodeDeltaStream(reader, otherGapCompressedBytes); err != nil {
			return nil, fmt.Errorf("read compressed other-gap add stream: %w", err)
		}
		if _, err := readCodeDeltaStream(reader, otherValueCompressedBytes); err != nil {
			return nil, fmt.Errorf("read compressed other-value add stream: %w", err)
		}
		addRawBytes = oneGapRawBytes + otherGapRawBytes + otherValueRawBytes
	} else {
		if _, err := readCodeDeltaStream(reader, addCompressedBytes); err != nil {
			return nil, fmt.Errorf("read compressed add stream: %w", err)
		}
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("unexpected trailing split stream bytes: %d", reader.Len())
	}

	compressedPayloadBytes := controlCompressedBytes + insertCompressedBytes + addCompressedBytes
	totalDeltaBytes := uint64(len(deltaBytes))
	return &codeDeltaStreamDiagnostics{
		Magic:                               magic,
		HeaderBytes:                         headerBytes,
		ControlRawBytes:                     controlRawBytes,
		ControlCompressedBytes:              controlCompressedBytes,
		InsertRawBytes:                      insertRawBytes,
		InsertCompressedBytes:               insertCompressedBytes,
		AddRawBytes:                         addRawBytes,
		AddCompressedBytes:                  addCompressedBytes,
		AddTransform:                        addTransform,
		CompressedPayloadBytes:              compressedPayloadBytes,
		CompressedPayloadSharePercent:       percentOf(compressedPayloadBytes, totalDeltaBytes),
		AddCompressedPayloadSharePercent:    percentOf(addCompressedBytes, compressedPayloadBytes),
		AddCompressedTotalDeltaSharePercent: percentOf(addCompressedBytes, totalDeltaBytes),
	}, nil
}

func diagnoseCodeDeltaSections(baseBytes []byte, candidateBytes []byte, deltaBytes []byte) (*codeDeltaSectionDiagnostics, error) {
	sections, isELF, err := parseCodeDeltaELFSections(baseBytes)
	if err != nil {
		return nil, err
	}
	if !isELF {
		return nil, nil
	}
	ops, err := decodeSplitCodeDeltaControlOps(deltaBytes)
	if err != nil {
		return nil, err
	}
	if ops == nil {
		return nil, nil
	}

	summaries := make(map[string]*codeDeltaSectionSummary, len(sections))
	for _, section := range sections {
		changedBytes := countSectionSameOffsetChangedBytes(baseBytes, candidateBytes, section)
		if changedBytes == 0 {
			continue
		}
		summary := sectionSummaryFor(summaries, section)
		summary.SameOffsetChangedBytes = changedBytes
		summary.SameOffsetChangedPct = percentOf(changedBytes, section.Size)
	}

	var addBytes uint64
	var addOps int
	for _, op := range ops {
		if op.Kind != codeDeltaOpAdd && op.Kind != codeDeltaOpSparseAdd {
			continue
		}
		addOps++
		addBytes += op.Length
		for _, section := range sections {
			overlap := overlapBytes(op.BaseOffset, op.Length, section.Offset, section.Size)
			if overlap == 0 {
				continue
			}
			summary := sectionSummaryFor(summaries, section)
			summary.AddBytes += overlap
			summary.AddOps++
		}
	}

	sectionSummaries := make([]codeDeltaSectionSummary, 0, len(summaries))
	var sameOffsetChangedBytes uint64
	for _, summary := range summaries {
		if summary.AddBytes == 0 && summary.SameOffsetChangedBytes == 0 {
			continue
		}
		sameOffsetChangedBytes += summary.SameOffsetChangedBytes
		sectionSummaries = append(sectionSummaries, *summary)
	}
	sort.SliceStable(sectionSummaries, func(left int, right int) bool {
		if sectionSummaries[left].AddBytes != sectionSummaries[right].AddBytes {
			return sectionSummaries[left].AddBytes > sectionSummaries[right].AddBytes
		}
		if sectionSummaries[left].SameOffsetChangedBytes != sectionSummaries[right].SameOffsetChangedBytes {
			return sectionSummaries[left].SameOffsetChangedBytes > sectionSummaries[right].SameOffsetChangedBytes
		}
		return sectionSummaries[left].Name < sectionSummaries[right].Name
	})

	return &codeDeltaSectionDiagnostics{
		Format:                 "elf",
		SectionCount:           len(sections),
		AddBytes:               addBytes,
		AddOps:                 addOps,
		SameOffsetChangedBytes: sameOffsetChangedBytes,
		Sections:               sectionSummaries,
	}, nil
}

func parseCodeDeltaELFSections(elfBytes []byte) ([]codeDeltaELFSection, bool, error) {
	if len(elfBytes) < 4 || !bytes.Equal(elfBytes[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return nil, false, nil
	}
	file, err := elf.NewFile(bytes.NewReader(elfBytes))
	if err != nil {
		return nil, true, fmt.Errorf("parse ELF sections: %w", err)
	}
	defer file.Close()

	sections := make([]codeDeltaELFSection, 0, len(file.Sections))
	for _, section := range file.Sections {
		if section == nil || section.Size == 0 || section.Name == "" {
			continue
		}
		if section.Type == elf.SHT_NOBITS {
			continue
		}
		if section.Offset >= uint64(len(elfBytes)) {
			continue
		}
		size := section.Size
		if section.Offset+size > uint64(len(elfBytes)) {
			size = uint64(len(elfBytes)) - section.Offset
		}
		if size == 0 {
			continue
		}
		sections = append(sections, codeDeltaELFSection{
			Name:   section.Name,
			Offset: section.Offset,
			Size:   size,
		})
	}
	return sections, true, nil
}

func decodeSplitCodeDeltaControlOps(deltaBytes []byte) ([]codeDeltaControlOp, error) {
	reader := bytes.NewReader(deltaBytes)
	magicBytes := make([]byte, len(codeDeltaMagicV1))
	if _, err := reader.Read(magicBytes); err != nil {
		return nil, fmt.Errorf("read delta magic: %w", err)
	}
	magic := string(magicBytes)
	if magic == codeDeltaMagicV12 || magic == codeDeltaMagicV13 {
		if _, err := binary.ReadUvarint(reader); err != nil {
			return nil, fmt.Errorf("read transformed add stream marker: %w", err)
		}
	}
	if magic != codeDeltaMagicV7 &&
		magic != codeDeltaMagicV8 &&
		magic != codeDeltaMagicV10 &&
		magic != codeDeltaMagicV11 &&
		magic != codeDeltaMagicV12 &&
		magic != codeDeltaMagicV13 &&
		magic != codeDeltaMagicV14 {
		return nil, nil
	}

	controlLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read control stream length: %w", err)
	}
	compressedControlLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed control stream length: %w", err)
	}
	if _, err := binary.ReadUvarint(reader); err != nil {
		return nil, fmt.Errorf("read insert stream length: %w", err)
	}
	compressedInsertLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed insert stream length: %w", err)
	}
	if _, err := binary.ReadUvarint(reader); err != nil {
		return nil, fmt.Errorf("read add stream length: %w", err)
	}
	var compressedAddLength uint64
	if magic == codeDeltaMagicV14 {
		if _, err := binary.ReadUvarint(reader); err != nil {
			return nil, fmt.Errorf("read one-gap stream length: %w", err)
		}
		compressedOneGapLength, err := binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read compressed one-gap stream length: %w", err)
		}
		if _, err := binary.ReadUvarint(reader); err != nil {
			return nil, fmt.Errorf("read other-pair stream length: %w", err)
		}
		compressedOtherGapLength, err := binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read compressed other-gap stream length: %w", err)
		}
		if _, err := binary.ReadUvarint(reader); err != nil {
			return nil, fmt.Errorf("read other-value stream length: %w", err)
		}
		compressedOtherValueLength, err := binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read compressed other-value stream length: %w", err)
		}
		compressedAddLength = compressedOneGapLength + compressedOtherGapLength + compressedOtherValueLength
	} else {
		compressedAddLength, err = binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read compressed add stream length: %w", err)
		}
	}

	compressedControl, err := readCodeDeltaStream(reader, compressedControlLength)
	if err != nil {
		return nil, fmt.Errorf("read compressed control stream: %w", err)
	}
	if _, err := readCodeDeltaStream(reader, compressedInsertLength); err != nil {
		return nil, fmt.Errorf("read compressed insert stream: %w", err)
	}
	if _, err := readCodeDeltaStream(reader, compressedAddLength); err != nil {
		return nil, fmt.Errorf("read compressed add stream: %w", err)
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("unexpected split stream trailing bytes: %d", reader.Len())
	}

	controlStream, err := decompressCodeDeltaBodyWithDict(compressedControl, nil, controlLength)
	if err != nil {
		return nil, fmt.Errorf("decompress control stream: %w", err)
	}
	return decodeCodeDeltaControlStream(controlStream)
}

func decodeCodeDeltaControlStream(controlStream []byte) ([]codeDeltaControlOp, error) {
	reader := bytes.NewReader(controlStream)
	opCount, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read control op count: %w", err)
	}
	ops := make([]codeDeltaControlOp, 0, int(opCount))
	for index := uint64(0); index < opCount; index++ {
		kind, err := reader.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read control op kind %d: %w", index, err)
		}
		baseOffset, err := binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read control op base offset %d: %w", index, err)
		}
		length, err := binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read control op length %d: %w", index, err)
		}
		op := codeDeltaControlOp{
			Kind:       kind,
			BaseOffset: baseOffset,
			Length:     length,
		}
		if kind == codeDeltaOpSparseAdd {
			payloadLength, err := binary.ReadUvarint(reader)
			if err != nil {
				return nil, fmt.Errorf("read control sparse add payload length %d: %w", index, err)
			}
			op.PayloadLength = payloadLength
		}
		ops = append(ops, op)
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("unexpected control stream trailing bytes: %d", reader.Len())
	}
	return ops, nil
}

func sectionSummaryFor(summaries map[string]*codeDeltaSectionSummary, section codeDeltaELFSection) *codeDeltaSectionSummary {
	key := fmt.Sprintf("%s@%d", section.Name, section.Offset)
	if summary, ok := summaries[key]; ok {
		return summary
	}
	summary := &codeDeltaSectionSummary{
		Name:   section.Name,
		Offset: section.Offset,
		Size:   section.Size,
	}
	summaries[key] = summary
	return summary
}

func countSectionSameOffsetChangedBytes(baseBytes []byte, candidateBytes []byte, section codeDeltaELFSection) uint64 {
	if section.Offset >= uint64(len(baseBytes)) || section.Offset >= uint64(len(candidateBytes)) {
		return 0
	}
	baseLimit := minUint64(section.Offset+section.Size, uint64(len(baseBytes)))
	candidateLimit := minUint64(section.Offset+section.Size, uint64(len(candidateBytes)))
	limit := minUint64(baseLimit, candidateLimit)
	var changed uint64
	for offset := section.Offset; offset < limit; offset++ {
		if baseBytes[int(offset)] != candidateBytes[int(offset)] {
			changed++
		}
	}
	return changed
}

func overlapBytes(start uint64, length uint64, sectionOffset uint64, sectionSize uint64) uint64 {
	if length == 0 || sectionSize == 0 {
		return 0
	}
	end := start + length
	sectionEnd := sectionOffset + sectionSize
	if end <= sectionOffset || sectionEnd <= start {
		return 0
	}
	return minUint64(end, sectionEnd) - maxUint64(start, sectionOffset)
}

func minUint64(left uint64, right uint64) uint64 {
	if left < right {
		return left
	}
	return right
}

func maxUint64(left uint64, right uint64) uint64 {
	if left > right {
		return left
	}
	return right
}

func codeDeltaAddTransformName(transform byte) string {
	switch transform {
	case codeDeltaAddTransformRaw:
		return "raw"
	case codeDeltaAddTransformDelta:
		return "delta"
	case codeDeltaAddTransformXOR:
		return "xor"
	case codeDeltaAddTransformBitplaneZeroOne:
		return "bitplane_zero_one"
	default:
		return fmt.Sprintf("unknown-%d", transform)
	}
}

func percentOf(part uint64, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

func aggregateAndroidCodeDeltaBenchmark(fixtures []androidCodeDeltaBenchmarkFixtureRun, strategyNames []string) androidCodeDeltaBenchmarkAggregate {
	totalByStrategy := make(map[string]*androidCodeDeltaBenchmarkStrategyTotal, len(strategyNames))
	for _, strategyName := range strategyNames {
		totalByStrategy[strategyName] = &androidCodeDeltaBenchmarkStrategyTotal{
			Strategy: strategyName,
		}
	}
	for _, fixture := range fixtures {
		if fixture.Winner != nil {
			totalByStrategy[fixture.Winner.Strategy].Wins++
		}
		for _, strategy := range fixture.Strategies {
			total := totalByStrategy[strategy.Strategy]
			total.RawDeltaBytes += strategy.RawDeltaBytes
			total.DeflatedDeltaBytes += strategy.DeflatedDeltaBytes
			if strategy.Verified {
				total.VerifiedFixtures++
			}
		}
	}
	totals := make([]androidCodeDeltaBenchmarkStrategyTotal, 0, len(strategyNames))
	for _, strategyName := range strategyNames {
		total := totalByStrategy[strategyName]
		if len(fixtures) > 0 {
			total.AverageDeflatedDeltaBytes = total.DeflatedDeltaBytes / uint64(len(fixtures))
		}
		totals = append(totals, *total)
	}
	sort.SliceStable(totals, func(left int, right int) bool {
		return totals[left].DeflatedDeltaBytes < totals[right].DeflatedDeltaBytes
	})
	aggregate := androidCodeDeltaBenchmarkAggregate{
		FixtureCount: len(fixtures),
		Strategies:   totals,
	}
	if len(totals) > 0 {
		aggregate.Winner = &androidCodeDeltaBenchmarkWinner{
			Strategy:           totals[0].Strategy,
			RawDeltaBytes:      totals[0].RawDeltaBytes,
			DeflatedDeltaBytes: totals[0].DeflatedDeltaBytes,
		}
	}
	return aggregate
}
