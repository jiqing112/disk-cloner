//go:build !windows

package cli

// SetupConsole is a no-op on non-Windows platforms (terminals are already
// UTF-8).
func SetupConsole() {}
