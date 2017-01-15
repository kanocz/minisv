package main

import (
	"fmt"
	"os/exec"
	"sync"
	"time"
)

func taskRun(name string) {
	// for log rotation we need layer in the middle
	writer := logWithRotation(fmt.Sprintf("%s/%s%s.log", config.LogDir, config.LogPrefix, name),
		config.LogSuffixDate, config.Tasks[name].fSignal, config.LogDate)
	defer writer.Close()

	fmt.Fprintf(writer, "[minisv] Starting %s %v\n", config.Tasks[name].Command, config.Tasks[name].Args)
	cmd := exec.Command(config.Tasks[name].Command, config.Tasks[name].Args...)
	cmd.Stdout = writer
	cmd.Stderr = writer
	if "" != config.Tasks[name].WorkDir {
		cmd.Dir = config.Tasks[name].WorkDir
	}

	err := cmd.Start()
	if nil != err {
		fmt.Fprintf(writer, "[minisv] Error starting %s (%s): %v\n", name, config.Tasks[name].Command, err)
		return
	}

	cmd.Wait()
}

func taskLoop(name string, cExit chan bool, wg *sync.WaitGroup) {
	defer wg.Done()

	// for log rotation we need layer in the middle
	out := logWithRotation(fmt.Sprintf("%s/%s%s.log", config.LogDir, config.LogPrefix, name),
		config.LogSuffixDate, config.Tasks[name].fSignal, config.LogDate)
	defer out.Close()

	var err error

	// true - main is cmd1, false - main is cmd2 :)
	stage := true

	startNext := func() (*exec.Cmd, chan error, error) {
		fmt.Fprintf(out, "[minisv] Starting %s %v\n", config.Tasks[name].Command, config.Tasks[name].Args)
		cmd := exec.Command(config.Tasks[name].Command, config.Tasks[name].Args...)
		cmd.Stdout = out
		cmd.Stderr = out
		if "" != config.Tasks[name].WorkDir {
			cmd.Dir = config.Tasks[name].WorkDir
		}

		err := cmd.Start()
		if nil != err {
			fmt.Fprintf(out, "[minisv] Error starting %s (%s): %v\n", name, config.Tasks[name].Command, err)
			time.Sleep(time.Second * time.Duration(config.Tasks[name].Pause))
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
		if stage && !run1 {
			cmd1, done1, err = startNext()
			run1 = nil == err
		}
		if !stage && !run2 {
			cmd2, done2, err = startNext()
			run2 = nil == err
		}

		select {
		case err := <-done1:

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

		case err := <-done2:

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

		case sig := <-config.Tasks[name].cSignal:
			if stage {
				fmt.Fprintln(out, "[minisv] Sending ", sig, " signal to process ", cmd1.Process.Pid)
				cmd1.Process.Signal(sig)
			} else {
				fmt.Fprintln(out, "[minisv] Sending ", sig, " signal to process ", cmd2.Process.Pid)
				cmd2.Process.Signal(sig)
			}

			continue

		case <-config.Tasks[name].rSignal:
			fmt.Fprintln(out, "[minisv] Doing gracefull restart")

			// castling of running processes
			if stage {
				cmd2, done2, err = startNext()
				run2 = nil == err

			} else {
				cmd1, done1, err = startNext()
				run1 = nil == err
			}

			if nil != err {
				fmt.Fprintln(out, "[minisv] Unable to start new instance, continue using old one")
				continue
			}

			var exited bool
			if stage {
				exited = waitForErrChan(done2, time.Second*time.Duration(config.Tasks[name].StartTime))
			} else {
				exited = waitForErrChan(done1, time.Second*time.Duration(config.Tasks[name].StartTime))
			}

			if exited {
				fmt.Fprintln(out, "[minisv] New instance exited too fast, continue using old one")
				continue
			}

			stage = !stage

			fmt.Fprintln(out, "[minisv] New instance running, terminating old one")
			if stage {
				termChild(run2, cmd2, done2, config.Tasks[name].Wait, out, nil)
			} else {
				termChild(run1, cmd1, done1, config.Tasks[name].Wait, out, nil)
			}

			continue

		case <-cExit:
			fmt.Fprintln(out, "[minisv] Sending term signal to childs")
			smallWg := sync.WaitGroup{}
			smallWg.Add(2)
			go termChild(run1, cmd1, done1, config.Tasks[name].Wait, out, &smallWg)
			go termChild(run2, cmd2, done2, config.Tasks[name].Wait, out, &smallWg)
			smallWg.Wait()
			return
		}

		if config.Tasks[name].Pause != 0 {
			fmt.Fprintln(out, "Waiting ", time.Second*time.Duration(config.Tasks[name].Pause), " before restart")
			time.Sleep(time.Second * time.Duration(config.Tasks[name].Pause))
		}
	}
}
