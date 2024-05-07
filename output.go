package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	gelfChunkSize = 1300
)

var (
	gelfMsgID uint64
)

func openOrStdout(filename string, timeFormat string) *os.File {
	outName := filename
	if timeFormat != "" {
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

func sendGrayLogUDPMessage(message string, serviceName string, graylog grayLogConfig) error {
	if graylog.socket == nil {
		return nil
	}

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "minisv" // graylog rejects messages without hostname
	}

	msg := map[string]interface{}{
		"version":       "1.1",
		"host":          hostname,
		"short_message": message,
		"timestamp":     float64(time.Now().UnixNano()) / 1000000000.0,
		"level":         graylog.Level,
		"_service":      serviceName,
	}

	for k, v := range graylog.AddFields {
		msg[k] = v
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("error encoding graylog json message: %w", err)
	}

	if len(data) > 1400 { // with IP and UDP headers this will be more than 1500 bytes
		var buf bytes.Buffer
		cdata := zlib.NewWriter(&buf)
		_, err := cdata.Write(data)
		if err == nil {
			err = cdata.Close()
			if err == nil {
				data = buf.Bytes()
			}
		}
	}

	// if message is small enough
	if len(data) < 1401 {
		_, err = graylog.socket.Write(data)
		if err != nil {
			return fmt.Errorf("error sending graylog UDP message: %w", err)
		}
		return nil
	}

	if len(data) > 65000 { // what is actual limit for graylog/udp?..
		return fmt.Errorf("log message is too long for graylog (%d bytes)", len(data))
	}

	// if no we need to send in chunks...
	header := [12]byte{0x1e, 0x0f}
	binary.BigEndian.PutUint64(header[2:10], atomic.AddUint64(&gelfMsgID, 1))
	chunks := byte(len(data) / gelfChunkSize)
	if len(data)%gelfChunkSize > 0 {
		chunks++
	}
	header[11] = chunks

	for i := 0; i < len(data); i += gelfChunkSize {
		if i+gelfChunkSize < len(data) {
			_, err = graylog.socket.Write(append(header[:], data[i:i+gelfChunkSize]...))
		} else {
			_, err = graylog.socket.Write(append(header[:], data[i:]...))
		}
		if err != nil {
			return fmt.Errorf("error sending graylog UDP message: %w", err)
		}
		header[10]++
	}

	return nil
}

func logWithRotation(filename string, timeSuffixFormat string, rotate chan bool,
	timeFormat string, serviceName string, graylog grayLogConfig) io.WriteCloser {

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
				if timeFormat != "" {
					_, err = out.WriteString(
						fmt.Sprintf("%s: %s", time.Now().Format(timeFormat), str))
				} else {
					_, err = out.WriteString(str)
				}
				if nil != err {
					log.Println("Error writing to output file: ", err)
				}
				sendGrayLogUDPMessage(str, serviceName, graylog)
			}
		}
	}()

	return writer
}

func rotateLogs() {
	config := aConfig.Load()

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
	config := aConfig.Load()

	if nil == config.LogReopen {
		return
	}

	every := time.Duration(*config.LogReopen)

	// if no value present in config
	if every == 0 {
		return
	}

	log.Println("Rotate logs every ", every)

	// align time in so ugly way :)
	time.Sleep(time.Until(time.Now().Truncate(every).Add(every)))

	go rotateLogs()
	ticker := time.NewTicker(every)
	for range ticker.C {
		go rotateLogs()
	}
}
