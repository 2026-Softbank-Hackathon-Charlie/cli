package cli

import (
	"io"
	"os"

	"golang.org/x/term"
)

// ansiTerminal reports whether w is a terminal that understands ANSI escapes.
func ansiTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd())) && enableANSI(f.Fd())
}

// termSize returns the terminal's columns and rows, with a fallback.
func termSize(w io.Writer) (cols, rows int) {
	if f, ok := w.(*os.File); ok {
		if c, r, err := term.GetSize(int(f.Fd())); err == nil && c > 0 && r > 0 {
			return c, r
		}
	}
	return 100, 40
}
