package fetch

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testPayload returns deterministic pseudo-random content for the fake
// service; every byte is non-zero so zero-fill corruption is detectable.
func testPayload(size int64) []byte {
	buf := make([]byte, size)
	r := rand.New(rand.NewSource(42))
	r.Read(buf)
	for i := range buf {
		if buf[i] == 0 {
			buf[i] = 0x5A
		}
	}
	return buf
}

// fakeService emulates the passthrough endpoint:
//   - ranged requests serve true slices, with configurable flaky failures
//   - requests without a Range header get the "malformed" zero-padded body
//   - some responses can be truncated mid-body to exercise retry handling
type fakeService struct {
	payload     []byte
	failEvery   int32 // 1/N of requests fail with 401; 0 disables
	truncateAt  int64 // if >0, bodies are cut after N bytes (first M requests)
	truncateCnt int32 // number of requests still affected by truncateAt
	reqCount    atomic.Int32
}

func (s *fakeService) handler(w http.ResponseWriter, r *http.Request) {
	n := s.reqCount.Add(1)
	if s.failEvery > 0 && n%int32(s.failEvery) == 0 {
		http.Error(w, `{"title":"BadRequest","status":401}`, http.StatusUnauthorized)
		return
	}

	rng := r.Header.Get("Range")
	if rng == "" {
		// Malformed mode: real bytes for the first ~1 MiB, zero padding after.
		body := make([]byte, len(s.payload))
		copy(body, s.payload[:min(int64(len(s.payload)), 1040378)])
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Range", "bytes 0-"+itoa(int64(len(body)-1))+"/"+itoa(int64(len(body))))
		w.Write(body)
		return
	}

	start, end, ok := parseRange(rng, int64(len(s.payload)))
	if !ok {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}

	body := s.payload[start : end+1]

	// Optional truncation to simulate connections dying mid-transfer.
	if s.truncateAt > 0 && s.truncateCnt > 0 {
		s.truncateCnt--
		if int64(len(body)) > s.truncateAt {
			w.Header().Set("Content-Range", "bytes "+itoa(start)+"-"+itoa(end)+"/"+itoa(int64(len(s.payload))))
			w.Header().Set("Content-Length", itoa(int64(len(body))))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(body[:s.truncateAt])
			return
		}
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Range", "bytes "+itoa(start)+"-"+itoa(end)+"/"+itoa(int64(len(s.payload))))
	w.WriteHeader(http.StatusPartialContent)
	w.Write(body)
}

func parseRange(v string, total int64) (start, end int64, ok bool) {
	var s, e int64
	if _, err := fmt.Sscanf(v, "bytes=%d-%d", &s, &e); err != nil {
		return 0, 0, false
	}
	if s < 0 || e >= total || s > e {
		return 0, 0, false
	}
	return s, e, true
}

func TestDownloadAssemblesParts(t *testing.T) {
	payload := testPayload(3*1024*1024 + 777)           // unaligned size
	svc := &fakeService{payload: payload, failEvery: 3} // every 3rd request 401s
	srv := httptest.NewServer(http.HandlerFunc(svc.handler))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	total, err := Download(context.Background(), out, Options{
		URL:         srv.URL,
		Headers:     http.Header{"X-SPOPacToken": []string{"tok"}},
		ChunkSize:   256 * 1024,
		Concurrency: 4,
		MaxAttempts: 10,
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != int64(len(payload)) {
		t.Fatalf("total = %d, want %d", total, len(payload))
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("assembled bytes differ from source (len %d vs %d)", len(got), len(payload))
	}
}

func TestDownloadRetriesTransient401(t *testing.T) {
	payload := testPayload(512 * 1024)
	// 50% failure rate, like the real service.
	svc := &fakeService{payload: payload, failEvery: 2}
	srv := httptest.NewServer(http.HandlerFunc(svc.handler))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	_, err := Download(context.Background(), out, Options{
		URL:         srv.URL,
		ChunkSize:   256 * 1024,
		MaxAttempts: 20,
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, payload) {
		t.Fatal("assembled bytes differ after retries")
	}
}

func TestDownloadHandlesTruncatedBody(t *testing.T) {
	payload := testPayload(256 * 1024)
	// First two responses are cut after 1 KiB; retries must succeed.
	svc := &fakeService{payload: payload, truncateAt: 1024, truncateCnt: 2}
	srv := httptest.NewServer(http.HandlerFunc(svc.handler))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	_, err := Download(context.Background(), out, Options{
		URL:         srv.URL,
		ChunkSize:   128 * 1024,
		Concurrency: 1,
		MaxAttempts: 6,
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, payload) {
		t.Fatal("assembled bytes differ after truncated transfers")
	}
}

func TestDownloadRefusesUnrangedResponse(t *testing.T) {
	payload := testPayload(1024)
	// Server ignores Range entirely (200 + zero-padded body).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, len(payload)))
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	_, err := Download(context.Background(), out, Options{
		URL:         srv.URL,
		ChunkSize:   512,
		MaxAttempts: 2,
		Timeout:     5 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error when server ignores Range")
	}
}

func TestChunkSizeCap(t *testing.T) {
	_, err := Download(context.Background(), os.DevNull, Options{
		URL:       "https://example.invalid/",
		ChunkSize: 2 * 1024 * 1024,
	})
	if err == nil || !contains(err.Error(), "chunk size") {
		t.Fatalf("expected chunk size error, got %v", err)
	}
}

func TestParseContentRange(t *testing.T) {
	s, e, total, err := parseContentRange("bytes 62914560-64394366/64394367")
	if err != nil {
		t.Fatal(err)
	}
	if s != 62914560 || e != 64394366 || total != 64394367 {
		t.Errorf("got %d %d %d", s, e, total)
	}
	_, _, _, err = parseContentRange("")
	if err == nil {
		t.Error("expected error for missing header")
	}
	_, _, _, err = parseContentRange("items 1-2/3")
	if err == nil {
		t.Error("expected error for non-bytes unit")
	}
}

// small helpers to keep the test file compact

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
