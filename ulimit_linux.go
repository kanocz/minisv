package main

import "syscall"

var (
	rlimitTypes = map[string]int{
		"as":     syscall.RLIMIT_AS,
		"core":   syscall.RLIMIT_CORE,
		"cpu":    syscall.RLIMIT_CPU,
		"data":   syscall.RLIMIT_DATA,
		"fsize":  syscall.RLIMIT_FSIZE,
		"nofile": syscall.RLIMIT_NOFILE,
		"nproc":  0x6, // don't know why it's not defined in go :(
		"stack":  syscall.RLIMIT_STACK,
	}
)
