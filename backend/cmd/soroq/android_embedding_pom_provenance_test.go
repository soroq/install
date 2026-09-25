package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pomPin(t *testing.T, version string, body []byte) {
	t.Helper()
	sum := sha256.Sum256(body)
	prev, had := androidEmbeddingPomPins[version]
	androidEmbeddingPomPins[version] = hex.EncodeToString(sum[:])
	t.Cleanup(func() {
		if had {
			androidEmbeddingPomPins[version] = prev
		} else {
			delete(androidEmbeddingPomPins, version)
		}
	})
}

func readProv(t *testing.T, dst string) embeddingPomProvenance {
	t.Helper()
	var p embeddingPomProvenance
	raw, err := os.ReadFile(dst + ".provenance.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEmbeddingPomVendoredIsPreferredAndVerified(t *testing.T) {
	const v = "1.0.0-test-vendored"
	body := []byte("<project>vendored</project>")
	pomPin(t, v, body)
	cache := t.TempDir()
	vend := filepath.Join(cache, androidVendoredEmbeddingPomRel(v))
	os.MkdirAll(filepath.Dir(vend), 0o755)
	os.WriteFile(vend, body, 0o644)
	dst := filepath.Join(t.TempDir(), "flutter_embedding_release.pom")
	// A URL that would fail proves no download is attempted.
	if err := placeEmbeddingPom(cache, dst, v, "http://127.0.0.1:9/never"); err != nil {
		t.Fatal(err)
	}
	if p := readProv(t, dst); p.Source != "frontend-vendored" || !p.Pinned {
		t.Fatalf("provenance %+v", p)
	}
	os.WriteFile(vend, []byte("<project>tampered</project>"), 0o644)
	if err := placeEmbeddingPom(cache, filepath.Join(t.TempDir(), "p.pom"), v, "http://127.0.0.1:9/never"); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("tampered vendored POM accepted: %v", err)
	}
}

func TestEmbeddingPomDownloadIsVerified(t *testing.T) {
	const v = "1.0.0-test-download"
	body := []byte("<project>upstream</project>")
	served := body
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(served) }))
	defer srv.Close()
	pomPin(t, v, body)
	dst := filepath.Join(t.TempDir(), "flutter_embedding_release.pom")
	if err := placeEmbeddingPom(t.TempDir(), dst, v, srv.URL+"/x.pom"); err != nil {
		t.Fatal(err)
	}
	if p := readProv(t, dst); p.Source != "downloaded" || !p.Pinned || p.From != srv.URL+"/x.pom" {
		t.Fatalf("provenance %+v", p)
	}
	served = []byte("<project>substituted</project>")
	if err := placeEmbeddingPom(t.TempDir(), filepath.Join(t.TempDir(), "p.pom"), v, srv.URL+"/x.pom"); err == nil {
		t.Fatal("substituted upstream POM accepted")
	}
	// A previously placed POM is re-verified, not trusted.
	os.WriteFile(dst, []byte("<project>edited</project>"), 0o644)
	if err := placeEmbeddingPom(t.TempDir(), dst, v, srv.URL+"/x.pom"); err == nil {
		t.Fatal("edited placed POM accepted")
	}
}

func TestEmbeddingPomUnpinnedVersionIsRecordedAsUnpinned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<p/>")) }))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "flutter_embedding_release.pom")
	if err := placeEmbeddingPom(t.TempDir(), dst, "1.0.0-not-pinned", srv.URL+"/x.pom"); err != nil {
		t.Fatal(err)
	}
	if p := readProv(t, dst); p.Pinned || p.Source != "downloaded" || len(p.SHA256) != 64 {
		t.Fatalf("provenance %+v", p)
	}
}

func TestEmbeddingPomPinForThe3449Engine(t *testing.T) {
	if androidEmbeddingPomPins["1.0.0-5a2a6a42cce67f965cf540fcecf616faca624aa1"] != "6c24ccd1be9736d19c125fcee17e9bd94a27f716f091bc6688f55fa2e47c5538" {
		t.Fatal("5a2a6a42 embedding POM pin missing or changed")
	}
}
