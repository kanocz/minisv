package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
)

const (
	// AppVersion contains current application version for -version command flag
	AppVersion = "1.3.0"
)

var (
	needExit = make(chan bool)
	tasksWg  = sync.WaitGroup{}
	// runs of one-time tasks; they get SIGTERM on exit like the others
	oneTimeWg = sync.WaitGroup{}
	// set before the http server shuts down: a handler that outlives the
	// shutdown timeout must not start a task nobody will wait for
	exiting atomic.Bool
)

func main() {

	version := flag.Bool("version", false, "print minisv version")
	flag.Parse()

	if *version {
		fmt.Println(AppVersion)
		os.Exit(0)
	}

	if !readConfig() {
		return
	}

	log.Println("Starting...")

	gelfMsgID = uint64(rand.Int63())

	config := aConfig.Load()

	processRLimits(config.Limits)
	startReaper()

	for _, task := range config.Tasks {
		if !task.OneTime {
			tasksWg.Add(1)
			go task.Loop(needExit, &tasksWg)
		}
	}

	go httpInit()

	// log rotation on HUP and on timer
	go rotateOnHUP()
	go rotateEveryPeriod()

	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGTERM)
	signal.Notify(exitChan, syscall.SIGINT)

	log.Println("Running...")

	<-exitChan

	log.Println("Exiting...")

	httpStop()
	close(needExit)
	tasksWg.Wait()
	oneTimeWg.Wait()

}
