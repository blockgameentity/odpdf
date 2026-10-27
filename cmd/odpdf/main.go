// Command odpdf (OneDrive Protected PDF Extractor) downloads a
// password-protected PDF through a OneDrive "protected" share link and
// removes its password protection locally.
//
// It replays the authorized download requests of the OneDrive PDF viewer
// (MediaTA transform/passthrough, ranged octet-stream fetches) using auth
// extracted from a HAR capture or from direct header/cookie key-value pairs,
// puts the parts back together at their byte offsets and then decrypts the
// assembled PDF. It performs plain download requests only; it does not
// circumvent any access control.
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/desu/onedrive-protected-pdf-extractor/internal/authkv"
	"github.com/desu/onedrive-protected-pdf-extractor/internal/fetch"
	"github.com/desu/onedrive-protected-pdf-extractor/internal/har"
	"github.com/desu/onedrive-protected-pdf-extractor/internal/pdftool"
	"github.com/desu/onedrive-protected-pdf-extractor/internal/ui"
)

const defaultMediaPHost = "westeurope1-mediap.svc.ms"

type config struct {
	harPaths    authkv.HeaderFlags
	headers     authkv.HeaderFlags
	cookies     authkv.CookieFlags
	password    string
	outPath     string
	rawOut      string
	keepRaw     bool
	chunkSize   int64
	concurrency int
	retries     int
	timeout     time.Duration
	mediaPHost  string
	quiet       bool
	colorMode   string
}

func main() {
	if err := run(); err != nil {
		ui.Error("%v", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config{}
	fs := flag.NewFlagSet("odpdf", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `odpdf - OneDrive Protected PDF Extractor

Downloads a password-protected PDF through a OneDrive share link by replaying
the viewer's authorized range requests, assembles the parts and removes the
PDF password locally.

Usage:
  odpdf [flags] <onedrive-url>

Auth (provide at least one):
  -H, --har FILE          HAR capture of the PDF viewer session; the
                          authorized passthrough request is extracted from it
                          (repeatable)
      --header "N: V"     direct header KV for the download requests, e.g.
                          "X-SPOPacToken: v1.eyJ..." (repeatable; "N=V" also
                          accepted)
      --cookie "k=v"      direct cookie KV ("a=b; c=d" and "Cookie: a=b"
                          accepted; repeatable)

Password:
  -p, --password PASS     PDF password (default: $ODPDF_PASSWORD /
                          $PDF_PASSWORD, else prompted)

Download:
      --chunk N           bytes per range request (default 524288, max 1 MiB)
  -c, --concurrency N     parallel range requests (default 0 = all parts at
                          once; set N to throttle)
      --retries N         max attempts per chunk (default 8)
      --timeout D         per-request timeout (default 2m)

Output:
  -o, --out FILE          decrypted PDF path (default: <name>.pdf)
      --raw-out FILE      keep the raw (encrypted) download at this path
      --keep-raw          keep the raw encrypted PDF next to the output
                          (default true)
      --mediap-host HOST  passthrough host used when the URL has to be
                          resolved without a HAR (default `+defaultMediaPHost+`)
      --color MODE        color output: auto|always|never (default auto)
  -q, --quiet             suppress progress output

The onedrive-url may be the share link as opened in the browser, or a direct
transform/passthrough URL.

`)
		fs.PrintDefaults()
	}

	fs.Var(&cfg.harPaths, "H", "HAR capture (repeatable)")
	fs.Var(&cfg.harPaths, "har", "HAR capture (repeatable)")
	fs.Var(&cfg.headers, "header", "header KV (repeatable)")
	fs.Var(&cfg.cookies, "cookie", "cookie KV (repeatable)")
	fs.StringVar(&cfg.password, "p", "", "PDF password")
	fs.StringVar(&cfg.password, "password", "", "PDF password")
	fs.StringVar(&cfg.outPath, "o", "", "decrypted output path")
	fs.StringVar(&cfg.outPath, "out", "", "decrypted output path")
	fs.StringVar(&cfg.rawOut, "raw-out", "", "keep raw encrypted download here")
	fs.BoolVar(&cfg.keepRaw, "keep-raw", true, "keep the raw encrypted PDF")
	fs.Int64Var(&cfg.chunkSize, "chunk", 512*1024, "bytes per range request")
	fs.IntVar(&cfg.concurrency, "c", 0, "parallel range requests (0 = all parts at once)")
	fs.IntVar(&cfg.concurrency, "concurrency", 0, "parallel range requests (0 = all parts at once)")
	fs.IntVar(&cfg.retries, "retries", 8, "max attempts per chunk")
	fs.DurationVar(&cfg.timeout, "timeout", 2*time.Minute, "per-request timeout")
	fs.StringVar(&cfg.mediaPHost, "mediap-host", defaultMediaPHost, "passthrough host for URL resolution")
	fs.StringVar(&cfg.colorMode, "color", "auto", "color output: auto|always|never")
	fs.BoolVar(&cfg.quiet, "q", false, "suppress progress output")
	fs.BoolVar(&cfg.quiet, "quiet", false, "suppress progress output")

	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	ui.Init(cfg.colorMode, os.Stderr)
	ui.SetQuiet(cfg.quiet)

	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("expected exactly one URL argument, got %d", fs.NArg())
	}
	rawURL := fs.Arg(0)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- 1. Assemble the authorized request from HAR and/or KV input ----
	headers, err := authkv.Parse(cfg.headers, cfg.cookies)
	if err != nil {
		return err
	}

	authDesc := "header/cookie key-values"
	var harReq *har.AuthorizedRequest
	if len(cfg.harPaths) > 0 {
		harReq, err = har.Collect(cfg.harPaths)
		if err != nil {
			return err
		}
		// HAR headers form the base; explicit KVs override them.
		merged := harReq.Headers.Clone()
		for k, vs := range headers {
			if len(vs) > 0 {
				merged[k] = vs
			}
		}
		headers = merged

		authDesc = "HAR " + strings.Join(cfg.harPaths, ", ")
		if exp, ok := har.TokenExpiry(harReq.Headers); ok {
			if time.Now().After(exp) {
				ui.Warn("X-SPOPacToken from HAR expired at %s; downloads will likely fail until you re-capture", exp.Format(time.RFC3339))
			} else {
				authDesc += fmt.Sprintf(" · token valid until %s", exp.Local().Format("15:04:05"))
			}
		}
	}

	if len(headers) == 0 {
		return fmt.Errorf("no authorization: pass --har FILE or --header \"X-SPOPacToken: ...\" (see README)")
	}

	// ---- 2. Determine the passthrough download URL ----
	pu, err := resolvePassthroughURL(rawURL, harReq, headers, cfg.mediaPHost)
	if err != nil {
		return err
	}

	// ---- 3. Resolve the PDF password ----
	password := cfg.password
	if password == "" {
		password = os.Getenv("ODPDF_PASSWORD")
	}
	if password == "" {
		password = os.Getenv("PDF_PASSWORD")
	}
	if password == "" {
		password, err = promptPassword()
		if err != nil {
			return err
		}
	}

	// ---- 4. Plan output paths ----
	baseName := deriveName(rawURL, pu)
	outPath := cfg.outPath
	if outPath == "" {
		outPath = baseName + ".pdf"
	}
	rawPath := cfg.rawOut
	if rawPath == "" {
		rawPath = strings.TrimSuffix(outPath, filepath.Ext(outPath)) + ".encrypted.pdf"
	}

	// ---- 5. Download the parts and assemble them ----
	opts := fetch.Options{
		URL:         pu,
		Headers:     headers,
		ChunkSize:   cfg.chunkSize,
		Concurrency: cfg.concurrency,
		MaxAttempts: cfg.retries,
		Timeout:     cfg.timeout,
	}

	total, err := fetch.Probe(ctx, opts)
	if err != nil {
		return err
	}
	numParts := (total + opts.ChunkSize - 1) / opts.ChunkSize

	ui.Step("OneDrive Protected PDF Extractor")
	ui.Detail("source", "%s", truncate(rawURL, 96))
	ui.Detail("auth", "%s", authDesc)
	ui.Detail("parts", "%s · %d × %s · parallel", ui.HumanBytes(total), numParts, ui.HumanBytes(opts.ChunkSize))
	ui.Detail("output", "%s", outPath)

	prog := ui.NewProgress()
	opts.Progress = prog.Update

	started := time.Now()
	logStep(cfg, "downloading to %s", rawPath)
	if _, err := fetch.Download(ctx, rawPath, opts); err != nil {
		return err
	}
	elapsed := time.Since(started)

	ok, err := pdftool.IsPDF(rawPath)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s does not start with %%PDF-; the download is not a PDF", rawPath)
	}
	ui.OK("downloaded", "%s in %s (%s/s) · %d parts",
		ui.HumanBytes(total), ui.HumanDuration(elapsed),
		ui.HumanBytes(int64(float64(total)/elapsed.Seconds())), numParts)

	// ---- 6. Remove the password ----
	logStep(cfg, "removing password")
	if err := pdftool.Decrypt(ctx, rawPath, outPath, password); err != nil {
		if errors.Is(err, pdftool.ErrWrongPassword) {
			return fmt.Errorf("the PDF rejected the supplied password")
		}
		return err
	}
	ui.OK("decrypted", "%s", outPath)

	if err := pdftool.Validate(ctx, outPath, ""); err != nil {
		return fmt.Errorf("decrypted output failed validation: %w", err)
	}
	ui.OK("validated", "document structure is intact")

	if !cfg.keepRaw && cfg.rawOut == "" {
		if err := os.Remove(rawPath); err != nil {
			return fmt.Errorf("cleanup %s: %w", rawPath, err)
		}
	}

	// Machine-readable result on stdout.
	fmt.Println(outPath)
	return nil
}

// resolvePassthroughURL decides which URL to download from:
//  1. the user URL itself when it already targets /transform/passthrough
//  2. the passthrough URL captured in the HAR
//  3. the share link resolved through the SharePoint shares API (needs
//     cookies/headers that authenticate the _api/v2.0 endpoint)
func resolvePassthroughURL(userURL string, harReq *har.AuthorizedRequest, headers http.Header, mediaPHost string) (string, error) {
	if har.IsPassthrough(userURL) {
		return userURL, nil
	}
	if harReq != nil && harReq.URL != "" {
		return harReq.URL, nil
	}

	docid, err := resolveShareDocID(userURL, headers)
	if err != nil {
		return "", fmt.Errorf("cannot resolve share URL without a HAR: %w\nprovide --har FILE or a direct transform/passthrough URL", err)
	}
	u := &url.URL{
		Scheme: "https",
		Host:   mediaPHost,
		Path:   "/transform/passthrough",
	}
	q := url.Values{}
	q.Set("provider", "spo")
	q.Set("inputFormat", "pdf")
	q.Set("docid", docid)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// resolveShareDocID calls GET /_api/v2.0/shares/u!<base64url(shareURL)>/
// driveItem and builds the docid understood by the passthrough service.
func resolveShareDocID(shareURL string, headers http.Header) (string, error) {
	u, err := url.Parse(shareURL)
	if err != nil {
		return "", fmt.Errorf("parse URL: %w", err)
	}
	if u.Host == "" || u.Scheme == "" {
		return "", fmt.Errorf("not an absolute URL: %q", shareURL)
	}

	enc := base64.RawURLEncoding.EncodeToString([]byte(shareURL))
	apiURL := fmt.Sprintf("https://%s/_api/v2.0/shares/u!%s/driveItem", u.Host, enc)

	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("shares API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("shares API returned HTTP %d", resp.StatusCode)
	}

	var item struct {
		ID              string `json:"id"`
		CTag            string `json:"cTag"`
		ParentReference struct {
			DriveID string `json:"driveId"`
		} `json:"parentReference"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return "", fmt.Errorf("decode driveItem: %w", err)
	}
	if item.ID == "" || item.ParentReference.DriveID == "" {
		return "", fmt.Errorf("driveItem response is missing id/driveId")
	}

	docid := fmt.Sprintf("https://%s:443/_api/v2.0/drives/%s/items/%s?version=Published",
		u.Host, item.ParentReference.DriveID, item.ID)
	return docid, nil
}

// deriveName picks a base file name from the share URL's id= parameter, the
// driveItem id, or a plain default.
func deriveName(userURL, passthroughURL string) string {
	if u, err := url.Parse(userURL); err == nil {
		if id := u.Query().Get("id"); id != "" {
			if base := filepath.Base(strings.ReplaceAll(id, "\\", "/")); base != "" && base != "." && base != "/" {
				return strings.TrimSuffix(base, filepath.Ext(base))
			}
		}
	}
	if u, err := url.Parse(passthroughURL); err == nil {
		q := u.Query()
		if docid := q.Get("docid"); docid != "" {
			if du, err := url.Parse(docid); err == nil {
				parts := strings.Split(strings.Trim(du.Path, "/"), "/")
				if n := len(parts); n >= 2 {
					return parts[n-1]
				}
			}
		}
	}
	return "document"
}

// promptPassword reads the PDF password from the terminal without echo, or
// from stdin when it is not a terminal.
func promptPassword() (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, ui.Cyan("PDF password: "))
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		return string(b), nil
	}
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no password: pass --password or set ODPDF_PASSWORD")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// logStep prints an informational step line unless quiet.
func logStep(cfg config, format string, args ...any) {
	if !cfg.quiet {
		ui.Info(format, args...)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
