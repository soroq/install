package main

import "strings"
import "testing"

// The release's acceptance case: the pub package named `path` and a real relative path dependency in
// the SAME pubspec, in both dependency blocks, with the local one still absolutised.
func TestPackageNamedPathAlongsideGenuineLocalPathDependency(t *testing.T) {
	in := `name: app
dependencies:
  flutter:
    sdk: flutter
  path: ^1.9.1
  soroq_flutter:
    path: ../packages/soroq_flutter
  dynamic_modules:
    path: ./vendor/dynamic_modules
dev_dependencies:
  path: any
  test_helper:
    path: ../tools/test_helper
`
	out := pubspecWithAbsolutePathDependencies(in, "/repo/app")
	for _, keep := range []string{"  path: ^1.9.1", "  path: any"} {
		if !strings.Contains(out, keep) {
			t.Errorf("the pub package named `path` was rewritten (%q missing):\n%s", keep, out)
		}
	}
	for _, want := range []string{
		"path: /repo/packages/soroq_flutter",
		"path: /repo/app/vendor/dynamic_modules",
		"path: /repo/tools/test_helper",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("genuine local path dependency not absolutised (%q missing):\n%s", want, out)
		}
	}
}
