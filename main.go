package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

const (
	// AppVersion contains current application version for -version command flag
	AppVersion = "1.2.0"
)

var (
	needExit = make(chan bool)
	tasksWg  = sync.WaitGroup{}
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

	config := aConfig.Load().(Config)

	processRLimits(config.Limits)

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

	close(needExit)
	tasksWg.Wait()

}
