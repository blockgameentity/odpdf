package har

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeHAR builds a synthetic HAR file with the given passthrough entries
// mixed with unrelated noise entries.
func writeHAR(t *testing.T, entries []map[string]any) string {
	t.Helper()
	har := map[string]any{
		"log": map[string]any{
			"version": "1.2",
			"entries": entries,
		},
	}
	data, err := json.Marshal(har)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "test.har")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func passthroughEntry(started, rawURL string, headers map[string]string, rng string) map[string]any {
	hs := []map[string]string{}
	for k, v := range headers {
		hs = append(hs, map[string]string{"name": k, "value": v})
	}
	if rng != "" {
		hs = append(hs, map[string]string{"name": "Range", "value": rng})
	}
	return map[string]any{
		"startedDateTime": started,
		"request": map[string]any{
			"method":  "GET",
			"url":     rawURL,
			"headers": hs,
		},
		"response": map[string]any{
			"status": 206,
			"content": map[string]any{
				"size":     12345,
				"mimeType": "application/octet-stream",
				"text":     "AAAA", // body must be ignored
			},
		},
	}
}

func noiseEntry(rawURL string) map[string]any {
	return map[string]any{
		"startedDateTime": "2026-10-27T13:00:00Z",
		"request": map[string]any{
			"method":  "GET",
			"url":     rawURL,
			"headers": []map[string]string{{"name": "X-Noise", "value": "1"}},
		},
		"response": map[string]any{
			"status":  200,
			"content": map[string]any{"size": 2, "text": "AA"},
		},
	}
}

const testPassthroughURL = "https://westeurope1-mediap.svc.ms/transform/passthrough?provider=spo&inputFormat=pdf&docid=abc"

func TestCollectExtractsPassthrough(t *testing.T) {
	path := writeHAR(t, []map[string]any{
		noiseEntry("https://ktsdk-my.sharepoint.com/page"),
		passthroughEntry("2026-10-27T13:55:00Z", testPassthroughURL, map[string]string{
			"X-SPOPacToken":   "v1.token",
			"User-Agent":      "TestUA",
			"Host":            "must-be-dropped",
			"Connection":      "keep-alive",
			"Accept-Encoding": "gzip",
		}, "bytes=0-3145727"),
	})

	req, err := Collect([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if req.URL != testPassthroughURL {
		t.Errorf("URL = %q", req.URL)
	}
	if got := req.Headers.Get("X-SPOPacToken"); got != "v1.token" {
		t.Errorf("X-SPOPacToken = %q", got)
	}
	if got := req.Headers.Get("User-Agent"); got != "TestUA" {
		t.Errorf("User-Agent = %q", got)
	}
	for _, dropped := range []string{"Host", "Connection", "Accept-Encoding"} {
		if req.Headers.Get(dropped) != "" {
			t.Errorf("header %q must be dropped", dropped)
		}
	}
	if len(req.Ranges) != 1 || req.Ranges[0] != "bytes=0-3145727" {
		t.Errorf("Ranges = %v", req.Ranges)
	}
}

func TestCollectMergesMultipleFilesLatterWins(t *testing.T) {
	p1 := writeHAR(t, []map[string]any{
		passthroughEntry("2026-10-27T13:55:00Z", "https://host/transform/passthrough?docid=old", map[string]string{
			"X-SPOPacToken": "old-token",
			"Referer":       "https://ref1/",
		}, ""),
	})
	p2 := writeHAR(t, []map[string]any{
		passthroughEntry("2026-10-27T13:57:00Z", "https://host/transform/passthrough?docid=new", map[string]string{
			"X-SPOPacToken": "new-token",
		}, ""),
	})

	req, err := Collect([]string{p1, p2})
	if err != nil {
		t.Fatal(err)
	}
	if req.URL != "https://host/transform/passthrough?docid=new" {
		t.Errorf("URL = %q", req.URL)
	}
	if got := req.Headers.Get("X-SPOPacToken"); got != "new-token" {
		t.Errorf("X-SPOPacToken = %q", got)
	}
	if got := req.Headers.Get("Referer"); got != "https://ref1/" {
		t.Errorf("Referer should survive merge, got %q", got)
	}
}

func TestCollectNoPassthrough(t *testing.T) {
	path := writeHAR(t, []map[string]any{
		noiseEntry("https://example.com/"),
	})
	if _, err := Collect([]string{path}); err == nil {
		t.Fatal("expected error for HAR without passthrough entries")
	}
}

func TestTokenExpiry(t *testing.T) {
	// Build a v1.<base64url-json> token with exp.
	payload := []byte(`{"exp":1793127128}`)
	tok := "v1." + base64RawURL(payload)
	h := http.Header{}
	h.Set("X-SPOPacToken", tok)

	exp, ok := TokenExpiry(h)
	if !ok {
		t.Fatal("expected expiry claim")
	}
	want := time.Unix(1793127128, 0)
	if !exp.Equal(want) {
		t.Errorf("exp = %v, want %v", exp, want)
	}

	if _, ok := TokenExpiry(http.Header{}); ok {
		t.Error("no token should yield no expiry")
	}
}

func base64RawURL(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	out := make([]byte, 0, (len(b)+2)/3*4)
	for i := 0; i < len(b); i += 3 {
		var n uint32
		rem := len(b) - i
		switch {
		case rem >= 3:
			n = uint32(b[i])<<16 | uint32(b[i+1])<<8 | uint32(b[i+2])
			out = append(out, alphabet[n>>18&0x3F], alphabet[n>>12&0x3F], alphabet[n>>6&0x3F], alphabet[n&0x3F])
		case rem == 2:
			n = uint32(b[i])<<16 | uint32(b[i+1])<<8
			out = append(out, alphabet[n>>18&0x3F], alphabet[n>>12&0x3F], alphabet[n>>6&0x3F])
		case rem == 1:
			n = uint32(b[i]) << 16
			out = append(out, alphabet[n>>18&0x3F], alphabet[n>>12&0x3F])
		}
	}
	return string(out)
}
