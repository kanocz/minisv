package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
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

func termChild(running bool, cmd *exec.Cmd, ch chan error, wait int, logger *log.Logger, wg *sync.WaitGroup) {
	if nil != wg {
		defer wg.Done()
	}
	if !running {
		return
	}

	cmd.Process.Signal(syscall.SIGTERM)
	if !waitForErrChan(ch, time.Duration(wait)*time.Second) {
		logger.Println("Process is still runing, sending kill signal")
		cmd.Process.Kill()
	}

}

func taskLoop(name string, cExit chan bool, wg *sync.WaitGroup) {
	defer wg.Done()

	out, err := os.OpenFile(fmt.Sprintf("%s/%s%s.log", config.LogDir, config.LogPrefix, name), os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if nil != err {
		log.Printf("Error opening output log (%s/%s.log), using stdout: %v\n", config.LogDir, name, err)
		out = os.Stdout
	} else {
		defer out.Close()
	}

	logger := log.New(out, "", log.LstdFlags)

	// true - main is cmd1, false - main is cmd2 :)
	stage := true

	startNext := func() (*exec.Cmd, chan error, error) {
		logger.Printf("Starting %s %v\n", config.Tasks[name].Command, config.Tasks[name].Args)
		cmd := exec.Command(config.Tasks[name].Command, config.Tasks[name].Args...)
		cmd.Stdout = out
		cmd.Stderr = out
		if "" != config.Tasks[name].WorkDir {
			cmd.Dir = config.Tasks[name].WorkDir
		}

		err := cmd.Start()
		if nil != err {
			logger.Printf("Error starting %s (%s): %v\n", name, config.Tasks[name].Command, err)
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
					logger.Println("Main process normal exit")
				} else {
					logger.Println("Main process exited, ", err)
				}

			} else {

				if nil == err {
					logger.Println("Old process normal exit")
				} else {
					logger.Println("Old process exited, ", err)
				}
				// don't need wait after old process exit
				continue
			}

		case err := <-done2:

			run2 = false

			if stage {

				if nil == err {
					logger.Println("Old process normal exit")
				} else {
					logger.Println("Old process exited, ", err)
				}
				// don't need wait after old process exit
				continue

			} else {

				if nil == err {
					logger.Println("Main process normal exit")
				} else {
					logger.Println("Main process exited, ", err)
				}

			}

		case sig := <-config.Tasks[name].cSignal:
			if stage {
				logger.Println("Sending ", sig, " signal to process ", cmd1.Process.Pid)
				cmd1.Process.Signal(sig)
			} else {
				logger.Println("Sending ", sig, " signal to process ", cmd2.Process.Pid)
				cmd2.Process.Signal(sig)
			}

			continue

		case <-config.Tasks[name].rSignal:
			logger.Println("Doing gracefull restart")

			// castling of running processes
			if stage {
				cmd2, done2, err = startNext()
				run2 = nil == err

			} else {
				cmd1, done1, err = startNext()
				run1 = nil == err
			}

			if nil != err {
				logger.Println("Unable to start new instance, continue using old one")
				continue
			}

			var exited bool
			if stage {
				exited = waitForErrChan(done2, time.Second*time.Duration(config.Tasks[name].StartTime))
			} else {
				exited = waitForErrChan(done1, time.Second*time.Duration(config.Tasks[name].StartTime))
			}

			if exited {
				logger.Println("New instance exited too fast, continue using old one")
				continue
			}

			stage = !stage

			logger.Println("New instance running, terminating old one")
			if stage {
				termChild(run1, cmd1, done1, config.Tasks[name].Wait, logger, nil)
			} else {
				termChild(run2, cmd2, done2, config.Tasks[name].Wait, logger, nil)
			}

			continue

		case <-cExit:
			logger.Println("Sending term signal to childs")
			smallWg := sync.WaitGroup{}
			smallWg.Add(2)
			go termChild(run1, cmd1, done1, config.Tasks[name].Wait, logger, &smallWg)
			go termChild(run2, cmd2, done2, config.Tasks[name].Wait, logger, &smallWg)
			smallWg.Wait()
			return
		}

		if config.Tasks[name].Pause != 0 {
			logger.Println("Waiting ", time.Second*time.Duration(config.Tasks[name].Pause), " before restart")
			time.Sleep(time.Second * time.Duration(config.Tasks[name].Pause))
		}
	}
}

func main() {

	flag.Parse()
	if !readConfig() {
		return
	}

	needExit := make(chan bool)
	wg := sync.WaitGroup{}

	for name := range config.Tasks {
		wg.Add(1)
		cx := config.Tasks[name]
		cx.cSignal = make(chan os.Signal)
		cx.rSignal = make(chan bool)
		config.Tasks[name] = cx

		go taskLoop(name, needExit, &wg)
	}

	httpInit()

	log.Println("dockstarted running...")

	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGTERM)
	signal.Notify(exitChan, syscall.SIGINT)

	<-exitChan

	log.Println("Exiting...")

	close(needExit)
	wg.Wait()

}
