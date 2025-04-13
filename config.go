package main

import (
	"encoding/json"
	"flag"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type configDuration time.Duration

func (d *configDuration) UnmarshalJSON(b []byte) error {
	duration, err := time.ParseDuration(strings.Trim(string(b), " \""))
	*d = configDuration(duration)
	return err
}

func (d *configDuration) MarshalJSON() ([]byte, error) {
	return ([]byte)("\"" + (*time.Duration)(d).String() + "\""), nil
}

type grayLogConfig struct {
	Remote    string                 `json:"remote"`
	socket    *net.UDPConn           // udp connection
	Level     int                    `json:"level"`
	AddFields map[string]interface{} `json:"addfields"`
}

// Config represents not only configuration but also current running state
type Config struct {
	LogDir         string           `json:"logdir"`
	LogPrefix      string           `json:"logfileprefix"`
	LogSuffixDate  string           `json:"logsuffixdate"`
	LogDate        string           `json:"logdate"`
	LogReopen      *configDuration  `json:"logreopen"`
	LogBufferLines int              `json:"logbufferlines"` // Number of log lines to keep in memory buffer
	GrayLog        grayLogConfig    `json:"graylog"`
	Tasks          map[string]*Task `json:"tasks"`
	Limits         []configRLimit   `json:"limits"`
	HTTP           struct {
		Addr       string `json:"address"`
		Port       int    `json:"port"`
		ServerCert string `json:"servercert"`
		ServerKey  string `json:"serverkey"`
		ClientCert string `json:"clientcert"`
		User       string `json:"user"`
		Pass       string `json:"password"`
	} `json:"http"`
}

var (
	aConfig          atomic.Pointer[Config]
	configChangeLock sync.Mutex
)

var (
	configfile = flag.String("config", "/etc/minisv.json",
		"minisv config file in json format")
)

func readConfig() bool {
	data, err := os.ReadFile(*configfile)
	if nil != err {
		log.Println("Error reading config file: ", err)
		return false
	}

	var config Config

	err = json.Unmarshal(data, &config)
	if nil != err {
		log.Println("Error parsing config file: ", err)
		return false
	}

	for name, task := range config.Tasks {
		task.name = name
	}

	// Set default value for LogBufferLines if not specified
	if config.LogBufferLines <= 0 {
		config.LogBufferLines = 10 // Default to 10 lines if not specified
	}

	if config.GrayLog.Remote != "" {
		addr, err := net.ResolveUDPAddr("udp", config.GrayLog.Remote)
		if err != nil {
			log.Println("invalid graylog remote: ", err)
		} else {
			config.GrayLog.socket, err = net.DialUDP("udp", nil, addr)
			if err != nil {
				log.Println("unable to create UDP socket for graylog: ", err)
			}
		}

	}

	aConfig.Store(&config)

	return true
}

var (
	saveMutex sync.Mutex
)

func saveConfig() {
	saveMutex.Lock()
	defer saveMutex.Unlock()

	data, err := json.MarshalIndent(aConfig.Load(), "", "  ")
	if nil != err {
		log.Println("Error json encoding config for save:", err)
		return
	}

	err = os.WriteFile(*configfile, data, 0644)
	if nil != err {
		log.Println("Error on config save:", err)
		return
	}
}
