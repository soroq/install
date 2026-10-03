package main

import (
	"strings"
	"testing"
)

// --allow-asset-diffs (Shorebird's flag of the same name) turns the icon refusal into a warning, and
// is taken out of the arguments before any delegate sees it.
func TestAllowAssetDiffs(t *testing.T) {
	t.Cleanup(func() { allowAssetDiffs = false })
	missing := map[string][]rune{"fonts/MaterialIcons-Regular.otf": {0xE139}}
	allowAssetDiffs = false
	if err := refuseMissingIconGlyphs(missing); err == nil || !strings.Contains(err.Error(), "--allow-asset-diffs") {
		t.Fatalf("without the flag the patch must be refused and name the flag, got %v", err)
	}
	rest := takeAllowAssetDiffs([]string{"--platforms=ios", "--allow-asset-diffs", "--rollout", "100"})
	if strings.Join(rest, " ") != "--platforms=ios --rollout 100" || !allowAssetDiffs {
		t.Fatalf("flag not taken: %v allow=%v", rest, allowAssetDiffs)
	}
	if err := refuseMissingIconGlyphs(missing); err != nil {
		t.Fatalf("with the flag the patch ships with a warning, got %v", err)
	}
}
