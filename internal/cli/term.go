package cli

import (
	"context"
	"errors"
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

// isTerminal reports whether f, the App's stdin or stdout, is a terminal.
// Tests replace it.
var isTerminal = func(f any) bool {
	file, ok := f.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

// readHidden reads one line from the terminal r without echoing it. Ctrl-C
// ends it at once and gives the terminal its echo back. Tests replace it.
var readHidden = func(ctx context.Context, r io.Reader) ([]byte, error) {
	f, ok := r.(*os.File)
	if !ok {
		return nil, errors.New("stdin is not a terminal")
	}
	fd := int(f.Fd())
	state, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	type line struct {
		b   []byte
		err error
	}
	done := make(chan line, 1)
	go func() {
		b, err := term.ReadPassword(fd)
		done <- line{b, err}
	}()
	select {
	case l := <-done:
		return l.b, l.err
	case <-ctx.Done():
		_ = term.Restore(fd, state) // ReadPassword restores it only after Enter
		return nil, ctx.Err()
	}
}
