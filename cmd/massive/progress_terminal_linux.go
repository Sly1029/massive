//go:build linux

package main

import (
	"golang.org/x/sys/unix"
	"os"
)

func progressTerminal(file *os.File) bool {
	_, err := unix.IoctlGetTermios(int(file.Fd()), unix.TCGETS)
	return err == nil
}
