//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package main

import "os"

func progressTerminal(*os.File) bool { return false }
