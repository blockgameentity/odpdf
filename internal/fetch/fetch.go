// Package fetch downloads the passthrough octet-stream the way the OneDrive
// PDF viewer does: as multiple ranged requests that are put back together at
// their byte offsets.
//
// Two service behaviors observed in HAR captures make this necessary:
//
//   - A request without a Range header gets a "malformed" response: real PDF
//     bytes for roughly the first megabyte, then zero padding to the declared
//     length.
//   - Range requests return true slices at their Content-Range offsets, but
//     the upstream fetch fails intermittently with 401 (roughly half of the
//     requests); retries converge within a few attempts.
package fetch

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a download.
type Options struct {
	// URL of the passthrough endpoint.
	URL string
	// Headers carries the authorization (X-SPOPacToken, cookies, ...).
	Headers http.Header
	// ChunkSize is the byte length of each ranged request (default 512 KiB).
	// Keep it at or below 1 MiB: the service's malformed mode starts after
	// roughly one megabyte.
	ChunkSize int64
	// Concurrency caps parallel ranged requests. 0 (the default) downloads
	// all parts at once: one request per chunk, all in parallel.
	Concurrency int
	// MaxAttempts is the per-chunk retry budget (default 8).
	MaxAttempts int
	// Timeout bounds a single HTTP request (default 2 minutes).
	Timeout time.Duration
	// Progress, when set, is called after every finished chunk with the
	// number of bytes written and the total size.
	Progress func(done, total int64)
}

func (o *Options) withDefaults() error {
	if o.URL == "" {
		return fmt.Errorf("fetch: missing URL")
	}
	if o.ChunkSize <= 0 {
		o.ChunkSize = 512 * 1024
	}
	if o.ChunkSize > 1024*1024 {
		return fmt.Errorf("fetch: chunk size %d exceeds 1 MiB; the service corrupts larger responses with zero padding", o.ChunkSize)
	}
	if o.Concurrency < 0 {
		return fmt.Errorf("fetch: invalid concurrency %d", o.Concurrency)
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 8
	}
	if o.Timeout <= 0 {
		o.Timeout = 2 * time.Minute
	}
	if o.Headers == nil {
		o.Headers = http.Header{}
	}
	return nil
}

// chunk is an inclusive byte range of the file.
type chunk struct {
	start, end int64
}

// Probe fetches the total byte size of the resource via a one-byte range
// request and also serves as an authorization sanity check.
func Probe(ctx context.Context, opts Options) (int64, error) {
	if err := opts.withDefaults(); err != nil {
		return 0, err
	}
	c := &client{opts: opts}
	resp, body, err := c.getRange(ctx, 0, 0)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if len(body) != 1 {
		return 0, fmt.Errorf("fetch: probe expected 1 byte, got %d", len(body))
	}
	total, err := contentRangeTotal(resp)
	if err != nil {
		return 0, err
	}
	return total, nil
}

// Download fetches the whole resource into outPath using ranged requests and
// returns the total number of bytes written.
func Download(ctx context.Context, outPath string, opts Options) (int64, error) {
	if err := opts.withDefaults(); err != nil {
		return 0, err
	}

	total, err := Probe(ctx, opts)
	if err != nil {
		return 0, err
	}
	if total <= 0 {
		return 0, fmt.Errorf("fetch: invalid total size %d", total)
	}

	f, err := os.Create(outPath)
	if err != nil {
		return 0, fmt.Errorf("fetch: create %s: %w", outPath, err)
	}
	defer f.Close()
	if err := f.Truncate(total); err != nil {
		return 0, fmt.Errorf("fetch: preallocate: %w", err)
	}

	c := &client{opts: opts}

	// Plan chunks and feed them to workers.
	type job struct{ c chunk }
	jobs := make(chan job)
	errs := make(chan error, 1)
	var doneBytes atomic.Int64
	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Fan out: by default every part is downloaded in parallel; an explicit
	// Concurrency caps the number of simultaneous requests.
	numChunks := (total + opts.ChunkSize - 1) / opts.ChunkSize
	workers := opts.Concurrency
	if workers == 0 || int64(workers) > numChunks {
		workers = int(numChunks)
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				n, err := c.fetchChunk(ctx, f, j.c)
				if err != nil {
					select {
					case errs <- err:
					default:
					}
					cancel()
					return
				}
				done := doneBytes.Add(n)
				if opts.Progress != nil {
					opts.Progress(done, total)
				}
			}
		}()
	}

feed:
	for s := int64(0); s < total; s += opts.ChunkSize {
		end := s + opts.ChunkSize - 1
		if end >= total {
			end = total - 1
		}
		select {
		case jobs <- job{c: chunk{start: s, end: end}}:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	select {
	case err := <-errs:
		return 0, err
	default:
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("fetch: cancelled: %w", err)
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("fetch: sync: %w", err)
	}
	return total, nil
}

// client performs retried ranged GETs.
type client struct {
	opts    Options
	httpCli *http.Client
}

func (c *client) http() *http.Client {
	if c.httpCli == nil {
		c.httpCli = &http.Client{Timeout: c.opts.Timeout}
	}
	return c.httpCli
}

// getRange performs one ranged GET and returns the response plus the fully
// read body. Retries are handled by the callers via fetchChunk/probe.
func (c *client) getRange(ctx context.Context, start, end int64) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.opts.URL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch: build request: %w", err)
	}
	for k, vs := range c.opts.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := c.http().Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch: request bytes=%d-%d: %w", start, end, err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (end-start)+2))
	resp.Body.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("fetch: read bytes=%d-%d: %w", start, end, err)
	}
	return resp, body, nil
}

// fetchChunk downloads one chunk with retries and writes it at its offset.
func (c *client) fetchChunk(ctx context.Context, f *os.File, ch chunk) (int64, error) {
	want := ch.end - ch.start + 1

	var lastErr error
	for attempt := 0; attempt < c.opts.MaxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, attempt); err != nil {
				return 0, err
			}
		}

		resp, body, err := c.getRange(ctx, ch.start, ch.end)
		if err != nil {
			lastErr = err
			continue
		}

		switch {
		case resp.StatusCode == http.StatusPartialContent:
			// Trust the server's Content-Range over the request: place the
			// bytes at the returned offset.
			start, end, _, err := parseContentRange(resp.Header.Get("Content-Range"))
			if err != nil {
				lastErr = fmt.Errorf("fetch: bytes=%d-%d: %w", ch.start, ch.end, err)
				continue
			}
			if start != ch.start || end != ch.end {
				lastErr = fmt.Errorf("fetch: bytes=%d-%d: server returned range %d-%d", ch.start, ch.end, start, end)
				continue
			}
			if int64(len(body)) != want {
				lastErr = fmt.Errorf("fetch: bytes=%d-%d: short body (%d of %d bytes)", ch.start, ch.end, len(body), want)
				continue
			}
			if _, err := f.WriteAt(body, ch.start); err != nil {
				return 0, fmt.Errorf("fetch: write at %d: %w", ch.start, err)
			}
			return want, nil

		case resp.StatusCode == http.StatusOK:
			// No Range support means we would receive the malformed
			// zero-padded stream; refuse to use it.
			lastErr = fmt.Errorf("fetch: bytes=%d-%d: server ignored Range header (200 response is zero-padded and unusable)", ch.start, ch.end)

		case resp.StatusCode == http.StatusUnauthorized ||
			resp.StatusCode == http.StatusForbidden ||
			resp.StatusCode == http.StatusRequestTimeout ||
			resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode >= 500:
			// The service intermittently fails its upstream fetch with 401.
			lastErr = fmt.Errorf("fetch: bytes=%d-%d: HTTP %d", ch.start, ch.end, resp.StatusCode)

		default:
			return 0, fmt.Errorf("fetch: bytes=%d-%d: unexpected HTTP %d", ch.start, ch.end, resp.StatusCode)
		}
	}
	return 0, fmt.Errorf("fetch: bytes=%d-%d: giving up after %d attempts: %w", ch.start, ch.end, c.opts.MaxAttempts, lastErr)
}

// sleepBackoff waits before retry attempt n (250ms * 2^n, capped at 10s,
// plus jitter) or returns early when ctx is done.
func sleepBackoff(ctx context.Context, attempt int) error {
	d := 250 * time.Millisecond << (attempt - 1)
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	d += time.Duration(rand.Int63n(int64(250 * time.Millisecond)))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// parseContentRange parses "bytes <start>-<end>/<total|*>".
func parseContentRange(v string) (start, end, total int64, err error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, 0, 0, fmt.Errorf("missing Content-Range header")
	}
	rest, ok := strings.CutPrefix(v, "bytes ")
	if !ok {
		return 0, 0, 0, fmt.Errorf("malformed Content-Range %q", v)
	}
	rangePart, totalPart, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, 0, 0, fmt.Errorf("malformed Content-Range %q", v)
	}
	sPart, ePart, ok := strings.Cut(rangePart, "-")
	if !ok {
		return 0, 0, 0, fmt.Errorf("malformed Content-Range %q", v)
	}
	start, err = strconv.ParseInt(strings.TrimSpace(sPart), 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("malformed Content-Range %q: %w", v, err)
	}
	end, err = strconv.ParseInt(strings.TrimSpace(ePart), 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("malformed Content-Range %q: %w", v, err)
	}
	totalPart = strings.TrimSpace(totalPart)
	if totalPart != "*" {
		total, err = strconv.ParseInt(totalPart, 10, 64)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("malformed Content-Range %q: %w", v, err)
		}
	}
	return start, end, total, nil
}

// contentRangeTotal extracts the total size from a probe response.
func contentRangeTotal(resp *http.Response) (int64, error) {
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("fetch: probe: expected HTTP 206, got %d", resp.StatusCode)
	}
	_, _, total, err := parseContentRange(resp.Header.Get("Content-Range"))
	if err != nil {
		return 0, fmt.Errorf("fetch: probe: %w", err)
	}
	if total <= 0 {
		return 0, fmt.Errorf("fetch: probe: unknown total size")
	}
	return total, nil
}
