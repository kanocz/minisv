package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
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
	LogDir        string           `json:"logdir"`
	LogPrefix     string           `json:"logfileprefix"`
	LogSuffixDate string           `json:"logsuffixdate"`
	LogDate       string           `json:"logdate"`
	LogReopen     configDuration   `json:"logreopen"`
	Tasks         map[string]*Task `json:"tasks"`
	HTTP          struct {
		Addr string `json:"address"`
		Port int    `json:"port"`
	} `json:"http"`
}

var (
	configfile = flag.String("config", "/etc/minisv.json",
		"minisv config file in json format")
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

	for name, task := range config.Tasks {
		task.name = name
	}

	return true
}
