package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// REAL-DATA CONTROL, opt-in. Runs the precise rule against a real baseline directory:
//
//	SOROQ_PATHA_BASELINE=<.soroq/releases/<runtime_id>> \
//	SOROQ_PATHA_QUERIES='<kind>|<libUri>::<class>::<vmName>,...' \
//	go test ./cmd/soroq -run TestValuePropagationPathAOnRealBaseline -v
//
// It prints the verdict for each identity; it asserts nothing about a particular app, because the
// graphs carry unrelated application strings and are never committed.
func TestValuePropagationPathAOnRealBaseline(t *testing.T) {
	dir := os.Getenv("SOROQ_PATHA_BASELINE")
	if dir == "" {
		t.Skip("set SOROQ_PATHA_BASELINE to a real baseline directory to run this control")
	}
	var queries []PathAQuery
	for _, q := range strings.Split(os.Getenv("SOROQ_PATHA_QUERIES"), ",") {
		kind, id, ok := strings.Cut(strings.TrimSpace(q), "|")
		if !ok {
			continue
		}
		lib, class, vm, err := splitIdentity(id)
		if err != nil {
			t.Fatal(err)
		}
		queries = append(queries, PathAQuery{PropagationQuery: PropagationQuery{Kind: kind, Class: class, VMName: vm}, LibURI: lib})
	}
	vp, err := analyzeValuePropagationPathA(filepath.Join(dir, freehandObjectGraphName), filepath.Join(dir, "app.dill"),
		filepath.Join(dir, "symbol_graph.json"), queries, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range queries {
		msg := valuePropagationRefusal(vp, q.Kind, q.Class, q.VMName)
		if msg == "" {
			t.Logf("ACCEPT %s::%s::%s", q.LibURI, q.Class, q.VMName)
		} else {
			t.Logf("REFUSE %s::%s::%s\n    %s", q.LibURI, q.Class, q.VMName, msg)
		}
	}
}
