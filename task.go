package main

import (
	"bytes"
	"fmt"
	"log"
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
	eSignal        chan bool      // exit loop, trigered on task delete
}

// TaskStatus is simple struct suitable for marshaling
type TaskStatus struct {
	Status   string    `json:"status"`
	Started  time.Time `json:"started,omitempty"`
	Finished time.Time `json:"finished,omitempty"`
}

// GetStatus return task's status in struct
func (t *Task) GetStatus() TaskStatus {
	result := TaskStatus{}
	if status, ok := t.status.Load().(string); ok {
		result.Status = status
	} else {
		result.Status = "not started"
	}
	if started, ok := t.timeStarted.Load().(time.Time); ok {
		result.Started = started
	}
	if finished, ok := t.timeFinished.Load().(time.Time); ok {
		result.Finished = finished
	}

	return result
}

// Run task one time
func (t *Task) Run(input []byte) {

	defer func() {
		t.oneTimeMutex.Lock()
		t.oneTimeRunning = false
		t.oneTimeMutex.Unlock()
	}()

	t.cSignal = make(chan os.Signal)
	t.sSignal = make(chan bool)

	config := aConfig.Load()

	// for log rotation we need layer in the middle
	writer := logWithRotation(fmt.Sprintf("%s/%s%s.log",
		config.LogDir, config.LogPrefix, t.name),
		config.LogSuffixDate, t.fSignal, config.LogDate,
		t.name, config.GrayLog)
	defer func() {
		err := writer.Close()
		if nil != err {
			log.Println("Error closing output: ", err)
		}
	}()

	fmt.Fprintf(writer, "[minisv] Starting %s %v\n", t.Command, t.Args)
	cmd := exec.Command(t.Command, t.Args...)
	cmd.Stdout = writer
	cmd.Stderr = writer
	if nil != input {
		cmd.Stdin = bytes.NewReader(input)
	}
	if t.WorkDir != "" {
		cmd.Dir = t.WorkDir
	}

	t.timeStarted.Store(time.Now())
	t.status.Store("starting")

	err := cmd.Start()
	if nil != err {
		fmt.Fprintf(writer, "[minisv] Error starting %s (%s): %v\n",
			t.name, t.Command, err)
		t.status.Store("start failed: " + err.Error())
		t.timeFinished.Store(time.Now())
		return
	}

	t.status.Store("running")

	cmdDone := make(chan error)
	go func() {
		cmdDone <- cmd.Wait()
	}()

	killIt := make(chan bool)
	killCanceled := false

itsDone:
	for {
		select {

		case err = <-cmdDone:
			killCanceled = true
			break itsDone

		case sig := <-t.cSignal:
			fmt.Fprintln(writer, "[minisv] Sending ", sig,
				" signal to process ", cmd.Process.Pid)
			err = cmd.Process.Signal(sig)
			if nil != err {
				fmt.Fprintln(writer, "[minisv] Error sending ", sig, ": ", err)
			}

		case <-t.sSignal:
			fmt.Fprintln(writer, "[minisv] Stopping task")
			err = cmd.Process.Signal(syscall.SIGTERM)
			if nil != err {
				fmt.Fprintln(writer, "[minisv] Error sending SIGTERM: ", err)
			}
			go func() {
				time.Sleep(time.Duration(t.Wait) * time.Second)
				killIt <- true
			}()

		case <-killIt:
			if !killCanceled {
				fmt.Fprintln(writer, "[minisv] Killing task")
				err = cmd.Process.Signal(syscall.SIGKILL)
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

// Loop task runinng and restarting
func (t *Task) Loop(cExit chan bool, wg *sync.WaitGroup) {
	defer wg.Done()

	// init channels
	t.cSignal = make(chan os.Signal)
	t.rSignal = make(chan bool)
	t.fSignal = make(chan bool)
	t.sSignal = make(chan bool)
	t.eSignal = make(chan bool)

	config := aConfig.Load()

	// for log rotation we need layer in the middle
	out := logWithRotation(fmt.Sprintf("%s/%s%s.log",
		config.LogDir, config.LogPrefix, t.name),
		config.LogSuffixDate, t.fSignal, config.LogDate,
		t.name, config.GrayLog)
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
		cmd := exec.Command(t.Command, t.Args...)
		cmd.Stdout = out
		cmd.Stderr = out
		if t.WorkDir != "" {
			cmd.Dir = t.WorkDir
		}

		t.status.Store("starting")
		t.timeStarted.Store(time.Now())
		err = cmd.Start()
		if nil != err {
			t.status.Store("Error starting: " + err.Error())
			fmt.Fprintf(out, "[minisv] Error starting %s (%s): %v\n",
				t.name, t.Command, err)
			time.Sleep(time.Second * time.Duration(t.Pause))
			return nil, nil, err
		}
		t.status.Store(okstatus)

		cmdDone := make(chan error)
		go func() {
			cmdDone <- cmd.Wait()
		}()

		return cmd, cmdDone, nil
	}

	var cmd1, cmd2 *exec.Cmd
	var done1, done2 chan error
	var run1, run2 bool

	for {
		if !t.stopped {
			if stage && !run1 {
				cmd1, done1, err = startNext("started")
				run1 = nil == err
			}
			if !stage && !run2 {
				cmd2, done2, err = startNext("started")
				run2 = nil == err
			}
		}

		select {
		case err = <-done1:

			run1 = false

			if stage {

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
			if stage {
				fmt.Fprintln(out, "[minisv] Sending ", sig,
					" signal to process ", cmd1.Process.Pid)
				err = cmd1.Process.Signal(sig)
				if nil != err {
					fmt.Fprintln(out, "[minisv] Error sending ", sig, ": ", err)
				}
			} else {
				fmt.Fprintln(out, "[minisv] Sending ", sig,
					" signal to process ", cmd2.Process.Pid)
				err = cmd2.Process.Signal(sig)
				if nil != err {
					fmt.Fprintln(out, "[minisv] Error sending ", sig, ": ", err)
				}
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

			t.timeFinished.Store(time.Now())
			t.status.Store("stoped")

			continue

		case <-t.rSignal:
			if t.stopped {
				t.stopped = false
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
					t.status.Store("new instance exited too fast")
					fmt.Fprintln(out,
						"[minisv] New instance exited too fast, continue using old one")
					continue
				}

				stage = !stage

				t.status.Store("restart ok")
				fmt.Fprintln(out, "[minisv] New instance running, terminating old one")
				if stage {
					termChild(run2, cmd2, done2, t.Wait, out, nil)
				} else {
					termChild(run1, cmd1, done1, t.Wait, out, nil)
				}
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

		if t.Pause != 0 {
			fmt.Fprintln(out, "Waiting ",
				time.Second*time.Duration(t.Pause), " before restart")
			time.Sleep(time.Second * time.Duration(t.Pause))
		}
	}
}
