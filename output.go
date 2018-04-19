package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func openOrStdout(filename string, timeFormat string) *os.File {
	outName := filename
	if "" != timeFormat {
		outName = fmt.Sprintf("%s.%s", filename, time.Now().Format(timeFormat))
	}

	out, err := os.OpenFile(outName, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0600)
	if nil != err {
		log.Printf("[minisv] Error opening output log (%s), using stdout: %v\n",
			outName, err)
		return os.Stdout
	}
	return out
}

func logWithRotation(filename string, timeSuffixFormat string, rotate chan bool,
	timeFormat string) io.WriteCloser {

	reader, writer := io.Pipe()

	bufread := bufio.NewReader(reader)
	bufchan := make(chan string, 100)

	go func() {
		defer close(bufchan)

		var err error
		var str string

		for nil == err {
			str, err = bufread.ReadString('\n')
			if nil == err {
				bufchan <- str
			}
		}
	}()

	go func() {
		out := openOrStdout(filename, timeSuffixFormat)
		for {
			select {
			case <-rotate:
				if os.Stdout != out {
					if err := out.Close(); nil != err {
						log.Println("Error closing output: ", err)
					}
				}
				out = openOrStdout(filename, timeSuffixFormat)
			case str, ok := <-bufchan:
				if !ok {
					if os.Stdout != out {
						if err := out.Close(); nil != err {
							log.Println("Error closing output: ", err)
						}
					}
					return
				}
				var err error
				if "" != timeFormat {
					_, err = out.WriteString(
						fmt.Sprintf("%s: %s", time.Now().Format(timeFormat), str))
				} else {
					_, err = out.WriteString(str)
				}
				if nil != err {
					log.Println("Error writing to output file: ", err)
				}
			}
		}
	}()

	return writer
}

func rotateLogs() {
	config := aConfig.Load().(Config)

	for name := range config.Tasks {
		if !config.Tasks[name].OneTime {
			select {
			case config.Tasks[name].fSignal <- true:
			default:
			}
		}
	}
}

func rotateOnHUP() {
	hupChan := make(chan os.Signal, 1)
	signal.Notify(hupChan, syscall.SIGHUP)
	for range hupChan {
		rotateLogs()
	}
}

func rotateEveryPeriod() {
	config := aConfig.Load().(Config)

	if nil == config.LogReopen {
		return
	}

	every := time.Duration(*config.LogReopen)

	// if no value present in config
	if 0 == every {
		return
	}

	// align time in so ugly way :)
	time.Sleep(time.Now().Truncate(every).Add(every).Sub(time.Now()))

	go rotateLogs()
	ticker := time.NewTicker(every)
	for range ticker.C {
		go rotateLogs()
	}
}
