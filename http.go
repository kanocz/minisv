package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"syscall"

	"github.com/pressly/chi"
	"github.com/pressly/chi/middleware"
	"github.com/pressly/chi/render"
)

func httpInit() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Route("/:id", func(r chi.Router) {
		r.Get("/restart", httpRestartTask)
		r.Get("/run", httpRunTask)
		r.Get("/stop", httpStopTask)
		r.Get("/term", httpSignalTask(syscall.SIGTERM))
		r.Get("/hup", httpSignalTask(syscall.SIGHUP))
		r.Get("/kill", httpSignalTask(syscall.SIGKILL))
		r.Get("/rotate", httpLogRotateTask)
		r.Get("/status", httpStatusOfTast)
	})
	log.Println(
		http.ListenAndServe(
			fmt.Sprintf("%s:%d", config.HTTP.Addr, config.HTTP.Port), r))
}

func getTask(w http.ResponseWriter, r *http.Request, allowOneTime bool) *Task {
	name := chi.URLParam(r, "id")
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

	go task.Run()
	_, _ = w.Write([]byte("ok"))
}

func httpStopTask(w http.ResponseWriter, r *http.Request) {
	task := getTask(w, r, false)
	if nil == task {
		return
	}

	task.sSignal <- true
	_, _ = w.Write([]byte("ok"))
}

func httpSignalTask(sig os.Signal) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		task := getTask(w, r, false)
		if nil == task {
			return
		}

		task.cSignal <- sig
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
