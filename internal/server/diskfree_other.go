//go:build !unix

package server

// diskFree is unavailable on this platform; staging space is not checked.
func diskFree(string) (uint64, bool) { return 0, false }
