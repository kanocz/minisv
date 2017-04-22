package main

import (
	"fmt"
	"io"
	"log"
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

func termChild(running bool, cmd *exec.Cmd, ch chan error,
	wait int, out io.Writer, wg *sync.WaitGroup) {
	if nil != wg {
		defer wg.Done()
	}
	if !running {
		return
	}

	err := cmd.Process.Signal(syscall.SIGTERM)
	if nil != err {
		_, e := fmt.Fprintln(out, "Error sending TERM signal: ", err)
		if nil != e {
			log.Println("Error writing log: ", err)
		}
	}

	if !waitForErrChan(ch, time.Duration(wait)*time.Second) {
		_, e := fmt.Fprintln(out, "Process is still running, sending kill signal")
		if nil != e {
			log.Println("Error writing log: ", err)
		}

		err = cmd.Process.Kill()
		if nil != err {
			_, e := fmt.Fprintln(out, "Error sending KILL signal: ", err)
			if nil != e {
				log.Println("Error writing log: ", err)
			}
		}
	}

}
