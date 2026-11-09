package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// pids of the children minisv started: the reaper must not wait() on them,
// their exit status belongs to cmd.Wait
var (
	childMu   sync.Mutex
	childPids = map[int]bool{}
)

// newCmd builds the command of the task writing its output to out
func (t *Task) newCmd(out io.Writer) *exec.Cmd {
	cmd := exec.Command(t.Command, t.Args...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Dir = t.WorkDir
	// own process group, so stop/kill reach grandchildren as well
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// after the process exits don't wait forever for stdout/stderr EOF:
	// a grandchild holding the pipe open would block restart and status
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

// startCmd starts cmd and returns a channel delivering the result of Wait
func startCmd(cmd *exec.Cmd) (chan error, error) {
	// held across Start, so the reaper can't take the exit status
	// of a child that died before it was registered
	childMu.Lock()
	err := cmd.Start()
	if nil == err {
		childPids[cmd.Process.Pid] = true
	}
	childMu.Unlock()
	if nil != err {
		return nil, err
	}

	// buffered: after a restart or stop nobody may be receiving anymore
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if errors.Is(err, exec.ErrWaitDelay) {
			// exit status is fine, a grandchild still held the output pipe
			fmt.Fprintln(cmd.Stdout, "[minisv] Output pipe closed, a child outlived the process")
			err = nil
		}
		childMu.Lock()
		delete(childPids, cmd.Process.Pid)
		childMu.Unlock()
		done <- err
	}()
	return done, nil
}

// signalGroup sends sig to the whole process group of cmd. Signal 0
// through os.Process only checks that the process was not waited for
// yet: a raw kill(-pid) after that could hit a reused pid. The leader is
// in the group, so sig itself is sent once
func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if err := cmd.Process.Signal(syscall.Signal(0)); nil != err {
		return err
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}

// sendSignal delivers a user requested signal: TERM and KILL go to the
// whole group so children do not outlive the task, HUP only to the leader,
// which reloads and handles its own children
func sendSignal(cmd *exec.Cmd, sig os.Signal) error {
	switch sig {
	case syscall.SIGTERM, syscall.SIGKILL:
		return signalGroup(cmd, sig.(syscall.Signal))
	}
	return cmd.Process.Signal(sig)
}

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
	// the exit status may already be waiting: the process is gone and the
	// pid may belong to somebody else by now, nothing to signal
	select {
	case <-ch:
		return
	default:
	}

	err := signalGroup(cmd, syscall.SIGTERM)
	if nil != err {
		_, e := fmt.Fprintln(out, "Error sending TERM signal: ", err)
		if nil != e {
			log.Println("Error writing log: ", e)
		}
	}

	if !waitForErrChan(ch, time.Duration(wait)*time.Second) {
		_, e := fmt.Fprintln(out, "Process is still running, sending kill signal")
		if nil != e {
			log.Println("Error writing log: ", e)
		}

		err = signalGroup(cmd, syscall.SIGKILL)
		if nil != err {
			_, e := fmt.Fprintln(out, "Error sending KILL signal: ", err)
			if nil != e {
				log.Println("Error writing log: ", e)
			}
		}
		// don't wait on ch here: cmd.Wait() also waits for stdout/stderr
		// EOF, which an orphaned grandchild may hold open for a long time
	}

}
