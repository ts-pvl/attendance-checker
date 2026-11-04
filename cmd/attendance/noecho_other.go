//go:build !linux

package main

// noEcho is a no-op outside Linux; the server targets Linux only.
func noEcho(int) func() { return func() {} }
