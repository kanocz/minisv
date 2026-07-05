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
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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

// statusWriter remembers the response code for request logging
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		log.Printf("\"%s %s\" from %s - %d in %s",
			r.Method, r.URL.Path, r.RemoteAddr, sw.status, time.Since(start))
	})
}

func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); nil != p {
				log.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, p)
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); nil != err {
		log.Println("Error encoding json response:", err)
	}
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
	mux := http.NewServeMux()

	config := aConfig.Load()

	authEnabled := config.HTTP.User != "" && config.HTTP.Pass != ""
	if !authEnabled {
		// the API allows to run arbitrary commands, so listening on
		// a non-loopback address without auth is almost surely a mistake
		// (empty Addr means listening on all interfaces)
		ip := net.ParseIP(config.HTTP.Addr)
		if config.HTTP.Addr != "localhost" && (nil == ip || !ip.IsLoopback()) {
			log.Printf("WARNING: HTTP API allows starting arbitrary commands"+
				" and is listening on %q without authentication! Set http.user"+
				" and http.password in the config or bind to 127.0.0.1",
				fmt.Sprintf("%s:%d", config.HTTP.Addr, config.HTTP.Port))
		}
	}

	// Load HTML templates
	templates := loadTemplates()

	// Create a sub-filesystem for templates to serve static files
	staticFS, err := fs.Sub(templatesFS, "templates")
	if err != nil {
		log.Printf("Error creating sub-filesystem for static files: %v", err)
	} else {
		// Serve favicon.svg and other static files from the templates directory
		mux.HandleFunc("GET /favicon.svg", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("GET /api", httpAllStatusAPI)
	mux.HandleFunc("GET /api/{$}", httpAllStatusAPI)
	mux.HandleFunc("GET /api/config", httpGetConfigInfo)
	mux.HandleFunc("POST /api/{id}", httpCreateTask)
	mux.HandleFunc("DELETE /api/{id}", httpDeleteTask)
	mux.HandleFunc("GET /api/{id}/restart", httpRestartTask)
	mux.HandleFunc("GET /api/{id}/run", httpRunTask)
	mux.HandleFunc("POST /api/{id}/run", httpRunTaskWithInput)
	mux.HandleFunc("GET /api/{id}/stop", httpStopTask)
	mux.HandleFunc("GET /api/{id}/term", httpSignalTask(syscall.SIGTERM))
	mux.HandleFunc("GET /api/{id}/hup", httpSignalTask(syscall.SIGHUP))
	mux.HandleFunc("GET /api/{id}/kill", httpSignalTask(syscall.SIGKILL))
	mux.HandleFunc("GET /api/{id}/rotate", httpLogRotateTask)
	mux.HandleFunc("GET /api/{id}/status", httpStatusOfTast)
	mux.HandleFunc("GET /api/{id}/logs", httpGetTaskLogBuffer)
	mux.HandleFunc("GET /status", httpAllStatusAPI)

	// UI routes
	mux.HandleFunc("GET /{$}", httpUIHome(templates))
	mux.HandleFunc("GET /ui/tasks", httpUITaskList(templates))

	// the same task API used to live at the root (e.g. /{id}/restart);
	// registering those patterns directly would conflict with /api ones
	// in ServeMux, so forward everything unmatched to /api instead
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/api" + r.URL.Path
		mux.ServeHTTP(w, r2)
	})

	var handler http.Handler = mux
	if authEnabled {
		handler = basicAuth(config.HTTP.User, config.HTTP.Pass)(handler)
	}
	handler = recoverPanics(handler)
	handler = logRequests(handler)

	listenString := fmt.Sprintf("%s:%d", config.HTTP.Addr, config.HTTP.Port)

	srv := &http.Server{
		Addr:    listenString,
		Handler: handler,
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

		go func() {
			log.Println(srv.ListenAndServeTLS(config.HTTP.ServerCert, config.HTTP.ServerKey))
		}()
	} else {
		// HTTP
		go func() {
			log.Println(srv.ListenAndServe())
		}()
	}

	return srv
}

func getTask(w http.ResponseWriter, r *http.Request, allowOneTime bool) *Task {
	name := r.PathValue("id")

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

	writeJSON(w, result)
}

func httpStatusOfTast(w http.ResponseWriter, r *http.Request) {
	task := getTask(w, r, true)
	if nil == task {
		return
	}

	writeJSON(w, task.GetStatus())
}

func httpRestartTask(w http.ResponseWriter, r *http.Request) {
	task := getTask(w, r, false)
	if nil == task {
		return
	}

	// Loop creates the channel on start, so it may be nil for a moment
	// right after task creation
	if nil == task.rSignal {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("task is not ready yet, try again"))
		return
	}

	// Loop may be busy (restart pause, waiting for a child to terminate)
	// or already gone (task deleted) — don't hang the handler forever
	select {
	case task.rSignal <- true:
		_, _ = w.Write([]byte("ok"))
	case <-time.After(3 * time.Second):
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("task is busy, try again later"))
	}
}

func httpRunTask(w http.ResponseWriter, r *http.Request) {
	task := getTask(w, r, true)
	if nil == task {
		return
	}

	// Run() would clash with the Loop of a persistent task
	// (reassigns signal channels, spawns an unsupervised instance)
	if !task.OneTime {
		w.WriteHeader(http.StatusNotAcceptable)
		_, _ = w.Write([]byte("not a one-time task, use /restart to start it"))
		return
	}

	task.oneTimeMutex.Lock()
	running := task.oneTimeRunning
	if !running {
		task.oneTimeRunning = true
	}
	task.oneTimeMutex.Unlock()
	if running {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("already running"))
		return
	}

	go task.Run(nil)
	_, _ = w.Write([]byte("ok"))
}

func httpRunTaskWithInput(w http.ResponseWriter, r *http.Request) {
	if nil == r.Body {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("no body"))
		return
	}

	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if nil != err {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("body read error: " + err.Error()))
		return
	}
	if nil == body {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("no body readed"))
		return
	}

	task := getTask(w, r, true)
	if nil == task {
		return
	}

	if !task.OneTime {
		w.WriteHeader(http.StatusNotAcceptable)
		_, _ = w.Write([]byte("not a one-time task, use /restart to start it"))
		return
	}

	task.oneTimeMutex.Lock()
	running := task.oneTimeRunning
	if !running {
		task.oneTimeRunning = true
	}
	task.oneTimeMutex.Unlock()
	if running {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("already running"))
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
	name := r.PathValue("id")
	task, ok := config.Tasks[name]

	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("task not found"))
		return
	}

	// exit task; one-time tasks have no Loop and no eSignal channel
	if task.eSignal != nil {
		close(task.eSignal)
	}

	newTasks := make(map[string]*Task, len(config.Tasks))
	for tname, task := range config.Tasks {
		if tname != name {
			newTasks[tname] = task
		}
	}

	// copy the Config itself too: mutating the stored one would race
	// with readers holding the pointer from aConfig.Load()
	newConfig := *config
	newConfig.Tasks = newTasks

	aConfig.Store(&newConfig)
	go saveConfig()

	_, _ = w.Write([]byte("ok"))
}

func httpCreateTask(w http.ResponseWriter, r *http.Request) {
	if nil == r.Body {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("no body"))
		return
	}

	body, err := io.ReadAll(r.Body)
	r.Body.Close()

	if nil != err {
		log.Println("Error processing create task request:", err)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("body read error: " + err.Error()))
		return
	}

	name := r.PathValue("id")
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("empty task name"))
		return
	}

	var task Task
	err = json.Unmarshal(body, &task)
	if nil != err {
		log.Println("Error processing create task request:", err)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("invalid json: " + err.Error()))
		return
	}

	if task.Command == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("command is required"))
		return
	}

	configChangeLock.Lock()
	defer configChangeLock.Unlock()

	config := aConfig.Load()
	_, ok := config.Tasks[name]

	if ok {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("task already exists"))
		return
	}

	// we need a copy of map to prevent read and write in the same moment
	newTasks := make(map[string]*Task, len(config.Tasks)+1)
	for name, task := range config.Tasks {
		newTasks[name] = task
	}
	task.name = name
	newTasks[name] = &task

	// copy the Config itself too: mutating the stored one would race
	// with readers holding the pointer from aConfig.Load()
	newConfig := *config
	newConfig.Tasks = newTasks
	aConfig.Store(&newConfig)

	go saveConfig()

	if !task.OneTime {
		tasksWg.Add(1)
		go task.Loop(needExit, &tasksWg)
		time.Sleep(time.Second)
	}
	writeJSON(w, task.GetStatus())
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

// Add new API endpoint to get log buffer
func httpGetTaskLogBuffer(w http.ResponseWriter, r *http.Request) {
	task := getTask(w, r, true)
	if task == nil {
		return
	}

	task.logBufferMutex.RLock()
	logBuffer := make([]string, len(task.logBuffer))
	copy(logBuffer, task.logBuffer)
	task.logBufferMutex.RUnlock()

	writeJSON(w, logBuffer)
}

// Add new API endpoint to get config info
func httpGetConfigInfo(w http.ResponseWriter, r *http.Request) {
	config := aConfig.Load()
	configInfo := map[string]interface{}{
		"logBufferLines": config.LogBufferLines,
	}
	writeJSON(w, configInfo)
}
