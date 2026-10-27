//go:build windows

package ui

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVirtualTerminal turns on ANSI escape processing for the console
// attached to out (needed in legacy conhost; Windows Terminal is always on).
func enableVirtualTerminal(out *os.File) {
	if out == nil {
		return
	}
	h := windows.Handle(out.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return
	}
	_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
}
