package main

import (
	"fmt"
	"net/http"
	"os"
	"syscall"

	"github.com/pressly/chi"
	"github.com/pressly/chi/middleware"
)

func httpInit() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Route("/:id", func(r chi.Router) {
		r.Get("/restart", httpRestartTask)
		r.Get("/run", httpRunTask)
		r.Get("/term", httpSignalTask(syscall.SIGTERM))
		r.Get("/hup", httpSignalTask(syscall.SIGHUP))
		r.Get("/kill", httpSignalTask(syscall.SIGKILL))
	})
	go http.ListenAndServe(fmt.Sprintf("%s:%d", config.HTTP.Addr, config.HTTP.Port), r)
}

func httpRestartTask(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "id")
	task, ok := config.Tasks[name]

	if !ok {
		w.Write([]byte("task not found"))
		return
	}

	if task.OneTime {
		w.Write([]byte("it's a one-time task"))
		return
	}

	task.rSignal <- true

	w.Write([]byte("ok"))
}

func httpRunTask(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "id")
	task, ok := config.Tasks[name]

	if !ok {
		w.Write([]byte("task not found"))
		return
	}

	if !task.OneTime {
		w.Write([]byte("not a one-time task"))
		return
	}

	go taskRun(name)

	w.Write([]byte("ok"))
}

func httpSignalTask(sig os.Signal) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "id")
		task, ok := config.Tasks[name]

		if !ok {
			w.Write([]byte("task not found"))
			return
		}

		if task.OneTime {
			w.Write([]byte("it's a one-time task"))
			return
		}

		task.cSignal <- sig

		w.Write([]byte("ok"))
	}
}
