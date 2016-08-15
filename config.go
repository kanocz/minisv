package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	"os"
)

var config struct {
	LogDir    string `json:"logdir"`
	LogPrefix string `json:"logfileprefix"`
	Tasks     map[string]struct {
		Command   string   `json:"command"`
		Args      []string `json:"args"`
		WorkDir   string   `json:"workdir"`
		Wait      int      `json:"wait"`
		Pause     int      `json:"restartPause"`
		StartTime int      `json:"startTime"`
		// hidden fields
		cSignal chan os.Signal
		rSignal chan bool
	} `json:"tasks"`
	HTTP struct {
		Addr string `json:"address"`
		Port int    `json:"port"`
	} `json:"http"`
}

var (
	configfile = flag.String("config", "/opt/dockstarter.json", "DockStarter config file in json format")
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
