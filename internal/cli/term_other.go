//go:build !windows

package cli

// enableANSI is a no-op: Unix terminals understand ANSI escapes.
func enableANSI(uintptr) bool { return true }
