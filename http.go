package main

import (
	"crypto/tls"
	"crypto/x509"
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
	"golang.org/x/crypto/bcrypt"
)

func _requestBasicAuth(w http.ResponseWriter) {
	w.Header().Add("WWW-Authenticate", `Basic realm="minisv"`)
	w.WriteHeader(http.StatusUnauthorized)
}

func basicAuth(user string, passwordHash string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			username, password, ok := r.BasicAuth()
			if !ok {
				_requestBasicAuth(w)
				return
			}

			if user != username {
				_requestBasicAuth(w)
				return
			}

			if nil != bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)) {
				_requestBasicAuth(w)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func httpInit() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	config := aConfig.Load().(Config)

	if "" != config.HTTP.User && "" != config.HTTP.Pass {
		r.Use(basicAuth(config.HTTP.User, config.HTTP.Pass))
	}

	r.Get("/", httpAllStatus)
	r.Route("/{id}", func(r chi.Router) {
		r.Post("/", httpCreateTask)
		r.Delete("/", httpDeleteTask)
		r.Get("/restart", httpRestartTask)
		r.Get("/run", httpRunTask)
		r.Post("/run", httpRunTaskWithInput)
		r.Get("/stop", httpStopTask)
		r.Get("/term", httpSignalTask(syscall.SIGTERM))
		r.Get("/hup", httpSignalTask(syscall.SIGHUP))
		r.Get("/kill", httpSignalTask(syscall.SIGKILL))
		r.Get("/rotate", httpLogRotateTask)
		r.Get("/status", httpStatusOfTast)
	})

	listenString := fmt.Sprintf("%s:%d", config.HTTP.Addr, config.HTTP.Port)

	if "" == config.HTTP.ServerCert || "" == config.HTTP.ServerKey {
		log.Println(
			http.ListenAndServe(
				listenString, r))
	} else {
		if "" == config.HTTP.ClientCert {
			log.Println(
				http.ListenAndServeTLS(
					listenString, config.HTTP.ServerCert, config.HTTP.ServerKey, r))

		} else {

			clientCert, err := ioutil.ReadFile(config.HTTP.ClientCert)
			if nil != err {
				log.Fatalln("Error loading client cert:", err)
			}
			clientCertPool := x509.NewCertPool()
			clientCertPool.AppendCertsFromPEM(clientCert)
			srv := &http.Server{
				Addr:    listenString,
				Handler: r,
				TLSConfig: &tls.Config{
					ClientAuth: tls.RequireAndVerifyClientCert,
					ClientCAs:  clientCertPool,
				},
			}
			log.Println(
				srv.ListenAndServeTLS(config.HTTP.ServerCert, config.HTTP.ServerKey))

		}
	}

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

type httpAllStatusItem struct {
	Command string     `json:"command"`
	Args    []string   `json:"args"`
	OneTime bool       `json:"onetime"`
	Status  TaskStatus `json:"status"`
}

func httpAllStatus(w http.ResponseWriter, r *http.Request) {
	config := aConfig.Load().(Config)
	result := map[string]httpAllStatusItem{}

	for name, task := range config.Tasks {
		result[name] = httpAllStatusItem{
			Command: task.Command,
			Args:    task.Args,
			OneTime: task.OneTime,
			Status:  task.GetStatus(),
		}
	}

	render.JSON(w, r, result)
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

	go task.Run(nil)
	_, _ = w.Write([]byte("ok"))
}

func httpRunTaskWithInput(w http.ResponseWriter, r *http.Request) {
	if nil == r.Body {
		_, _ = w.Write([]byte("no body"))
		return
	}

	defer r.Body.Close()
	body, err := ioutil.ReadAll(r.Body)
	if nil != err {
		_, _ = w.Write([]byte("body read error: " + err.Error()))
		return
	}
	if nil == body {
		_, _ = w.Write([]byte("no body readed"))
		return
	}

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

	go task.Run(body)
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
