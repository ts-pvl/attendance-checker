//go:build linux

package main

import "golang.org/x/sys/unix"

// noEcho turns off terminal echo while a password is typed and returns a
// function that restores it. It does nothing when stdin is not a terminal.
func noEcho(fd int) func() {
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return func() {}
	}
	t := *old
	t.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &t); err != nil {
		return func() {}
	}
	return func() { unix.IoctlSetTermios(fd, unix.TCSETS, old) }
}
