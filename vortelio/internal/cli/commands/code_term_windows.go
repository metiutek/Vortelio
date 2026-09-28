//go:build windows

package commands

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableOutputProcessing is a no-op on Windows: raw mode only changes the input
// console mode, output keeps translating "\n".
func enableOutputProcessing(int) {}

// enableVTOutput turns on ANSI escape handling for the console (colors, cursor
// movement) on hosts where it is not already enabled.
func enableVTOutput() {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) == nil {
		windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.ENABLE_PROCESSED_OUTPUT)
	}
}
