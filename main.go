package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

func main() {

	flag.Parse()
	if !readConfig() {
		return
	}

	needExit := make(chan bool)
	wg := sync.WaitGroup{}

	for _, task := range config.Tasks {
		if !task.OneTime {
			wg.Add(1)
			task.cSignal = make(chan os.Signal)
			task.rSignal = make(chan bool)
			task.fSignal = make(chan bool)
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
