//go:build windows

package main

import (
	"golang.org/x/sys/windows"
	"os"
)

func progressTerminal(file *os.File) bool {
	var mode uint32
	handle := windows.Handle(file.Fd())
	if windows.GetConsoleMode(handle, &mode) != nil {
		return false
	}
	// Enable the existing Windows console's ANSI support for the compact line.
	return windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
