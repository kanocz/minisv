package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
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
	stopped bool           // indicate to don't restart after "die"
	name    string         // duplicate name from config
	cSignal chan os.Signal // send signal to process
	rSignal chan bool      // restart signal
	fSignal chan bool      // log flush signal
	sSignal chan bool      // signal to stop task
}

// Run task one time
func (t *Task) Run() {
	// for log rotation we need layer in the middle
	writer := logWithRotation(fmt.Sprintf("%s/%s%s.log",
		config.LogDir, config.LogPrefix, t.name),
		config.LogSuffixDate, t.fSignal, config.LogDate)
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
	if "" != t.WorkDir {
		cmd.Dir = t.WorkDir
	}

	err := cmd.Start()
	if nil != err {
		fmt.Fprintf(writer, "[minisv] Error starting %s (%s): %v\n",
			t.name, t.Command, err)
		return
	}

	err = cmd.Wait()
	if nil != err {
		fmt.Fprintf(writer, "[minisv] Command %s (%s) ended with error: %v\n",
			t.name, t.Command, err)
	}
}

// Loop task runinng and restarting
func (t *Task) Loop(cExit chan bool, wg *sync.WaitGroup) {
	defer wg.Done()

	// init channels
	t.cSignal = make(chan os.Signal)
	t.rSignal = make(chan bool)
	t.fSignal = make(chan bool)
	t.sSignal = make(chan bool)

	// for log rotation we need layer in the middle
	out := logWithRotation(fmt.Sprintf("%s/%s%s.log",
		config.LogDir, config.LogPrefix, t.name),
		config.LogSuffixDate, t.fSignal, config.LogDate)
	defer func() {
		err := out.Close()
		if nil != err {
			log.Println("Error closing output: ", err)
		}
	}()

	var err error

	// true - main is cmd1, false - main is cmd2 :)
	stage := true

	startNext := func() (*exec.Cmd, chan error, error) {
		fmt.Fprintf(out, "[minisv] Starting %s %v\n", t.Command, t.Args)
		cmd := exec.Command(t.Command, t.Args...)
		cmd.Stdout = out
		cmd.Stderr = out
		if "" != t.WorkDir {
			cmd.Dir = t.WorkDir
		}

		err = cmd.Start()
		if nil != err {
			fmt.Fprintf(out, "[minisv] Error starting %s (%s): %v\n",
				t.name, t.Command, err)
			time.Sleep(time.Second * time.Duration(t.Pause))
			return nil, nil, err
		}

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
				cmd1, done1, err = startNext()
				run1 = nil == err
			}
			if !stage && !run2 {
				cmd2, done2, err = startNext()
				run2 = nil == err
			}
		}

		select {
		case err = <-done1:

			run1 = false

			if stage {

				if nil == err {
					fmt.Fprintln(out, string("[minisv] Main process normal exit"))
				} else {
					fmt.Fprintln(out, "[minisv] Main process exited, ", err)
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

				if nil == err {
					fmt.Fprintln(out, "[minisv] Main process normal exit")
				} else {
					fmt.Fprintln(out, "[minisv] Main process exited, ", err)
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
			continue

		case <-t.rSignal:
			if t.stopped {
				t.stopped = false
				fmt.Fprintln(out, "[minisv] Starting task")
			} else {

				fmt.Fprintln(out, "[minisv] Doing graceful restart")

				// castling of running processes
				if stage {
					cmd2, done2, err = startNext()
					run2 = nil == err

				} else {
					cmd1, done1, err = startNext()
					run1 = nil == err
				}

				if nil != err {
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
					fmt.Fprintln(out,
						"[minisv] New instance exited too fast, continue using old one")
					continue
				}

				stage = !stage

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
		}

		if t.Pause != 0 {
			fmt.Fprintln(out, "Waiting ",
				time.Second*time.Duration(t.Pause), " before restart")
			time.Sleep(time.Second * time.Duration(t.Pause))
		}
	}
}
