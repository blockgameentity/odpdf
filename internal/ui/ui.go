// Package ui renders color-coded, terminal-friendly CLI output: styled
// status lines, aligned key/value details and a live progress bar.
//
// Colors follow the standard conventions: they are enabled automatically
// when writing to a terminal, can be forced with mode "always" or disabled
// with mode "never", and honor the NO_COLOR and TERM=dumb environment
// variables.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// ANSI SGR codes.
const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	red    = "\x1b[31m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	blue   = "\x1b[34m"
	cyan   = "\x1b[36m"
	gray   = "\x1b[90m"
)

var (
	colorOn bool
	quiet   bool

	mu  sync.Mutex
	out io.Writer = os.Stderr
)

// Init configures color output. mode is "auto", "always" or "never"; out is
// the terminal stream the ANSI sequences are written to.
func Init(mode string, outStream *os.File) {
	switch mode {
	case "always":
		colorOn = true
	case "never":
		colorOn = false
	default: // auto
		if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
			colorOn = false
			return
		}
		if outStream != nil {
			colorOn = term.IsTerminal(int(outStream.Fd()))
		}
	}
	if colorOn && outStream != nil {
		enableVirtualTerminal(outStream)
	}
}

// SetQuiet suppresses informational output (Step, Info, Detail, progress).
// Warnings, errors and final OK lines are always shown.
func SetQuiet(q bool) { quiet = q }

// SetOutput redirects UI output (defaults to stderr).
func SetOutput(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	out = w
}

// Enabled reports whether color sequences are emitted.
func Enabled() bool { return colorOn }

func paint(code, s string) string {
	if !colorOn {
		return s
	}
	return code + s + reset
}

// Bold, Dim and the color helpers style a string when colors are enabled and
// return it unchanged otherwise.
func Bold(s string) string   { return paint(bold, s) }
func Dim(s string) string    { return paint(dim, s) }
func Red(s string) string    { return paint(red, s) }
func Green(s string) string  { return paint(green, s) }
func Yellow(s string) string { return paint(yellow, s) }
func Blue(s string) string   { return paint(blue, s) }
func Cyan(s string) string   { return paint(cyan, s) }
func Gray(s string) string   { return paint(gray, s) }

func printf(format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintf(out, format, args...)
}

// Step prints a cyan section marker line, e.g. "» downloading".
func Step(format string, args ...any) {
	if quiet {
		return
	}
	printf("%s %s\n", Cyan("»"), fmt.Sprintf(format, args...))
}

// Info prints a plain informational line.
func Info(format string, args ...any) {
	if quiet {
		return
	}
	printf("  %s\n", fmt.Sprintf(format, args...))
}

// Detail prints an aligned key/value line with a dim key label.
func Detail(key, format string, args ...any) {
	if quiet {
		return
	}
	printf("  %-9s %s\n", Gray(key), fmt.Sprintf(format, args...))
}

// OK prints a green check line with a bold label.
func OK(label, format string, args ...any) {
	printf("%s %-10s %s\n", Green("✓"), Bold(label), fmt.Sprintf(format, args...))
}

// Warn prints a yellow warning line (never suppressed).
func Warn(format string, args ...any) {
	printf("%s %s\n", Yellow("!"), Yellow(fmt.Sprintf(format, args...)))
}

// Error prints a red error line (never suppressed).
func Error(format string, args ...any) {
	printf("%s %s\n", Red("✗"), Red(fmt.Sprintf(format, args...)))
}

// Progress renders a live single-line progress bar on the output stream.
type Progress struct {
	mu       sync.Mutex
	start    time.Time
	lastDraw time.Time
	prevLen  int
}

// NewProgress returns a progress renderer whose lifetime starts now.
func NewProgress() *Progress {
	return &Progress{start: time.Now()}
}

// Update redraws the progress bar; it self-throttles to ~10 redraws/sec and
// always draws the final state when done reaches total.
func (p *Progress) Update(done, total int64) {
	if p == nil || quiet {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	final := total > 0 && done >= total
	if !final && now.Sub(p.lastDraw) < 100*time.Millisecond {
		return
	}
	p.lastDraw = now

	var pct float64
	if total > 0 {
		pct = float64(done) * 100 / float64(total)
	}
	const width = 28
	filled := int(pct / 100 * width)
	bar := Cyan(strings.Repeat("█", filled)) + Gray(strings.Repeat("░", width-filled))

	elapsed := now.Sub(p.start)
	rate := ""
	eta := ""
	if elapsed > time.Second && done > 0 {
		bps := float64(done) / elapsed.Seconds()
		rate = fmt.Sprintf("%.1f MiB/s", bps/(1024*1024))
		if !final && bps > 0 {
			eta = fmt.Sprintf("ETA %s", HumanDuration(time.Duration(float64(total-done)/bps*float64(time.Second))))
		}
	}

	line := fmt.Sprintf("  [%s] %5.1f%%  %s / %s  %s %s",
		bar, pct, HumanBytes(done), HumanBytes(total), Gray(rate), Gray(eta))

	erase := ""
	if colorOn {
		erase = "\x1b[K"
	} else if pad := p.prevLen - len(line); pad > 0 {
		erase = strings.Repeat(" ", pad)
	}
	p.prevLen = len(line)
	printf("\r%s%s", line, erase)
	if final {
		printf("\n")
	}
}

// HumanBytes formats a byte count as B, KiB, MiB, ...
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// HumanDuration formats a duration compactly: 9.3s, 4:05, 1:02:03.
func HumanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
	}
}
