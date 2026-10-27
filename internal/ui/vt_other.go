//go:build !windows

package ui

import "os"

// enableVirtualTerminal is a no-op: POSIX terminals handle ANSI natively.
func enableVirtualTerminal(out *os.File) {}
