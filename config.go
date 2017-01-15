package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"time"
)

type configDuration time.Duration

func (d *configDuration) UnmarshalJSON(b []byte) error {
	duration, err := time.ParseDuration(strings.Trim(string(b), " \""))
	*d = configDuration(duration)
	return err
}

var config struct {
	LogDir        string         `json:"logdir"`
	LogPrefix     string         `json:"logfileprefix"`
	LogSuffixDate string         `json:"logsuffixdate"`
	LogDate       string         `json:"logdate"`
	LogReopen     configDuration `json:"logreopen"`
	Tasks         map[string]struct {
		Command   string   `json:"command"`
		Args      []string `json:"args"`
		WorkDir   string   `json:"workdir"`
		Wait      int      `json:"wait"`
		Pause     int      `json:"restartPause"`
		StartTime int      `json:"startTime"`
		OneTime   bool     `json:"oneTime"`
		// hidden fields
		cSignal chan os.Signal
		rSignal chan bool // restart signal
		fSignal chan bool // log flush signal
	} `json:"tasks"`
	HTTP struct {
		Addr string `json:"address"`
		Port int    `json:"port"`
	} `json:"http"`
}

var (
	configfile = flag.String("config", "/opt/minisv.json", "minisv config file in json format")
)

func readConfig() bool {
	data, err := ioutil.ReadFile(*configfile)
	if nil != err {
		log.Println("Error reading config file: ", err)
		return false
	}

	err = json.Unmarshal(data, &config)
	if nil != err {
		log.Println("Error parsing config file: ", err)
		return false
	}

	return true
}
