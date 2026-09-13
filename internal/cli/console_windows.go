//go:build windows

package cli

import "golang.org/x/sys/windows"

// SetupConsole switches the Windows console to UTF-8 (code page 65001) so
// Chinese text and file paths are read and displayed correctly. Done via the
// Win32 API directly — spawning PowerShell/chcp for this cost several
// hundred milliseconds on every start.
func SetupConsole() {
	_ = windows.SetConsoleOutputCP(65001)
	_ = windows.SetConsoleCP(65001)
}
