package main

import (
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

func waitForErrChan(c chan error, t time.Duration) bool {
	select {
	case <-time.After(t):
		return false
	case <-c:
		return true
	}
}

func termChild(running bool, cmd *exec.Cmd, ch chan error, wait int, out io.Writer, wg *sync.WaitGroup) {
	if nil != wg {
		defer wg.Done()
	}
	if !running {
		return
	}

	cmd.Process.Signal(syscall.SIGTERM)
	if !waitForErrChan(ch, time.Duration(wait)*time.Second) {
		fmt.Fprintln(out, "Process is still runing, sending kill signal")
		cmd.Process.Kill()
	}

}
