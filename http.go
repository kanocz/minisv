package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/render"
)

func httpInit() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Route("/{id}", func(r chi.Router) {
		r.Post("/", httpCreateTask)
		r.Delete("/", httpDeleteTask)
		r.Get("/restart", httpRestartTask)
		r.Get("/run", httpRunTask)
		r.Get("/stop", httpStopTask)
		r.Get("/term", httpSignalTask(syscall.SIGTERM))
		r.Get("/hup", httpSignalTask(syscall.SIGHUP))
		r.Get("/kill", httpSignalTask(syscall.SIGKILL))
		r.Get("/rotate", httpLogRotateTask)
		r.Get("/status", httpStatusOfTast)
	})

	config := aConfig.Load().(Config)

	log.Println(
		http.ListenAndServe(
			fmt.Sprintf("%s:%d", config.HTTP.Addr, config.HTTP.Port), r))
}

func getTask(w http.ResponseWriter, r *http.Request, allowOneTime bool) *Task {
	name := chi.URLParam(r, "id")

	config := aConfig.Load().(Config)

	task, ok := config.Tasks[name]

	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("task not found"))
		return nil
	}

	if !allowOneTime && task.OneTime {
		w.WriteHeader(http.StatusNotAcceptable)
		_, _ = w.Write([]byte("it's a one-time task"))
		return nil
	}

	return task
}

func httpStatusOfTast(w http.ResponseWriter, r *http.Request) {
	task := getTask(w, r, true)
	if nil == task {
		return
	}

	render.JSON(w, r, task.GetStatus())
}

func httpRestartTask(w http.ResponseWriter, r *http.Request) {
	task := getTask(w, r, false)
	if nil == task {
		return
	}

	task.rSignal <- true
	_, _ = w.Write([]byte("ok"))
}

func httpRunTask(w http.ResponseWriter, r *http.Request) {
	task := getTask(w, r, true)
	if nil == task {
		return
	}

	task.oneTimeMutex.Lock()
	running := task.oneTimeRunning
	if !running {
		task.oneTimeRunning = true
	}
	task.oneTimeMutex.Unlock()
	if running {
		_, _ = w.Write([]byte("just running"))
		return
	}

	go task.Run()
	_, _ = w.Write([]byte("ok"))
}

func httpStopTask(w http.ResponseWriter, r *http.Request) {
	task := getTask(w, r, true)
	if nil == task {
		return
	}

	if nil != task.sSignal {
		select {
		case task.sSignal <- true:
		default:
		}
	}

	_, _ = w.Write([]byte("ok"))
}

func httpSignalTask(sig os.Signal) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		task := getTask(w, r, true)
		if nil == task {
			return
		}

		if nil != task.cSignal {
			select {
			case task.cSignal <- sig:
			default:
			}
		}
		_, _ = w.Write([]byte("ok"))
	}
}

func httpLogRotateTask(w http.ResponseWriter, r *http.Request) {
	task := getTask(w, r, false)
	if nil == task {
		return
	}

	select {
	case task.fSignal <- true:
	default:
	}

	_, _ = w.Write([]byte("ok"))
}

func httpDeleteTask(w http.ResponseWriter, r *http.Request) {
	configChangeLock.Lock()
	defer configChangeLock.Unlock()

	config := aConfig.Load().(Config)
	name := chi.URLParam(r, "id")
	task, ok := config.Tasks[name]

	if !ok {
		return
	}

	// exit task
	close(task.eSignal)

	newTasks := make(map[string]*Task, len(config.Tasks)+1)
	for tname, task := range config.Tasks {
		if tname != name {
			newTasks[tname] = task
		}
	}

	config.Tasks = newTasks

	aConfig.Store(config)
	go saveConfig()

	_, _ = w.Write([]byte("ok"))
}

func httpCreateTask(w http.ResponseWriter, r *http.Request) {
	if nil == r.Body {
		return
	}

	body, err := ioutil.ReadAll(r.Body)
	r.Body.Close()

	if nil != err {
		log.Println("Error processing create task request:", err)
		return
	}

	name := chi.URLParam(r, "id")
	if "" == name {
		return
	}

	var task Task
	err = json.Unmarshal(body, &task)
	if nil != err {
		log.Println("Error processing create task request:", err)
		return
	}

	if "" == task.Command {
		return
	}

	configChangeLock.Lock()
	defer configChangeLock.Unlock()

	config := aConfig.Load().(Config)
	_, ok := config.Tasks[name]

	if ok {
		_, _ = w.Write([]byte("task just exist"))
		return
	}

	// we need a copy of map to prevent read and write in the same moment
	newTasks := make(map[string]*Task, len(config.Tasks)+1)
	for name, task := range config.Tasks {
		newTasks[name] = task
	}
	newTasks[name] = &task

	config.Tasks = newTasks
	aConfig.Store(config)

	go saveConfig()

	if !task.OneTime {
		tasksWg.Add(1)
		go task.Loop(needExit, &tasksWg)
		time.Sleep(time.Second)
	}
	render.JSON(w, r, task.GetStatus())
}
