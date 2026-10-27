package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPaintDisabledReturnsPlain(t *testing.T) {
	colorOn = false
	defer func() { colorOn = false }()
	if got := Bold("x"); got != "x" {
		t.Errorf("Bold = %q", got)
	}
	if got := Red("x"); got != "x" {
		t.Errorf("Red = %q", got)
	}
}

func TestPaintEnabledWrapsCodes(t *testing.T) {
	colorOn = true
	defer func() { colorOn = false }()
	got := Green("ok")
	if !strings.HasPrefix(got, "\x1b[") || !strings.HasSuffix(got, "\x1b[0m") {
		t.Errorf("Green = %q, want ANSI-wrapped", got)
	}
	if !strings.Contains(got, "ok") {
		t.Errorf("Green lost payload: %q", got)
	}
}

func TestOutputFunctions(t *testing.T) {
	var buf bytes.Buffer
	SetOutput(&buf)
	defer SetOutput(nil)
	colorOn = false
	SetQuiet(false)

	Step("hello %d", 1)
	OK("label", "value %d", 2)
	Warn("careful")
	Error("boom")

	s := buf.String()
	for _, want := range []string{"» hello 1", "✓", "label", "value 2", "! careful", "✗ boom"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q in %q", want, s)
		}
	}
}

func TestQuietSuppressesInfoOnly(t *testing.T) {
	var buf bytes.Buffer
	SetOutput(&buf)
	defer SetOutput(nil)
	colorOn = false
	SetQuiet(true)
	defer SetQuiet(false)

	Step("hidden")
	Info("hidden")
	Detail("key", "hidden")
	OK("shown", "always")
	Warn("shown")
	Error("shown")

	s := buf.String()
	if strings.Contains(s, "hidden") {
		t.Errorf("quiet leaked info output: %q", s)
	}
	for _, want := range []string{"always", "shown"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q in %q", want, s)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:        "0 B",
		512:      "512 B",
		1024:     "1.0 KiB",
		1536:     "1.5 KiB",
		64394367: "61.4 MiB",
	}
	for in, want := range cases {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		9300 * time.Millisecond: "9.3s",
		245 * time.Second:       "4:05",
		3723 * time.Second:      "1:02:03",
	}
	for in, want := range cases {
		if got := HumanDuration(in); got != want {
			t.Errorf("HumanDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestProgressRenders(t *testing.T) {
	var buf bytes.Buffer
	SetOutput(&buf)
	defer SetOutput(nil)
	colorOn = false
	SetQuiet(false)

	p := NewProgress()
	p.Update(50, 100)
	p.lastDraw = time.Time{} // defeat throttle for the final draw
	p.Update(100, 100)

	s := buf.String()
	if !strings.Contains(s, "100.0%") {
		t.Errorf("progress missing final pct: %q", s)
	}
	if !strings.Contains(s, "█") || !strings.Contains(s, "░") {
		t.Errorf("progress missing bar: %q", s)
	}
}
