package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"soroq/backend/internal/domain"
)

func TestBuildAndApplyCodeDeltaRoundTripsMixedChanges(t *testing.T) {
	t.Parallel()

	baseBytes := buildPatternedCodePayload(220)
	candidateBytes := append([]byte(nil), baseBytes...)
	candidateBytes = append(candidateBytes[:280], append([]byte("INSERT-ONE-DELTA"), candidateBytes[280:]...)...)
	copy(candidateBytes[760:780], []byte("REPLACED-BLOCK-DELTA"))
	candidateBytes = append(candidateBytes[:1330], candidateBytes[1368:]...)

	deltaBytes, summary, err := buildCodeDelta(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDelta() error = %v", err)
	}
	if summary.Strategy != codeDeltaStrategyV15 {
		t.Fatalf("expected default v15 strategy, got %#v", summary)
	}
	if !bytes.HasPrefix(deltaBytes, []byte(codeDeltaMagicV15)) {
		t.Fatalf("expected default v15 delta magic")
	}
	if summary.InsertOps != 1 || summary.InsertedBytes == 0 {
		t.Fatalf("expected default bsdiff payload summary in %#v", summary)
	}

	reconstructed, err := applyCodeDelta(baseBytes, deltaBytes)
	if err != nil {
		t.Fatalf("applyCodeDelta() error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("reconstructed bytes did not match candidate payload")
	}
}

func TestBuildAndApplyCodeDeltaV2FindsOutOfOrderCopies(t *testing.T) {
	t.Parallel()

	block := func(label string) []byte {
		var output bytes.Buffer
		for i := 0; i < 256; i++ {
			output.WriteString(fmt.Sprintf("%s-%04d|", label, i))
		}
		return output.Bytes()
	}
	blockA := block("alpha")
	blockB := block("bravo")
	blockC := block("charlie")
	blockD := block("delta")
	baseBytes := bytes.Join([][]byte{blockA, blockB, blockC, blockD}, nil)
	candidateBytes := bytes.Join([][]byte{
		blockA,
		[]byte("small-literal-change"),
		blockC,
		blockB,
		blockD,
	}, nil)

	v1Bytes, _, err := buildCodeDeltaV1(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV1() error = %v", err)
	}
	v2Bytes, summary, err := buildCodeDeltaV2(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV2() error = %v", err)
	}
	if summary.Strategy != codeDeltaStrategyV2 {
		t.Fatalf("expected v2 strategy, got %#v", summary)
	}
	if len(v2Bytes) >= len(v1Bytes) {
		t.Fatalf("expected v2 delta to beat v1 for reordered blocks: v1=%d v2=%d summary=%#v", len(v1Bytes), len(v2Bytes), summary)
	}

	reconstructed, err := applyCodeDelta(baseBytes, v2Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v2) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v2 reconstructed bytes did not match candidate payload")
	}
}

func TestBuildAndApplyCodeDeltaV5FindsUnalignedSuffixCopies(t *testing.T) {
	t.Parallel()

	block := func(label string) []byte {
		var output bytes.Buffer
		for i := 0; i < 512; i++ {
			output.WriteString(fmt.Sprintf("%s-%04d|", label, i))
		}
		return output.Bytes()
	}
	prefix := bytes.Repeat([]byte("x"), 7)
	blockA := block("alpha")
	blockB := block("bravo")
	blockC := block("charlie")
	baseBytes := bytes.Join([][]byte{prefix, blockA, blockB, blockC}, nil)
	candidateBytes := bytes.Join([][]byte{
		[]byte("candidate-prefix"),
		blockC,
		blockA,
		[]byte("candidate-middle"),
		blockB,
	}, nil)

	v3Bytes, _, err := buildCodeDeltaV3(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV3() error = %v", err)
	}
	v5Bytes, summary, err := buildCodeDeltaV5(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV5() error = %v", err)
	}
	if summary.Strategy != codeDeltaStrategyV5 {
		t.Fatalf("expected v5 strategy, got %#v", summary)
	}
	if len(v5Bytes) >= len(v3Bytes) {
		t.Fatalf("expected v5 to beat v3 on unaligned reordered copies: v3=%d v5=%d summary=%#v", len(v3Bytes), len(v5Bytes), summary)
	}
	reconstructed, err := applyCodeDelta(baseBytes, v5Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v5) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v5 reconstructed bytes did not match candidate payload")
	}
}

func TestBuildAndApplyCodeDeltaV6RoundTripsSuffixCopies(t *testing.T) {
	t.Parallel()

	block := func(label string) []byte {
		var output bytes.Buffer
		for i := 0; i < 512; i++ {
			output.WriteString(fmt.Sprintf("%s-%04d|", label, i))
		}
		return output.Bytes()
	}
	blockA := block("alpha")
	blockB := block("bravo")
	blockC := block("charlie")
	baseBytes := bytes.Join([][]byte{blockA, blockB, blockC}, nil)
	candidateBytes := bytes.Join([][]byte{
		[]byte("candidate-prefix"),
		blockC,
		blockA,
		[]byte("candidate-middle"),
		blockB,
	}, nil)

	v6Bytes, summary, err := buildCodeDeltaV6(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV6() error = %v", err)
	}
	if summary.Strategy != codeDeltaStrategyV6 {
		t.Fatalf("expected v6 strategy, got %#v", summary)
	}
	reconstructed, err := applyCodeDelta(baseBytes, v6Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v6) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v6 reconstructed bytes did not match candidate payload")
	}
}

func TestBuildAndApplyCodeDeltaV7RoundTripsDictionaryCompressedBody(t *testing.T) {
	t.Parallel()

	baseBytes := buildPatternedCodePayload(700)
	candidateBytes := append([]byte(nil), baseBytes...)
	candidateBytes = append(candidateBytes[:4096], append([]byte("dictionary-compressed-insert"), candidateBytes[4096:]...)...)
	for offset := 12000; offset < minInt(len(candidateBytes), 18000); offset++ {
		candidateBytes[offset] = candidateBytes[offset] + byte(offset%17)
	}

	v7Bytes, summary, err := buildCodeDeltaV7(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV7() error = %v", err)
	}
	if summary.Strategy != codeDeltaStrategyV7 {
		t.Fatalf("expected v7 strategy, got %#v", summary)
	}
	reconstructed, err := applyCodeDelta(baseBytes, v7Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v7) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v7 reconstructed bytes did not match candidate payload")
	}
}

func TestBuildAndApplyCodeDeltaV8RoundTripsContextCompressedBody(t *testing.T) {
	t.Parallel()

	baseBytes := buildPatternedCodePayload(800)
	candidateBytes := append([]byte(nil), baseBytes...)
	for offset := 4096; offset < minInt(len(candidateBytes), 18000); offset++ {
		candidateBytes[offset] = candidateBytes[offset] + 1
	}
	insertOffset := minInt(len(candidateBytes), 20000)
	candidateBytes = append(candidateBytes[:insertOffset], append([]byte("v8-context-payload"), candidateBytes[insertOffset:]...)...)

	v8Bytes, summary, err := buildCodeDeltaV8(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV8() error = %v", err)
	}
	if summary.Strategy != codeDeltaStrategyV8 {
		t.Fatalf("expected v8 strategy, got %#v", summary)
	}
	reconstructed, err := applyCodeDelta(baseBytes, v8Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v8) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v8 reconstructed bytes did not match candidate payload")
	}
}

func TestBuildAndApplyCodeDeltaV10RoundTripsOutputCopies(t *testing.T) {
	t.Parallel()

	baseBytes := buildPatternedCodePayload(900)
	repeatedNewBlock := bytes.Repeat([]byte("v10-output-copy-only-block|"), 80)
	candidateBytes := append([]byte(nil), baseBytes[:6000]...)
	candidateBytes = append(candidateBytes, repeatedNewBlock...)
	candidateBytes = append(candidateBytes, baseBytes[6000:18000]...)
	candidateBytes = append(candidateBytes, repeatedNewBlock...)
	candidateBytes = append(candidateBytes, baseBytes[18000:]...)

	v8Bytes, _, err := buildCodeDeltaV8(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV8() error = %v", err)
	}
	v10Bytes, summary, err := buildCodeDeltaV10(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV10() error = %v", err)
	}
	if summary.Strategy != codeDeltaStrategyV10 {
		t.Fatalf("expected v10 strategy, got %#v", summary)
	}
	if summary.OutputCopyOps == 0 || summary.OutputCopiedBytes == 0 {
		t.Fatalf("expected v10 to emit output-copy ops for repeated candidate-side blocks: %#v", summary)
	}
	reconstructed, err := applyCodeDelta(baseBytes, v10Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v10) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v10 reconstructed bytes did not match candidate payload")
	}
	t.Logf("synthetic output-copy raw delta sizes: v8=%d v10=%d", len(v8Bytes), len(v10Bytes))
	t.Logf("synthetic output-copy deflated delta sizes: v8=%d v10=%d", compressedCodeDeltaBlockSize(v8Bytes), compressedCodeDeltaBlockSize(v10Bytes))
}

func TestBuildAndApplyCodeDeltaV11RoundTripsIndexedAddOutputCopies(t *testing.T) {
	t.Parallel()

	baseBytes := buildPatternedCodePayload(1600)
	candidateBytes := append([]byte(nil), baseBytes...)
	repeatedAddBlock := bytes.Repeat([]byte("v11-indexed-add-output-copy|"), 72)
	copy(candidateBytes[4096:4096+len(repeatedAddBlock)], repeatedAddBlock)
	copy(candidateBytes[24000:24000+len(repeatedAddBlock)], repeatedAddBlock)

	v10Bytes, _, err := buildCodeDeltaV10(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV10() error = %v", err)
	}
	v11Bytes, summary, err := buildCodeDeltaV11(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV11() error = %v", err)
	}
	if summary.Strategy != codeDeltaStrategyV11 {
		t.Fatalf("expected v11 strategy, got %#v", summary)
	}
	if summary.OutputCopyOps == 0 || summary.OutputCopiedBytes == 0 {
		t.Fatalf("expected v11 to emit output-copy ops for repeated ADD-region material: %#v", summary)
	}
	reconstructed, err := applyCodeDelta(baseBytes, v11Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v11) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v11 reconstructed bytes did not match candidate payload")
	}
	t.Logf("synthetic indexed output-copy raw delta sizes: v10=%d v11=%d", len(v10Bytes), len(v11Bytes))
	t.Logf("synthetic indexed output-copy deflated delta sizes: v10=%d v11=%d", compressedCodeDeltaBlockSize(v10Bytes), compressedCodeDeltaBlockSize(v11Bytes))
}

func TestBuildAndApplyCodeDeltaV12RoundTripsSparseAddOutputCopies(t *testing.T) {
	t.Parallel()

	baseBytes := buildPatternedCodePayload(1600)
	candidateBytes := append([]byte(nil), baseBytes...)
	for offset := 4096; offset < 20000; offset += 127 {
		candidateBytes[offset] = candidateBytes[offset] + 7
	}
	repeatedAddBlock := bytes.Repeat([]byte("v12-sparse-indexed-output-copy|"), 64)
	copy(candidateBytes[24000:24000+len(repeatedAddBlock)], repeatedAddBlock)
	copy(candidateBytes[36000:36000+len(repeatedAddBlock)], repeatedAddBlock)

	v11Bytes, _, err := buildCodeDeltaV11(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV11() error = %v", err)
	}
	v12Bytes, summary, err := buildCodeDeltaV12(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV12() error = %v", err)
	}
	if summary.Strategy != codeDeltaStrategyV12 {
		t.Fatalf("expected v12 strategy, got %#v", summary)
	}
	if summary.SparseAddOps == 0 && summary.OutputCopyOps == 0 {
		t.Fatalf("expected v12 to emit compact sparse or output-copy ops: %#v", summary)
	}
	reconstructed, err := applyCodeDelta(baseBytes, v12Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v12) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v12 reconstructed bytes did not match candidate payload")
	}
	t.Logf("synthetic sparse indexed output-copy raw delta sizes: v11=%d v12=%d", len(v11Bytes), len(v12Bytes))
	t.Logf("synthetic sparse indexed output-copy deflated delta sizes: v11=%d v12=%d", compressedCodeDeltaBlockSize(v11Bytes), compressedCodeDeltaBlockSize(v12Bytes))
}

func TestBuildAndApplyCodeDeltaV13RoundTripsBitplaneAddTransform(t *testing.T) {
	t.Parallel()

	baseBytes := buildPatternedCodePayload(2600)
	candidateBytes := append([]byte(nil), baseBytes...)
	for offset := 4096; offset < 42000; offset++ {
		if offset%7 == 0 || offset%11 == 0 {
			candidateBytes[offset] = candidateBytes[offset] + 1
		}
	}
	repeatedAddBlock := bytes.Repeat([]byte("v13-bitplane-indexed-output-copy|"), 64)
	copy(candidateBytes[44000:44000+len(repeatedAddBlock)], repeatedAddBlock)
	copy(candidateBytes[62000:62000+len(repeatedAddBlock)], repeatedAddBlock)

	v13Bytes, summary, err := buildCodeDeltaV13(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV13() error = %v", err)
	}
	if summary.Strategy != codeDeltaStrategyV13 {
		t.Fatalf("expected v13 strategy, got %#v", summary)
	}
	if !bytes.HasPrefix(v13Bytes, []byte(codeDeltaMagicV13)) {
		t.Fatalf("expected v13 magic")
	}
	reconstructed, err := applyCodeDelta(baseBytes, v13Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v13) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v13 reconstructed bytes did not match candidate payload")
	}
}

func TestBuildAndApplyCodeDeltaV14RoundTripsSplitAddStreams(t *testing.T) {
	t.Parallel()

	baseBytes := buildPatternedCodePayload(5000)
	candidateBytes := append([]byte(nil), baseBytes...)
	for offset := 8192; offset < 72000; offset++ {
		switch {
		case offset%5 == 0:
			candidateBytes[offset] = candidateBytes[offset] + 1
		case offset%97 == 0:
			candidateBytes[offset] = candidateBytes[offset] + 129
		case offset%193 == 0:
			candidateBytes[offset] = candidateBytes[offset] - 1
		}
	}
	repeatedAddBlock := bytes.Repeat([]byte("v14-split-add-streams|"), 96)
	copy(candidateBytes[76000:76000+len(repeatedAddBlock)], repeatedAddBlock)
	copy(candidateBytes[98000:98000+len(repeatedAddBlock)], repeatedAddBlock)

	v13Bytes, _, err := buildCodeDeltaV13(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV13() error = %v", err)
	}
	v14Bytes, summary, err := buildCodeDeltaV14(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV14() error = %v", err)
	}
	if summary.Strategy != codeDeltaStrategyV14 {
		t.Fatalf("expected v14 strategy, got %#v", summary)
	}
	if !bytes.HasPrefix(v14Bytes, []byte(codeDeltaMagicV14)) {
		t.Fatalf("expected v14 magic")
	}
	reconstructed, err := applyCodeDelta(baseBytes, v14Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v14) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v14 reconstructed bytes did not match candidate payload")
	}
	if len(v14Bytes) >= len(v13Bytes) {
		t.Fatalf("expected v14 to beat v13 for split-add fixture: v13=%d v14=%d", len(v13Bytes), len(v14Bytes))
	}
}

func TestBuildAndApplyCodeDeltaV15RoundTripsBsdiff(t *testing.T) {
	t.Parallel()

	baseBytes := buildPatternedCodePayload(5000)
	candidateBytes := append([]byte(nil), baseBytes...)
	copy(candidateBytes[1024:1024+len("soroq-v15-bs-diff")], "soroq-v15-bs-diff")
	copy(candidateBytes[64000:64000+len("tiny-title-change")], "tiny-title-change")
	for offset := 96000; offset < 128000; offset += 97 {
		candidateBytes[offset] = candidateBytes[offset] + byte(offset%251)
	}

	v15Bytes, summary, err := buildCodeDeltaV15(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV15() error = %v", err)
	}
	if summary.Strategy != codeDeltaStrategyV15 {
		t.Fatalf("expected v15 strategy, got %#v", summary)
	}
	if !bytes.HasPrefix(v15Bytes, []byte(codeDeltaMagicV15)) {
		t.Fatalf("expected v15 magic")
	}
	reconstructed, err := applyCodeDelta(baseBytes, v15Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v15) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v15 reconstructed bytes did not match candidate payload")
	}
}

func TestCodeDeltaAddTransformBitplaneZeroOneRoundTrips(t *testing.T) {
	t.Parallel()

	addStream := make([]byte, 0, 8192)
	state := uint32(0x5eed1234)
	for index := 0; index < 8192; index++ {
		state = state*1664525 + 1013904223
		bucket := state % 100
		switch {
		case bucket < 82:
			addStream = append(addStream, 0)
		case bucket < 97:
			addStream = append(addStream, 1)
		case bucket == 97:
			addStream = append(addStream, 129)
		default:
			addStream = append(addStream, byte(state>>24))
		}
	}
	transformed, err := transformCodeDeltaAddStream(addStream, codeDeltaAddTransformBitplaneZeroOne)
	if err != nil {
		t.Fatalf("transformCodeDeltaAddStream(bitplane) error = %v", err)
	}
	restored, err := restoreCodeDeltaAddStream(transformed, codeDeltaAddTransformBitplaneZeroOne)
	if err != nil {
		t.Fatalf("restoreCodeDeltaAddStream(bitplane) error = %v", err)
	}
	if !bytes.Equal(restored, addStream) {
		t.Fatalf("restored bitplane add stream mismatch")
	}
	rawCompressed, err := compressCodeDeltaBodyWithDict(addStream, nil)
	if err != nil {
		t.Fatalf("compress raw add stream error = %v", err)
	}
	transformedCompressed, err := compressCodeDeltaBodyWithDict(transformed, nil)
	if err != nil {
		t.Fatalf("compress transformed add stream error = %v", err)
	}
	if len(transformedCompressed) >= len(rawCompressed) {
		t.Fatalf("expected bitplane transform to help this low-entropy stream: raw=%d transformed=%d", len(rawCompressed), len(transformedCompressed))
	}
}

func TestCodeDeltaAddTransformOneOtherGapsRoundTrips(t *testing.T) {
	t.Parallel()

	addStream := make([]byte, 0, 12000)
	state := uint32(0x5eedbeef)
	for index := 0; index < 12000; index++ {
		state = state*1664525 + 1013904223
		bucket := state % 1000
		switch {
		case bucket < 805:
			addStream = append(addStream, 0)
		case bucket < 975:
			addStream = append(addStream, 1)
		case bucket < 985:
			addStream = append(addStream, 129)
		default:
			addStream = append(addStream, byte(state>>24))
		}
	}
	oneGaps, otherGaps, otherValues := transformCodeDeltaAddStreamOneOtherGaps(addStream)
	restored, err := restoreCodeDeltaAddStreamOneOtherGaps(uint64(len(addStream)), oneGaps, otherGaps, otherValues)
	if err != nil {
		t.Fatalf("restoreCodeDeltaAddStreamOneOtherGaps() error = %v", err)
	}
	if !bytes.Equal(restored, addStream) {
		t.Fatalf("restored one/other add stream mismatch")
	}
}

func TestResolveCodeDeltaBuildStrategyAllowsExplicitV13(t *testing.T) {
	t.Parallel()

	strategy, err := resolveCodeDeltaBuildStrategy("v13")
	if err != nil {
		t.Fatalf("resolveCodeDeltaBuildStrategy(v13) error = %v", err)
	}
	if strategy.name != codeDeltaStrategyV13 {
		t.Fatalf("expected v13 strategy, got %#v", strategy)
	}
	aliasStrategy, err := resolveCodeDeltaBuildStrategy(codeDeltaStrategyV13)
	if err != nil {
		t.Fatalf("resolveCodeDeltaBuildStrategy(v13 alias) error = %v", err)
	}
	if aliasStrategy.name != codeDeltaStrategyV13 {
		t.Fatalf("expected v13 alias strategy, got %#v", aliasStrategy)
	}
}

func TestResolveCodeDeltaBuildStrategyAllowsExplicitV14(t *testing.T) {
	t.Parallel()

	strategy, err := resolveCodeDeltaBuildStrategy("v14")
	if err != nil {
		t.Fatalf("resolveCodeDeltaBuildStrategy(v14) error = %v", err)
	}
	if strategy.name != codeDeltaStrategyV14 {
		t.Fatalf("expected v14 strategy, got %#v", strategy)
	}
	aliasStrategy, err := resolveCodeDeltaBuildStrategy(codeDeltaStrategyV14)
	if err != nil {
		t.Fatalf("resolveCodeDeltaBuildStrategy(v14 alias) error = %v", err)
	}
	if aliasStrategy.name != codeDeltaStrategyV14 {
		t.Fatalf("expected v14 alias strategy, got %#v", aliasStrategy)
	}
}

func TestCoalesceCodeDeltaOpsDoesNotMergeOutputCopies(t *testing.T) {
	t.Parallel()

	baseBytes := []byte{}
	ops := []codeDeltaOp{
		{
			Kind:    codeDeltaOpInsert,
			Length:  uint64(len("copy")),
			Literal: []byte("copy"),
		},
		{
			Kind:       codeDeltaOpOutputCopy,
			BaseOffset: 0,
			Length:     uint64(len("copy")),
		},
		{
			Kind:       codeDeltaOpOutputCopy,
			BaseOffset: uint64(len("copy")),
			Length:     uint64(len("copy")),
		},
	}

	coalesced := coalesceCodeDeltaOps(ops)
	if len(coalesced) != len(ops) {
		t.Fatalf("output-copy ops must remain independent, got %#v", coalesced)
	}

	deltaBytes, _, err := encodeSplitContextCodeDelta(
		codeDeltaMagicV11,
		baseBytes,
		coalesced,
		summarizeCodeDelta(baseBytes, coalesced),
	)
	if err != nil {
		t.Fatalf("encodeSplitContextCodeDelta() error = %v", err)
	}
	reconstructed, err := applyCodeDelta(baseBytes, deltaBytes)
	if err != nil {
		t.Fatalf("applyCodeDelta() error = %v", err)
	}
	if !bytes.Equal(reconstructed, []byte("copycopycopy")) {
		t.Fatalf("unexpected reconstructed payload: %q", reconstructed)
	}
}

func TestBuildAndApplyCodeDeltaV3UsesAddForDeflatableChanges(t *testing.T) {
	t.Parallel()

	baseBytes := buildPatternedCodePayload(700)
	candidateBytes := append([]byte(nil), baseBytes...)
	for offset := 4096; offset < minInt(len(candidateBytes), 12000); offset++ {
		candidateBytes[offset] = candidateBytes[offset] + 1
	}

	v2Bytes, _, err := buildCodeDeltaV2(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV2() error = %v", err)
	}
	v3Bytes, summary, err := buildCodeDeltaV3(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV3() error = %v", err)
	}
	if summary.Strategy != codeDeltaStrategyV3 {
		t.Fatalf("expected v3 strategy, got %#v", summary)
	}
	if summary.AddOps == 0 || summary.AddedBytes == 0 {
		t.Fatalf("expected v3 to emit add ops for deflatable same-offset changes: %#v", summary)
	}
	if compressedCodeDeltaBlockSize(v3Bytes) >= compressedCodeDeltaBlockSize(v2Bytes) {
		t.Fatalf("expected v3 to beat v2 after deflate: v2=%d v3=%d summary=%#v", compressedCodeDeltaBlockSize(v2Bytes), compressedCodeDeltaBlockSize(v3Bytes), summary)
	}

	reconstructed, err := applyCodeDelta(baseBytes, v3Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v3) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v3 reconstructed bytes did not match candidate payload")
	}
}

func TestBuildCodeDeltaV5RealFixtureFromEnv(t *testing.T) {
	basePath := os.Getenv("SOROQ_REAL_LIBAPP_BASE")
	candidatePath := os.Getenv("SOROQ_REAL_LIBAPP_CANDIDATE")
	if strings.TrimSpace(basePath) == "" || strings.TrimSpace(candidatePath) == "" {
		t.Skip("set SOROQ_REAL_LIBAPP_BASE and SOROQ_REAL_LIBAPP_CANDIDATE to benchmark a real libapp.so pair")
	}

	baseBytes, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatalf("read base fixture: %v", err)
	}
	candidateBytes, err := os.ReadFile(candidatePath)
	if err != nil {
		t.Fatalf("read candidate fixture: %v", err)
	}

	v3Bytes, v3Summary, err := buildCodeDeltaV3(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV3() error = %v", err)
	}
	v5Bytes, v5Summary, err := buildCodeDeltaV5(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV5() error = %v", err)
	}
	reconstructed, err := applyCodeDelta(baseBytes, v5Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v5) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v5 reconstructed bytes did not match candidate payload")
	}

	v3Compressed := compressedCodeDeltaBlockSize(v3Bytes)
	v5Compressed := compressedCodeDeltaBlockSize(v5Bytes)
	t.Logf("real fixture raw delta sizes: v3=%d v5=%d", len(v3Bytes), len(v5Bytes))
	t.Logf("real fixture deflated delta sizes: v3=%d v5=%d", v3Compressed, v5Compressed)
	t.Logf("real fixture v3 summary: %#v", v3Summary)
	t.Logf("real fixture v5 summary: %#v", v5Summary)
	if v5Compressed >= v3Compressed {
		t.Fatalf("expected v5 to beat v3 after deflate on the real fixture: v3=%d v5=%d", v3Compressed, v5Compressed)
	}
}

func TestBuildCodeDeltaV6RealFixtureFromEnv(t *testing.T) {
	basePath := os.Getenv("SOROQ_REAL_LIBAPP_BASE")
	candidatePath := os.Getenv("SOROQ_REAL_LIBAPP_CANDIDATE")
	if strings.TrimSpace(basePath) == "" || strings.TrimSpace(candidatePath) == "" {
		t.Skip("set SOROQ_REAL_LIBAPP_BASE and SOROQ_REAL_LIBAPP_CANDIDATE to benchmark a real libapp.so pair")
	}

	baseBytes, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatalf("read base fixture: %v", err)
	}
	candidateBytes, err := os.ReadFile(candidatePath)
	if err != nil {
		t.Fatalf("read candidate fixture: %v", err)
	}

	v5Bytes, v5Summary, err := buildCodeDeltaV5(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV5() error = %v", err)
	}
	v6Bytes, v6Summary, err := buildCodeDeltaV6(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV6() error = %v", err)
	}
	reconstructed, err := applyCodeDelta(baseBytes, v6Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v6) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v6 reconstructed bytes did not match candidate payload")
	}

	v5Compressed := compressedCodeDeltaBlockSize(v5Bytes)
	v6Compressed := compressedCodeDeltaBlockSize(v6Bytes)
	t.Logf("real fixture raw delta sizes: v5=%d v6=%d", len(v5Bytes), len(v6Bytes))
	t.Logf("real fixture deflated delta sizes: v5=%d v6=%d", v5Compressed, v6Compressed)
	t.Logf("real fixture v5 summary: %#v", v5Summary)
	t.Logf("real fixture v6 summary: %#v", v6Summary)
	if v6Compressed >= v5Compressed {
		t.Fatalf("expected v6 to beat v5 after deflate on the real fixture: v5=%d v6=%d", v5Compressed, v6Compressed)
	}
}

func TestBuildCodeDeltaV7RealFixtureFromEnv(t *testing.T) {
	basePath := os.Getenv("SOROQ_REAL_LIBAPP_BASE")
	candidatePath := os.Getenv("SOROQ_REAL_LIBAPP_CANDIDATE")
	if strings.TrimSpace(basePath) == "" || strings.TrimSpace(candidatePath) == "" {
		t.Skip("set SOROQ_REAL_LIBAPP_BASE and SOROQ_REAL_LIBAPP_CANDIDATE to benchmark a real libapp.so pair")
	}

	baseBytes, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatalf("read base fixture: %v", err)
	}
	candidateBytes, err := os.ReadFile(candidatePath)
	if err != nil {
		t.Fatalf("read candidate fixture: %v", err)
	}

	v6Bytes, v6Summary, err := buildCodeDeltaV6(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV6() error = %v", err)
	}
	v7Bytes, v7Summary, err := buildCodeDeltaV7(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV7() error = %v", err)
	}
	reconstructed, err := applyCodeDelta(baseBytes, v7Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v7) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v7 reconstructed bytes did not match candidate payload")
	}

	v6Compressed := compressedCodeDeltaBlockSize(v6Bytes)
	v7Compressed := compressedCodeDeltaBlockSize(v7Bytes)
	t.Logf("real fixture raw delta sizes: v6=%d v7=%d", len(v6Bytes), len(v7Bytes))
	t.Logf("real fixture deflated delta sizes: v6=%d v7=%d", v6Compressed, v7Compressed)
	t.Logf("real fixture v6 summary: %#v", v6Summary)
	t.Logf("real fixture v7 summary: %#v", v7Summary)
	if v7Compressed >= v6Compressed {
		t.Fatalf("expected v7 to beat v6 after deflate on the real fixture: v6=%d v7=%d", v6Compressed, v7Compressed)
	}
}

func TestBuildCodeDeltaV8RealFixtureFromEnv(t *testing.T) {
	basePath := os.Getenv("SOROQ_REAL_LIBAPP_BASE")
	candidatePath := os.Getenv("SOROQ_REAL_LIBAPP_CANDIDATE")
	if strings.TrimSpace(basePath) == "" || strings.TrimSpace(candidatePath) == "" {
		t.Skip("set SOROQ_REAL_LIBAPP_BASE and SOROQ_REAL_LIBAPP_CANDIDATE to benchmark a real libapp.so pair")
	}

	baseBytes, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatalf("read base fixture: %v", err)
	}
	candidateBytes, err := os.ReadFile(candidatePath)
	if err != nil {
		t.Fatalf("read candidate fixture: %v", err)
	}

	v7Bytes, v7Summary, err := buildCodeDeltaV7(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV7() error = %v", err)
	}
	v8Bytes, v8Summary, err := buildCodeDeltaV8(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV8() error = %v", err)
	}
	reconstructed, err := applyCodeDelta(baseBytes, v8Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v8) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v8 reconstructed bytes did not match candidate payload")
	}

	v7Compressed := compressedCodeDeltaBlockSize(v7Bytes)
	v8Compressed := compressedCodeDeltaBlockSize(v8Bytes)
	t.Logf("real fixture raw delta sizes: v7=%d v8=%d", len(v7Bytes), len(v8Bytes))
	t.Logf("real fixture deflated delta sizes: v7=%d v8=%d", v7Compressed, v8Compressed)
	t.Logf("real fixture v7 summary: %#v", v7Summary)
	t.Logf("real fixture v8 summary: %#v", v8Summary)
	if v8Compressed >= v7Compressed {
		t.Fatalf("expected v8 to beat v7 after deflate on the real fixture: v7=%d v8=%d", v7Compressed, v8Compressed)
	}
}

func TestBuildCodeDeltaV2RealFixtureFromEnv(t *testing.T) {
	basePath := os.Getenv("SOROQ_REAL_LIBAPP_BASE")
	candidatePath := os.Getenv("SOROQ_REAL_LIBAPP_CANDIDATE")
	if strings.TrimSpace(basePath) == "" || strings.TrimSpace(candidatePath) == "" {
		t.Skip("set SOROQ_REAL_LIBAPP_BASE and SOROQ_REAL_LIBAPP_CANDIDATE to benchmark a real libapp.so pair")
	}

	baseBytes, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatalf("read base fixture: %v", err)
	}
	candidateBytes, err := os.ReadFile(candidatePath)
	if err != nil {
		t.Fatalf("read candidate fixture: %v", err)
	}

	v1Bytes, v1Summary, err := buildCodeDeltaV1(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDelta(v1) error = %v", err)
	}
	v2Bytes, v2Summary, err := buildCodeDeltaV2(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV2() error = %v", err)
	}
	reconstructed, err := applyCodeDelta(baseBytes, v2Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v2) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v2 reconstructed bytes did not match candidate payload")
	}

	t.Logf("real fixture delta sizes: v1=%d v2=%d", len(v1Bytes), len(v2Bytes))
	t.Logf("real fixture v1 summary: %#v", v1Summary)
	t.Logf("real fixture v2 summary: %#v", v2Summary)
	if len(v2Bytes) >= len(v1Bytes) {
		t.Fatalf("expected v2 to beat v1 on the real fixture: v1=%d v2=%d", len(v1Bytes), len(v2Bytes))
	}
}

func TestBuildCodeDeltaV3RealFixtureFromEnv(t *testing.T) {
	basePath := os.Getenv("SOROQ_REAL_LIBAPP_BASE")
	candidatePath := os.Getenv("SOROQ_REAL_LIBAPP_CANDIDATE")
	if strings.TrimSpace(basePath) == "" || strings.TrimSpace(candidatePath) == "" {
		t.Skip("set SOROQ_REAL_LIBAPP_BASE and SOROQ_REAL_LIBAPP_CANDIDATE to benchmark a real libapp.so pair")
	}

	baseBytes, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatalf("read base fixture: %v", err)
	}
	candidateBytes, err := os.ReadFile(candidatePath)
	if err != nil {
		t.Fatalf("read candidate fixture: %v", err)
	}

	v2Bytes, v2Summary, err := buildCodeDeltaV2(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV2() error = %v", err)
	}
	v3Bytes, v3Summary, err := buildCodeDeltaV3(baseBytes, candidateBytes)
	if err != nil {
		t.Fatalf("buildCodeDeltaV3() error = %v", err)
	}
	reconstructed, err := applyCodeDelta(baseBytes, v3Bytes)
	if err != nil {
		t.Fatalf("applyCodeDelta(v3) error = %v", err)
	}
	if !bytes.Equal(reconstructed, candidateBytes) {
		t.Fatalf("v3 reconstructed bytes did not match candidate payload")
	}

	v2Compressed := compressedCodeDeltaBlockSize(v2Bytes)
	v3Compressed := compressedCodeDeltaBlockSize(v3Bytes)
	t.Logf("real fixture raw delta sizes: v2=%d v3=%d", len(v2Bytes), len(v3Bytes))
	t.Logf("real fixture deflated delta sizes: v2=%d v3=%d", v2Compressed, v3Compressed)
	t.Logf("real fixture v2 summary: %#v", v2Summary)
	t.Logf("real fixture v3 summary: %#v", v3Summary)
	if v3Compressed >= v2Compressed {
		t.Fatalf("expected v3 to beat v2 after deflate on the real fixture: v2=%d v3=%d", v2Compressed, v3Compressed)
	}
}

func TestBuildAndroidCodePatchBundleGeneratesVerifiedBundle(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseArtifactPath := filepath.Join(tempDir, "base.apk")
	candidateArtifactPath := filepath.Join(tempDir, "candidate.apk")
	baseLinkMetadataPath := filepath.Join(tempDir, "base-link.tsv")
	candidateLinkMetadataPath := filepath.Join(tempDir, "candidate-link.tsv")
	baseLinkMetadata := []byte("schema_version\tsnapshot\n1\tisolate\n")
	candidateLinkMetadata := []byte("schema_version\tsnapshot\n1\tisolate\n")
	if err := os.WriteFile(baseLinkMetadataPath, baseLinkMetadata, 0o644); err != nil {
		t.Fatalf("WriteFile(base link metadata) error = %v", err)
	}
	if err := os.WriteFile(candidateLinkMetadataPath, candidateLinkMetadata, 0o644); err != nil {
		t.Fatalf("WriteFile(candidate link metadata) error = %v", err)
	}
	metadata := testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "manual", "")

	baseArm64 := buildPatternedCodePayload(180)
	candidateArm64 := append([]byte(nil), baseArm64...)
	copy(candidateArm64[256:272], []byte("PATCHED-ARM64-01"))
	candidateArm64 = append(candidateArm64[:900], append([]byte("ARM64-INSERT"), candidateArm64[900:]...)...)

	baseX64 := buildPatternedCodePayload(160)
	candidateX64 := append([]byte(nil), baseX64...)
	candidateX64 = append(candidateX64[:640], candidateX64[676:]...)
	copy(candidateX64[96:112], []byte("PATCHED-X64-0001"))

	writeTestAndroidArtifact(t, baseArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         string(baseArm64),
		"lib/x86_64/libapp.so":                            string(baseX64),
		"lib/arm64-v8a/libflutter.so":                     "shared-flutter",
		"lib/x86_64/libflutter.so":                        "shared-flutter",
	})
	writeTestAndroidArtifact(t, candidateArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         string(candidateArm64),
		"lib/x86_64/libapp.so":                            string(candidateX64),
		"lib/arm64-v8a/libflutter.so":                     "shared-flutter",
		"lib/x86_64/libflutter.so":                        "shared-flutter",
	})

	baseSnapshot, err := captureAndroidReleaseSnapshot(baseArtifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(base) error = %v", err)
	}
	baseSnapshot.AOTLinkMetadata = []androidAOTLinkMetadataDescriptor{{
		Snapshot:  "isolate",
		Path:      baseLinkMetadataPath,
		Source:    "release_retained",
		SHA256:    sha256Hex(baseLinkMetadata),
		SizeBytes: uint64(len(baseLinkMetadata)),
	}}
	baseSnapshotPath := filepath.Join(tempDir, "base.json")
	writeTestJSONFile(t, baseSnapshotPath, baseSnapshot)

	candidateSnapshot, err := captureAndroidReleaseSnapshot(candidateArtifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(candidate) error = %v", err)
	}
	candidateSnapshot.AOTLinkMetadata = []androidAOTLinkMetadataDescriptor{{
		Snapshot:  "isolate",
		Path:      candidateLinkMetadataPath,
		Source:    "candidate_build",
		SHA256:    sha256Hex(candidateLinkMetadata),
		SizeBytes: uint64(len(candidateLinkMetadata)),
	}}
	candidateSnapshotPath := filepath.Join(tempDir, "candidate.json")
	writeTestJSONFile(t, candidateSnapshotPath, candidateSnapshot)

	codePlan, err := prepareAndroidCodePatchPlan(androidCodePatchPlanOptions{
		BaseSnapshotPath:      baseSnapshotPath,
		CandidateSnapshotPath: candidateSnapshotPath,
		ReleaseID:             "release-android-1",
		WorkspaceOut:          filepath.Join(tempDir, "workspace"),
	})
	if err != nil {
		t.Fatalf("prepareAndroidCodePatchPlan() error = %v", err)
	}
	if !codePlan.Ready {
		t.Fatalf("expected code plan to be ready: %#v", codePlan)
	}
	codePlanPath := filepath.Join(tempDir, "code-plan.json")
	writeTestJSONFile(t, codePlanPath, codePlan)

	report, bundleBytes, err := buildAndroidCodePatchBundle(androidCodePatchBuildOptions{
		CodePlanPath: codePlanPath,
		PatchID:      "code-patch-1",
		PatchNumber:  7,
		OutputPath:   filepath.Join(tempDir, "code-patch.zip"),
		SeedBase64:   base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)),
		KeyID:        "dev-code",
	})
	if err != nil {
		t.Fatalf("buildAndroidCodePatchBundle() error = %v", err)
	}
	if !report.Ready {
		t.Fatalf("expected report to be ready: %#v", report)
	}
	if !report.ManifestSigned {
		t.Fatalf("expected manifest signing to be enabled: %#v", report)
	}
	if len(report.Payloads) != 2 {
		t.Fatalf("expected 2 code payload reports, got %#v", report.Payloads)
	}
	if len(report.BaseAOTLinkMetadata) != 1 || report.BaseAOTLinkMetadata[0].Source != "release_retained" {
		t.Fatalf("expected base retained link metadata in report: %#v", report.BaseAOTLinkMetadata)
	}

	manifest, artifactBytes, overlayFiles := parseBuiltPatchBundle(t, bundleBytes)
	if manifest.Kind != domain.PatchKindExperimentalNativeAOT {
		t.Fatalf("expected experimental_native_aot patch kind, got %q", manifest.Kind)
	}
	if manifest.Signature == nil || strings.TrimSpace(*manifest.Signature) == "" {
		t.Fatalf("expected bundle manifest signature: %#v", manifest)
	}
	if len(overlayFiles) != 0 {
		t.Fatalf("expected no overlay files for code patch bundle, got %#v", overlayFiles)
	}
	if sha256Hex(artifactBytes) != manifest.Artifact.SHA256 {
		t.Fatalf("expected manifest artifact SHA to match artifact.bin")
	}

	artifactMetadata, deltaFiles := parseCodePatchArtifact(t, artifactBytes)
	if artifactMetadata.Strategy != codeDeltaStrategyV15 {
		t.Fatalf("unexpected code delta strategy: %#v", artifactMetadata)
	}
	if len(artifactMetadata.BaseAOTLinkMetadata) != 1 || artifactMetadata.BaseAOTLinkMetadata[0].SHA256 != sha256Hex(baseLinkMetadata) {
		t.Fatalf("expected base retained link metadata in artifact metadata: %#v", artifactMetadata.BaseAOTLinkMetadata)
	}
	if len(artifactMetadata.Payloads) != 2 {
		t.Fatalf("expected 2 artifact payloads, got %#v", artifactMetadata.Payloads)
	}
	if len(deltaFiles) != 2 {
		t.Fatalf("expected 2 delta files, got %d", len(deltaFiles))
	}

	baseByPath := map[string][]byte{
		"lib/arm64-v8a/libapp.so": baseArm64,
		"lib/x86_64/libapp.so":    baseX64,
	}
	candidateByPath := map[string][]byte{
		"lib/arm64-v8a/libapp.so": candidateArm64,
		"lib/x86_64/libapp.so":    candidateX64,
	}
	for _, payload := range artifactMetadata.Payloads {
		deltaBytes, ok := deltaFiles[payload.DeltaPath]
		if !ok {
			t.Fatalf("missing delta payload %q", payload.DeltaPath)
		}
		reconstructed, err := applyCodeDelta(baseByPath[payload.Path], deltaBytes)
		if err != nil {
			t.Fatalf("applyCodeDelta(%q) error = %v", payload.Path, err)
		}
		if !bytes.Equal(reconstructed, candidateByPath[payload.Path]) {
			t.Fatalf("reconstructed payload mismatch for %q", payload.Path)
		}
	}

	v11Report, v11BundleBytes, err := buildAndroidCodePatchBundle(androidCodePatchBuildOptions{
		CodePlanPath:      codePlanPath,
		PatchID:           "code-patch-v11",
		PatchNumber:       8,
		OutputPath:        filepath.Join(tempDir, "code-patch-v11.zip"),
		SeedBase64:        base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{10}, 32)),
		KeyID:             "dev-code-v11",
		CodeDeltaStrategy: "v11",
	})
	if err != nil {
		t.Fatalf("buildAndroidCodePatchBundle(v11) error = %v", err)
	}
	if !v11Report.Ready || len(v11Report.Payloads) != 2 {
		t.Fatalf("expected v11 report to be ready with payloads: %#v", v11Report)
	}
	for _, payload := range v11Report.Payloads {
		if payload.Strategy != codeDeltaStrategyV11 {
			t.Fatalf("expected v11 report payload strategy, got %#v", payload)
		}
	}

	_, v11ArtifactBytes, _ := parseBuiltPatchBundle(t, v11BundleBytes)
	v11ArtifactMetadata, v11DeltaFiles := parseCodePatchArtifact(t, v11ArtifactBytes)
	if v11ArtifactMetadata.Strategy != codeDeltaStrategyV11 {
		t.Fatalf("unexpected v11 code delta strategy: %#v", v11ArtifactMetadata)
	}
	if len(v11ArtifactMetadata.Payloads) != 2 {
		t.Fatalf("expected 2 v11 artifact payloads, got %#v", v11ArtifactMetadata.Payloads)
	}
	for _, payload := range v11ArtifactMetadata.Payloads {
		deltaBytes, ok := v11DeltaFiles[payload.DeltaPath]
		if !ok {
			t.Fatalf("missing v11 delta payload %q", payload.DeltaPath)
		}
		if !bytes.HasPrefix(deltaBytes, []byte(codeDeltaMagicV11)) {
			t.Fatalf("expected v11 delta magic for %q", payload.Path)
		}
		reconstructed, err := applyCodeDelta(baseByPath[payload.Path], deltaBytes)
		if err != nil {
			t.Fatalf("applyCodeDelta(v11 %q) error = %v", payload.Path, err)
		}
		if !bytes.Equal(reconstructed, candidateByPath[payload.Path]) {
			t.Fatalf("v11 reconstructed payload mismatch for %q", payload.Path)
		}
	}

	v13Report, v13BundleBytes, err := buildAndroidCodePatchBundle(androidCodePatchBuildOptions{
		CodePlanPath:      codePlanPath,
		PatchID:           "code-patch-v13",
		PatchNumber:       10,
		OutputPath:        filepath.Join(tempDir, "code-patch-v13.zip"),
		SeedBase64:        base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{12}, 32)),
		KeyID:             "dev-code-v13",
		CodeDeltaStrategy: "v13",
	})
	if err != nil {
		t.Fatalf("buildAndroidCodePatchBundle(v13) error = %v", err)
	}
	if !v13Report.Ready || len(v13Report.Payloads) != 2 {
		t.Fatalf("expected v13 report to be ready with payloads: %#v", v13Report)
	}
	for _, payload := range v13Report.Payloads {
		if payload.Strategy != codeDeltaStrategyV13 {
			t.Fatalf("expected v13 report payload strategy, got %#v", payload)
		}
	}

	_, v13ArtifactBytes, _ := parseBuiltPatchBundle(t, v13BundleBytes)
	v13ArtifactMetadata, v13DeltaFiles := parseCodePatchArtifact(t, v13ArtifactBytes)
	if v13ArtifactMetadata.Strategy != codeDeltaStrategyV13 {
		t.Fatalf("unexpected v13 code delta strategy: %#v", v13ArtifactMetadata)
	}
	for _, payload := range v13ArtifactMetadata.Payloads {
		deltaBytes, ok := v13DeltaFiles[payload.DeltaPath]
		if !ok {
			t.Fatalf("missing v13 delta payload %q", payload.DeltaPath)
		}
		if !bytes.HasPrefix(deltaBytes, []byte(codeDeltaMagicV13)) {
			t.Fatalf("expected v13 delta magic for %q", payload.Path)
		}
		reconstructed, err := applyCodeDelta(baseByPath[payload.Path], deltaBytes)
		if err != nil {
			t.Fatalf("applyCodeDelta(v13 %q) error = %v", payload.Path, err)
		}
		if !bytes.Equal(reconstructed, candidateByPath[payload.Path]) {
			t.Fatalf("v13 reconstructed payload mismatch for %q", payload.Path)
		}
	}

	cliBundlePath := filepath.Join(tempDir, "code-patch-v11-cli.zip")
	cliReportPath := filepath.Join(tempDir, "code-patch-v11-cli-report.json")
	if err := runBuildAndroidCodePatch([]string{
		"--code-plan", codePlanPath,
		"--patch-id", "code-patch-v11-cli",
		"--patch-number", "9",
		"--out", cliBundlePath,
		"--report-out", cliReportPath,
		"--seed-base64", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{11}, 32)),
		"--key-id", "dev-code-v11-cli",
		"--code-delta-strategy", "indexed_output_copy_v11",
	}); err != nil {
		t.Fatalf("runBuildAndroidCodePatch(v11 alias) error = %v", err)
	}
	cliBundleBytes, err := os.ReadFile(cliBundlePath)
	if err != nil {
		t.Fatalf("ReadFile(cliBundlePath) error = %v", err)
	}
	_, cliArtifactBytes, _ := parseBuiltPatchBundle(t, cliBundleBytes)
	cliArtifactMetadata, _ := parseCodePatchArtifact(t, cliArtifactBytes)
	if cliArtifactMetadata.Strategy != codeDeltaStrategyV11 {
		t.Fatalf("unexpected CLI v11 code delta strategy: %#v", cliArtifactMetadata)
	}
}

func parseCodePatchArtifact(
	t *testing.T,
	artifactBytes []byte,
) (androidCodeArtifactMetadata, map[string][]byte) {
	t.Helper()

	reader, err := zip.NewReader(bytes.NewReader(artifactBytes), int64(len(artifactBytes)))
	if err != nil {
		t.Fatalf("zip.NewReader() error = %v", err)
	}

	var metadata androidCodeArtifactMetadata
	deltaFiles := map[string][]byte{}
	for _, file := range reader.File {
		bytes, err := readZipFileBytes(file)
		if err != nil {
			t.Fatalf("readZipFileBytes(%q) error = %v", file.Name, err)
		}
		switch file.Name {
		case "metadata.json":
			if err := json.Unmarshal(bytes, &metadata); err != nil {
				t.Fatalf("json.Unmarshal(metadata) error = %v", err)
			}
		default:
			if strings.HasPrefix(file.Name, "deltas/") {
				deltaFiles[file.Name] = bytes
			}
		}
	}
	return metadata, deltaFiles
}

func buildPatternedCodePayload(segmentCount int) []byte {
	var output bytes.Buffer
	for i := 0; i < segmentCount; i++ {
		output.WriteString(fmt.Sprintf("segment-%04d|payload-%04d|", i, segmentCount-i))
	}
	return output.Bytes()
}
