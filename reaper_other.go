//go:build !linux

package main

// PID 1 inside a container is a Linux matter; nothing to reap elsewhere
func startReaper() {}
