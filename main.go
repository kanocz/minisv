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
	AppVersion = "1.1.1a"
)

func main() {

	version := flag.Bool("version", false, "print lcvpn version")
	flag.Parse()

	if *version {
		fmt.Println(AppVersion)
		os.Exit(0)
	}

	if !readConfig() {
		return
	}

	needExit := make(chan bool)
	wg := sync.WaitGroup{}

	for _, task := range config.Tasks {
		if !task.OneTime {
			wg.Add(1)
			go task.Loop(needExit, &wg)
		}
	}

	httpInit()

	log.Println("Running...")

	// log rotation on HUP and on timer
	go rotateOnHUP()
	go rotateEveryPeriod()

	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGTERM)
	signal.Notify(exitChan, syscall.SIGINT)

	<-exitChan

	log.Println("Exiting...")

	close(needExit)
	wg.Wait()

}
