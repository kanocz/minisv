package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Task represents one running process/application
type Task struct {
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	WorkDir   string   `json:"workdir"`
	Wait      int      `json:"wait"`
	Pause     int      `json:"restartPause"`
	StartTime int      `json:"startTime"`
	OneTime   bool     `json:"oneTime"`
	// hidden fields
	stopped        bool           // indicate to don't restart after "die"
	oneTimeRunning bool           // indicate that we're just running
	oneTimeMutex   sync.Mutex     // mutex for oneTimeRunning
	status         atomic.Value   // string like "none" (not started at all), "running", "finished", "restarting"
	timeStarted    atomic.Value   // when task started (time.Time / nil)
	timeFinished   atomic.Value   // last task finished time (time.Time / nil)
	name           string         // duplicate name from config
	cSignal        chan os.Signal // send signal to process
	rSignal        chan bool      // restart signal
	fSignal        chan bool      // log flush signal
	sSignal        chan bool      // signal to stop task
	eSignal        chan bool      // exit loop, triggered on task delete
	logBuffer      []string       // buffer for last 10 log lines
	logBufferMutex sync.RWMutex   // mutex for log buffer operations
}

// initChannels prepares control channels; must be called once, before
// Run/Loop start and before the task is visible to http handlers —
// recreating channels later would race with handlers reading the fields
func (t *Task) initChannels() {
	// buffered: http handlers send without blocking, and the loop may be
	// busy terminating a child or validating a restart at that moment -
	// an unbuffered channel would silently drop the request then
	t.cSignal = make(chan os.Signal, 1)
	t.fSignal = make(chan bool, 1)
	t.sSignal = make(chan bool, 1)
	// restart stays unbuffered: a queued second restart is never wanted
	t.rSignal = make(chan bool)
	t.eSignal = make(chan bool)
}

// TaskStatus is simple struct suitable for marshaling
type TaskStatus struct {
	Status   string    `json:"status"`
	Running  bool      `json:"running"`
	Started  time.Time `json:"started,omitzero"`
	Finished time.Time `json:"finished,omitzero"`
}

// isRunning tells whether a process is serving in this status; after
// a failed graceful restart the old instance keeps running
func isRunning(status string) bool {
	switch status {
	case "starting", "started", "running", "restart validation", "restart ok",
		"new instance failed", "new instance exited too fast":
		return true
	}
	return false
}

// GetStatus return task's status in struct
func (t *Task) GetStatus() TaskStatus {
	result := TaskStatus{Status: "not started"}
	if status, ok := t.status.Load().(string); ok {
		result.Status = status
	}
	result.Running = isRunning(result.Status)
	if started, ok := t.timeStarted.Load().(time.Time); ok {
		result.Started = started
	}
	// the finished time of the previous run means nothing while this one runs
	if finished, ok := t.timeFinished.Load().(time.Time); ok && !result.Running {
		result.Finished = finished
	}
	return result
}

// startOneTime marks the task as running and drops a stop or signal queued
// while it was idle; the http status and message when it can't. Done under
// the mutex so a stop sent through signalOneTime never lands in the gap
// between the check and the drain
func (t *Task) startOneTime() (int, string) {
	t.oneTimeMutex.Lock()
	defer t.oneTimeMutex.Unlock()
	if t.oneTimeRunning {
		return http.StatusConflict, "already running"
	}
	if exiting.Load() {
		return http.StatusServiceUnavailable, "minisv is exiting"
	}
	select {
	case <-t.cSignal:
	default:
	}
	select {
	case <-t.sSignal:
	default:
	}
	t.oneTimeRunning = true
	oneTimeWg.Add(1)
	return 0, ""
}

// signalOneTime runs send while the task is known to be running; false
// when it is idle and nobody would receive
func (t *Task) signalOneTime(send func()) bool {
	t.oneTimeMutex.Lock()
	defer t.oneTimeMutex.Unlock()
	if !t.oneTimeRunning {
		return false
	}
	send()
	return true
}

// dropStaleKill removes a term or kill queued for an instance that is
// already gone. With keepHup a hup stays in the buffer for the instance
// that took over; after a stop there is none, so it is dropped as well.
// Put back without blocking: Loop is the only reader, and a handler may
// have filled the slot meanwhile
func (t *Task) dropStaleKill(keepHup bool) {
	select {
	case sig := <-t.cSignal:
		if keepHup && sig == syscall.SIGHUP {
			select {
			case t.cSignal <- sig:
			default:
			}
		}
	default:
	}
}

// Run task one time
func (t *Task) Run(input []byte) {

	defer func() {
		t.oneTimeMutex.Lock()
		t.oneTimeRunning = false
		t.oneTimeMutex.Unlock()
		oneTimeWg.Done()
	}()

	config := aConfig.Load()

	// for log rotation we need layer in the middle
	writer := logWithRotation(fmt.Sprintf("%s/%s%s.log",
		config.LogDir, config.LogPrefix, t.name),
		config.LogSuffixDate, t.fSignal, config.LogDate,
		t.name, config.GrayLog, t)
	defer func() {
		err := writer.Close()
		if nil != err {
			log.Println("Error closing output: ", err)
		}
	}()

	fmt.Fprintf(writer, "[minisv] Starting %s %v\n", t.Command, t.Args)
	cmd := t.newCmd(writer)
	if nil != input {
		cmd.Stdin = bytes.NewReader(input)
	}

	t.timeStarted.Store(time.Now())
	t.status.Store("starting")

	cmdDone, err := startCmd(cmd)
	if nil != err {
		fmt.Fprintf(writer, "[minisv] Error starting %s (%s): %v\n",
			t.name, t.Command, err)
		t.status.Store("start failed: " + err.Error())
		t.timeFinished.Store(time.Now())
		return
	}

	t.status.Store("running")

	// buffered: the sender goroutine must not block forever
	// when the process exits before the kill timeout fires
	killIt := make(chan bool, 1)
	killCanceled := false

	stop := func(reason string) {
		fmt.Fprintln(writer, "[minisv] "+reason)
		err = signalGroup(cmd, syscall.SIGTERM)
		if nil != err {
			fmt.Fprintln(writer, "[minisv] Error sending SIGTERM: ", err)
		}
		go func() {
			time.Sleep(time.Duration(t.Wait) * time.Second)
			select {
			case killIt <- true:
			default:
			}
		}()
	}

	// nil once handled: a closed channel would fire on every select
	exit := needExit

itsDone:
	for {
		select {

		case err = <-cmdDone:
			killCanceled = true
			break itsDone

		case sig := <-t.cSignal:
			fmt.Fprintln(writer, "[minisv] Sending ", sig,
				" signal to process ", cmd.Process.Pid)
			err = sendSignal(cmd, sig)
			if nil != err {
				fmt.Fprintln(writer, "[minisv] Error sending ", sig, ": ", err)
			}

		case <-t.sSignal:
			stop("Stopping task")

		case <-exit:
			exit = nil
			stop("Terminating task, minisv is exiting")

		case <-killIt:
			if !killCanceled {
				fmt.Fprintln(writer, "[minisv] Killing task")
				err = signalGroup(cmd, syscall.SIGKILL)
				if nil != err {
					fmt.Fprintln(writer, "[minisv] Error sending SIGKILL: ", err)
				}
			}

		}
	}

	if nil != err {
		t.status.Store("finished with error: " + err.Error())
		fmt.Fprintf(writer, "[minisv] Command %s (%s) ended with error: %v\n",
			t.name, t.Command, err)
	} else {
		t.status.Store("finished")
	}
	t.timeFinished.Store(time.Now())
}

// Loop task running and restarting
func (t *Task) Loop(cExit chan bool, wg *sync.WaitGroup) {
	defer wg.Done()

	config := aConfig.Load()

	// for log rotation we need layer in the middle
	out := logWithRotation(fmt.Sprintf("%s/%s%s.log",
		config.LogDir, config.LogPrefix, t.name),
		config.LogSuffixDate, t.fSignal, config.LogDate,
		t.name, config.GrayLog, t)
	defer func() {
		err := out.Close()
		if nil != err {
			log.Println("Error closing output: ", err)
		}
	}()

	var err error

	// true - main is cmd1, false - main is cmd2 :)
	stage := true

	startNext := func(okstatus string) (*exec.Cmd, chan error, error) {
		fmt.Fprintf(out, "[minisv] Starting %s %v\n", t.Command, t.Args)
		cmd := t.newCmd(out)

		t.status.Store("starting")
		t.timeStarted.Store(time.Now())
		cmdDone, err := startCmd(cmd)
		if nil != err {
			t.status.Store("Error starting: " + err.Error())
			fmt.Fprintf(out, "[minisv] Error starting %s (%s): %v\n",
				t.name, t.Command, err)
			return nil, nil, err
		}
		t.status.Store(okstatus)

		return cmd, cmdDone, nil
	}

	var cmd1, cmd2 *exec.Cmd
	var done1, done2 chan error
	var run1, run2 bool

	// pause between start attempts when the command fails to start at all;
	// at least a second even with no restartPause configured, to avoid
	// a busy-loop on a permanently broken command
	retryPause := time.Duration(t.Pause) * time.Second
	if retryPause < time.Second {
		retryPause = time.Second
	}

	// non-nil while waiting before the next start attempt (restart pause
	// or failed-start retry); a timer in the select instead of time.Sleep
	// keeps the loop responsive to signals/stop/exit during the pause
	var startDelay <-chan time.Time

	for {
		if !t.stopped && nil == startDelay {
			if stage && !run1 {
				cmd1, done1, err = startNext("started")
				run1 = nil == err
			}
			if !stage && !run2 {
				cmd2, done2, err = startNext("started")
				run2 = nil == err
			}
			// start failed - retry after a pause instead of waiting forever
			if (stage && !run1) || (!stage && !run2) {
				startDelay = time.After(retryPause)
			}
		}

		select {
		case <-startDelay:
			startDelay = nil
			continue
		case err = <-done1:

			run1 = false

			if stage {

				// exit status of a process terminated by stop may arrive
				// late (cmd.Wait also waits for stdout/stderr EOF) —
				// don't overwrite the "stopped" status then
				if t.stopped {
					fmt.Fprintln(out, "[minisv] Process exited after stop")
					continue
				}

				t.timeFinished.Store(time.Now())

				if nil == err {
					fmt.Fprintln(out, string("[minisv] Main process normal exit"))
					t.status.Store("finished")
				} else {
					fmt.Fprintln(out, "[minisv] Main process exited, ", err)
					t.status.Store("finished with error: " + err.Error())
				}

			} else {

				if nil == err {
					fmt.Fprintln(out, "[minisv] Old process normal exit")
				} else {
					fmt.Fprintln(out, "[minisv] Old process exited, ", err)
				}
				// don't need wait after old process exit
				continue
			}

		case err = <-done2:

			run2 = false

			if stage {

				if nil == err {
					fmt.Fprintln(out, "[minisv] Old process normal exit")
				} else {
					fmt.Fprintln(out, "[minisv] Old process exited, ", err)
				}
				// don't need wait after old process exit
				continue

			} else {

				// see the done1 case above
				if t.stopped {
					fmt.Fprintln(out, "[minisv] Process exited after stop")
					continue
				}

				t.timeFinished.Store(time.Now())

				if nil == err {
					fmt.Fprintln(out, "[minisv] Main process normal exit")
					t.status.Store("finished")
				} else {
					fmt.Fprintln(out, "[minisv] Main process exited, ", err)
					t.status.Store("finished with error: " + err.Error())
				}

			}

		case sig := <-t.cSignal:
			cmd, running := cmd1, run1
			if !stage {
				cmd, running = cmd2, run2
			}
			if !running || nil == cmd || nil == cmd.Process {
				fmt.Fprintln(out, "[minisv] No running process to send ",
					sig, " signal to")
				continue
			}

			fmt.Fprintln(out, "[minisv] Sending ", sig,
				" signal to process ", cmd.Process.Pid)
			err = sendSignal(cmd, sig)
			if nil != err {
				fmt.Fprintln(out, "[minisv] Error sending ", sig, ": ", err)
			}

			continue

		case <-t.sSignal:
			fmt.Fprintln(out, "[minisv] Stopping task")
			t.stopped = true

			if stage {
				termChild(run1, cmd1, done1, t.Wait, out, nil)
				run1 = false
			} else {
				termChild(run2, cmd2, done2, t.Wait, out, nil)
				run2 = false
			}

			// a kill sent while termChild was busy must not hit the next start
			t.dropStaleKill(false)
			t.timeFinished.Store(time.Now())
			t.status.Store("stopped")

			continue

		case <-t.rSignal:
			if t.stopped {
				t.stopped = false
				// start immediately, don't wait out a pending pause
				startDelay = nil
				fmt.Fprintln(out, "[minisv] Starting task")
			} else {

				fmt.Fprintln(out, "[minisv] Doing graceful restart")

				// castling of running processes
				if stage {
					cmd2, done2, err = startNext("restart validation")
					run2 = nil == err

				} else {
					cmd1, done1, err = startNext("restart validation")
					run1 = nil == err
				}

				if nil != err {
					t.status.Store("new instance failed")
					fmt.Fprintln(out,
						"[minisv] Unable to start new instance, continue using old one")
					continue
				}

				var exited bool
				if stage {
					exited = waitForErrChan(done2, time.Second*time.Duration(t.StartTime))
				} else {
					exited = waitForErrChan(done1, time.Second*time.Duration(t.StartTime))
				}

				if exited {
					// its exit status is consumed, the done case will not fire
					if stage {
						run2 = false
					} else {
						run1 = false
					}
					t.status.Store("new instance exited too fast")
					fmt.Fprintln(out,
						"[minisv] New instance exited too fast, continue using old one")
					continue
				}

				stage = !stage

				t.status.Store("restart ok")
				fmt.Fprintln(out, "[minisv] New instance running, terminating old one")
				// clear the flag: termChild may have consumed the exit status
				// from the done channel, so the done case will not fire for it
				if stage {
					termChild(run2, cmd2, done2, t.Wait, out, nil)
					run2 = false
				} else {
					termChild(run1, cmd1, done1, t.Wait, out, nil)
					run1 = false
				}
				// a term or kill sent while the old instance was still there
				// was aimed at it, not at the new one
				t.dropStaleKill(true)
			}

			continue

		case <-cExit:
			fmt.Fprintln(out, "[minisv] Sending term signal to childs")
			smallWg := sync.WaitGroup{}
			smallWg.Add(2)
			go termChild(run1, cmd1, done1, t.Wait, out, &smallWg)
			go termChild(run2, cmd2, done2, t.Wait, out, &smallWg)
			smallWg.Wait()
			return

		case <-t.eSignal:
			fmt.Fprintln(out, "[minisv] {taskExit} Sending term signal to childs")
			smallWg := sync.WaitGroup{}
			smallWg.Add(2)
			go termChild(run1, cmd1, done1, t.Wait, out, &smallWg)
			go termChild(run2, cmd2, done2, t.Wait, out, &smallWg)
			smallWg.Wait()
			return
		}

		pause := time.Second * time.Duration(t.Pause)
		// a process dying right after start must not restart in a busy loop
		if started, ok := t.timeStarted.Load().(time.Time); ok &&
			time.Since(started) < time.Second {
			pause = retryPause
		}
		if pause > 0 {
			fmt.Fprintln(out, "[minisv] Waiting ", pause, " before restart")
			startDelay = time.After(pause)
		}
	}
}
