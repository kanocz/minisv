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

	for name := range config.Tasks {
		cx := config.Tasks[name]
		if !cx.OneTime {
			wg.Add(1)
			cx.cSignal = make(chan os.Signal)
			cx.rSignal = make(chan bool)
			cx.fSignal = make(chan bool)
			config.Tasks[name] = cx

			go taskLoop(name, needExit, &wg)
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
