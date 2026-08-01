//go:build !windows

package main

import "syscall"

// processAlive reports whether a PID still refers to a live process.
func processAlive(pid int) bool {
	// Signal 0 performs the permission and existence checks without delivering.
	return syscall.Kill(pid, 0) == nil
}
