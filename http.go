package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/render"
	"golang.org/x/crypto/bcrypt"
)

//go:embed templates/*
var templatesFS embed.FS

// UITaskData represents the task data used in UI templates
type UITaskData struct {
	Command string
	Args    []string
	OneTime bool
	Status  TaskStatus
}

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
	srv := httpStart()

	// wait for USR1 signal to restart server (manly for certificates reload)
	user1Chan := make(chan os.Signal, 1)
	signal.Notify(user1Chan, syscall.SIGUSR1)
	for range user1Chan {
		log.Println("Restarting http server...")
		srv.Shutdown(context.Background())
		srv = httpStart()
	}

}

func httpStart() *http.Server {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	config := aConfig.Load()

	if config.HTTP.User != "" && config.HTTP.Pass != "" {
		r.Use(basicAuth(config.HTTP.User, config.HTTP.Pass))
	}

	// Load HTML templates
	templates := loadTemplates()

	// Create a sub-filesystem for templates to serve static files
	staticFS, err := fs.Sub(templatesFS, "templates")
	if err != nil {
		log.Printf("Error creating sub-filesystem for static files: %v", err)
	} else {
		// Serve favicon.svg and other static files from the templates directory
		r.Get("/favicon.svg", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/svg+xml")
			file, err := staticFS.Open("favicon.svg")
			if err != nil {
				http.Error(w, "Favicon not found", http.StatusNotFound)
				return
			}
			defer file.Close()
			io.Copy(w, file)
		})

		// Could add other static files handlers here if needed in the future
	}

	// API routes
	r.Route("/api", func(r chi.Router) {
		r.Get("/", httpAllStatusAPI)
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
	})

	// UI routes
	r.Get("/", httpUIHome(templates))
	r.Get("/ui/tasks", httpUITaskList(templates))

	// Keep original API routes for backward compatibility
	r.Get("/status", httpAllStatusAPI)
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

	srv := &http.Server{
		Addr:    listenString,
		Handler: r,
	}

	if config.HTTP.ServerCert != "" && config.HTTP.ServerKey != "" {
		// HTTPS
		if config.HTTP.ClientCert != "" {
			clientCert, err := os.ReadFile(config.HTTP.ClientCert)
			if nil != err {
				log.Fatalln("Error loading client cert:", err)
			}
			clientCertPool := x509.NewCertPool()
			clientCertPool.AppendCertsFromPEM(clientCert)
			srv.TLSConfig = &tls.Config{
				ClientAuth: tls.RequireAndVerifyClientCert,
				ClientCAs:  clientCertPool,
			}

		}

		go log.Println(srv.ListenAndServeTLS(config.HTTP.ServerCert, config.HTTP.ServerKey))
	} else {
		// HTTP
		go log.Println(http.ListenAndServe(listenString, r))
	}

	return srv
}

func getTask(w http.ResponseWriter, r *http.Request, allowOneTime bool) *Task {
	name := chi.URLParam(r, "id")

	config := aConfig.Load()

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

func httpAllStatusAPI(w http.ResponseWriter, r *http.Request) {
	config := aConfig.Load()
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
	body, err := io.ReadAll(r.Body)
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

	config := aConfig.Load()
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

	body, err := io.ReadAll(r.Body)
	r.Body.Close()

	if nil != err {
		log.Println("Error processing create task request:", err)
		return
	}

	name := chi.URLParam(r, "id")
	if name == "" {
		return
	}

	var task Task
	err = json.Unmarshal(body, &task)
	if nil != err {
		log.Println("Error processing create task request:", err)
		return
	}

	if task.Command == "" {
		return
	}

	configChangeLock.Lock()
	defer configChangeLock.Unlock()

	config := aConfig.Load()
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
	task.name = name
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

// Template functions for the UI
func loadTemplates() *template.Template {
	// Create a new template with functions if needed
	// P.S.: just for future to avoid looking for documentation later :)
	tmpl := template.New("").Funcs(template.FuncMap{
		// Add any custom template functions here
	})

	// Parse templates from embedded filesystem
	templatesSubFS, err := fs.Sub(templatesFS, "templates")
	if err != nil {
		// it's strange, but we need to keep all services running
		// even if templates are not loaded and UI is not available
		log.Printf("Error creating sub-filesystem for templates: %v", err)
		return template.New("")
	}

	tmpl, err = tmpl.ParseFS(templatesSubFS, "*.html")
	if err != nil {
		// Same as above, we need to keep all services running
		log.Printf("Error loading embedded templates: %v", err)
		return template.New("")
	}

	return tmpl
}

// Add UI handler for home page
func httpUIHome(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		config := aConfig.Load()

		// Get hostname
		hostname, err := os.Hostname()
		if err != nil {
			hostname = "Unknown"
		}

		// Prepare data for the template
		data := struct {
			Tasks    map[string]UITaskData
			Hostname string
		}{
			Tasks:    make(map[string]UITaskData),
			Hostname: hostname,
		}

		for name, task := range config.Tasks {
			data.Tasks[name] = UITaskData{
				Command: task.Command,
				Args:    task.Args,
				OneTime: task.OneTime,
				Status:  task.GetStatus(),
			}
		}

		// Render the home template
		if err := tmpl.ExecuteTemplate(w, "layout.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			log.Printf("Template error: %v", err)
		}
	}
}

// Add UI handler for task list (used for HTMX partial updates)
func httpUITaskList(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		config := aConfig.Load()

		// Prepare data for the template
		data := struct {
			Tasks map[string]UITaskData
		}{
			Tasks: make(map[string]UITaskData),
		}

		for name, task := range config.Tasks {
			data.Tasks[name] = UITaskData{
				Command: task.Command,
				Args:    task.Args,
				OneTime: task.OneTime,
				Status:  task.GetStatus(),
			}
		}

		// Render just the tasks template (not the full layout)
		if err := tmpl.ExecuteTemplate(w, "tasks.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			log.Printf("Template error: %v", err)
		}
	}
}
