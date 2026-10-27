// Package har extracts authorized requests from HAR captures.
//
// The OneDrive PDF viewer fetches the document through the MediaTA
// "transform/passthrough" endpoint. A HAR exported from that session carries
// everything needed to replay those downloads: the passthrough URL and the
// authorization headers (X-SPOPacToken, cookies, ...). Response bodies are
// deliberately ignored: they are truncated/zero-padded capture artifacts and
// must not be used as data.
package har

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// KV is a name/value pair as it appears in HAR header and cookie lists.
type KV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type rawEntry struct {
	StartedDateTime string `json:"startedDateTime"`
	Request         struct {
		Method  string `json:"method"`
		URL     string `json:"url"`
		Headers []KV   `json:"headers"`
		Cookies []KV   `json:"cookies"`
	} `json:"request"`
	Response struct {
		Status  int  `json:"status"`
		Headers []KV `json:"headers"`
	} `json:"response"`
}

// Entry is a decoded HAR entry, limited to what replay needs.
type Entry struct {
	StartedDateTime string
	Method          string
	URL             string
	Headers         []KV
	Cookies         []KV
	Status          int
}

// AuthorizedRequest is a replayable, authorized passthrough request.
type AuthorizedRequest struct {
	// URL of the transform/passthrough endpoint (latest entry wins).
	URL string
	// Headers to send, merged across HAR files and entries; later wins.
	Headers http.Header
	// Observed Range request headers from the viewer (informational).
	Ranges []string
	// Sources lists the HAR files the request was built from.
	Sources []string
}

// IsPassthrough reports whether a URL targets the media passthrough endpoint.
func IsPassthrough(rawURL string) bool {
	u := strings.ToLower(rawURL)
	return strings.Contains(u, "/transform/passthrough")
}

// headerDenylist are hop-by-hop or per-request headers that must not be
// replayed; the downloader sets its own Range and forces identity encoding.
var headerDenylist = map[string]bool{
	"host":              true,
	"connection":        true,
	"content-length":    true,
	"accept-encoding":   true,
	"te":                true,
	"transfer-encoding": true,
	"priority":          true,
	"range":             true,
	"if-range":          true,
	"if-modified-since": true,
	"if-none-match":     true,
}

// Entries reads a HAR file and returns all entries whose request targets the
// transform/passthrough endpoint.
//
// Parsing is streaming: the giant base64 response bodies in the HAR are
// skipped without being buffered, so multi-hundred-MB HAR files are fine.
func Entries(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("har: %w", err)
	}
	defer f.Close()

	dec := json.NewDecoder(bufio.NewReader(f))

	// Navigate { "log": { "entries": [ ... ] } } without decoding the
	// whole file into memory.
	if err := seekKey(dec, "entries"); err != nil {
		return nil, fmt.Errorf("har %s: %w", path, err)
	}
	// Consume the array opening delimiter of "entries".
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("har %s: entries: %w", path, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, fmt.Errorf("har %s: entries is not an array", path)
	}

	var out []Entry
	for dec.More() {
		var re rawEntry
		if err := dec.Decode(&re); err != nil {
			return nil, fmt.Errorf("har %s: decode entry: %w", path, err)
		}
		if !IsPassthrough(re.Request.URL) {
			continue
		}
		out = append(out, Entry{
			StartedDateTime: re.StartedDateTime,
			Method:          re.Request.Method,
			URL:             re.Request.URL,
			Headers:         re.Request.Headers,
			Cookies:         re.Request.Cookies,
			Status:          re.Response.Status,
		})
	}
	return out, nil
}

// Collect builds a replayable authorized request from one or more HAR files.
// Files are processed in order; later files and later entries override the
// passthrough URL and same-named header values.
func Collect(paths []string) (*AuthorizedRequest, error) {
	req := &AuthorizedRequest{Headers: http.Header{}}
	found := false

	for _, p := range paths {
		entries, err := Entries(p)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			found = true
			req.URL = e.URL
			req.Sources = appendUnique(req.Sources, p)
			for _, h := range e.Headers {
				name := http.CanonicalHeaderKey(h.Name)
				if headerDenylist[strings.ToLower(h.Name)] {
					continue
				}
				req.Headers.Set(name, h.Value)
			}
			// HAR cookies are additional evidence; merge as a Cookie header.
			if len(e.Cookies) > 0 {
				var parts []string
				for _, c := range e.Cookies {
					parts = append(parts, c.Name+"="+c.Value)
				}
				existing := req.Headers.Get("Cookie")
				joined := strings.Join(parts, "; ")
				if existing != "" {
					joined = existing + "; " + joined
				}
				req.Headers.Set("Cookie", joined)
			}
			if r := headerValue(e.Headers, "Range"); r != "" {
				req.Ranges = appendUnique(req.Ranges, r)
			}
		}
	}

	if !found {
		return nil, fmt.Errorf("har: no transform/passthrough request found in %v", paths)
	}
	return req, nil
}

// TokenExpiry inspects an X-SPOPacToken (v1.<base64url-json>...) and returns
// its exp claim, if present.
func TokenExpiry(headers http.Header) (time.Time, bool) {
	tok := headers.Get("X-SPOPacToken")
	if tok == "" {
		return time.Time{}, false
	}
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	payload, err := decodeBase64URL(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, false
	}
	sec, err := claims.Exp.Int64()
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
}

// seekKey advances the decoder to the value of the next object key matching
// name. Only real object keys are matched (not string values). Note that
// json.Decoder.Token omits ':' and ',' so key position is derived from the
// nesting stack and key/value alternation within objects.
func seekKey(dec *json.Decoder, name string) error {
	var stack []json.Delim // nesting of '{' and '['
	expectKey := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("find %q: %w", name, err)
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, t)
				expectKey = true
			case '[':
				stack = append(stack, t)
				expectKey = false
			case '}', ']':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				// A closed value is followed by a key in an object.
				expectKey = len(stack) > 0 && stack[len(stack)-1] == '{'
			}
		case string:
			if expectKey {
				if t == name {
					return nil
				}
				expectKey = false // the key's value follows
			} else {
				expectKey = len(stack) > 0 && stack[len(stack)-1] == '{'
			}
		default:
			// Numbers, booleans, null are values.
			expectKey = len(stack) > 0 && stack[len(stack)-1] == '{'
		}
	}
}

func headerValue(kvs []KV, name string) string {
	for _, kv := range kvs {
		if strings.EqualFold(kv.Name, name) {
			return kv.Value
		}
	}
	return ""
}

func appendUnique(list []string, v string) []string {
	for _, s := range list {
		if s == v {
			return list
		}
	}
	return append(list, v)
}

func decodeBase64URL(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}
