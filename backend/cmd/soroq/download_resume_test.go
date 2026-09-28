package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flakyArchiveServer serves body, cutting the FIRST response off after cut bytes. honourRange decides
// whether later Range requests get a 206 from the requested byte or the whole body again (200).
func flakyArchiveServer(t *testing.T, body []byte, cut int, honourRange bool) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		start := 0
		if rng := r.Header.Get("Range"); honourRange && strings.HasPrefix(rng, "bytes=") {
			start, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-"))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
			w.Header().Set("Content-Length", strconv.Itoa(len(body)-start))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
		}
		if n == 1 {
			_, _ = w.Write(body[start:cut])
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close() // the network drops mid-download
			}
			return
		}
		_, _ = w.Write(body[start:])
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func downloadFor(t *testing.T, url string) (string, int64, []byte, error) {
	t.Helper()
	old := archiveDownloadBackoff
	archiveDownloadBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { archiveDownloadBackoff = old })
	f, err := os.CreateTemp(t.TempDir(), "archive-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sum, n, derr := streamDownloadToFile(url, f, nil)
	got, _ := os.ReadFile(f.Name())
	return sum, n, got, derr
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestDownloadResumesAfterADroppedConnection(t *testing.T) {
	body := bytes.Repeat([]byte("soroq-archive-"), 50000) // 700 KB
	srv, calls := flakyArchiveServer(t, body, 300000, true)
	sum, n, got, err := downloadFor(t, srv.URL)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("expected one resume (2 requests), got %d", calls.Load())
	}
	if n != int64(len(body)) || !bytes.Equal(got, body) || sum != sha(body) {
		t.Errorf("resumed file differs: n=%d want %d, sha match=%v", n, len(body), sum == sha(body))
	}
}

func TestDownloadRestartsWhenTheServerIgnoresTheRange(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 70000)
	srv, calls := flakyArchiveServer(t, body, 250000, false)
	sum, n, got, err := downloadFor(t, srv.URL)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if calls.Load() != 2 || n != int64(len(body)) || !bytes.Equal(got, body) || sum != sha(body) {
		t.Errorf("restart path: calls=%d n=%d equal=%v sha=%v", calls.Load(), n, bytes.Equal(got, body), sum == sha(body))
	}
}

func TestDownloadDoesNotRetryAClientError(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no such archive", http.StatusNotFound)
	}))
	defer srv.Close()
	if _, _, _, err := downloadFor(t, srv.URL); err == nil || !strings.Contains(err.Error(), "no such archive") {
		t.Fatalf("404 should fail with the server's message, got %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("a 404 was retried %d times", calls.Load()-1)
	}
}
