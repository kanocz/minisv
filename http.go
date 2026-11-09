package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
)

//go:embed templates/*
var templatesFS embed.FS

const (
	maxTaskJSONBytes = 1 << 20  // task description
	maxStdinBytes    = 64 << 20 // body passed to stdin of a one-time task
)

// taskNameRe limits names to what is safe in a log file name and a URL
var taskNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

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

// authLimiter counts failed logins per client address: after limit failures
// within a minute further attempts are rejected before the bcrypt check,
// which costs about a second of CPU with the cost 14 hash from the README
type authLimiter struct {
	mu    sync.Mutex
	limit int // <= 0 disables
	fails map[string][]time.Time
}

// recent drops the failures older than a minute and returns the rest;
// caller holds mu
func (l *authLimiter) recent(ip string) []time.Time {
	cutoff := time.Now().Add(-time.Minute)
	kept := l.fails[ip][:0]
	for _, at := range l.fails[ip] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, ip)
	} else {
		l.fails[ip] = kept
	}
	return kept
}

func (l *authLimiter) blocked(ip string) bool {
	if l.limit <= 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(ip)) >= l.limit
}

func (l *authLimiter) fail(ip string) {
	if l.limit <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// ponytail: entries of other addresses expire only when touched;
	// a sweep keeps the map bounded against an attacker rotating addresses
	if len(l.fails) > 10000 {
		for other := range l.fails {
			l.recent(other)
		}
	}
	l.fails[ip] = append(l.recent(ip), time.Now())
}

func (l *authLimiter) reset(ip string) {
	l.mu.Lock()
	delete(l.fails, ip)
	l.mu.Unlock()
}

func basicAuth(user string, passwordHash string, failLimit int) func(http.Handler) http.Handler {
	limiter := &authLimiter{limit: failLimit, fails: map[string][]time.Time{}}
	// sha256 of the last accepted user and password, checked before bcrypt:
	// the UI polls every few seconds and every poll would cost a bcrypt round
	var accepted atomic.Pointer[[sha256.Size]byte]

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			username, password, ok := r.BasicAuth()
			if !ok {
				_requestBasicAuth(w)
				return
			}

			sum := sha256.Sum256([]byte(username + "\x00" + password))
			if cached := accepted.Load(); nil != cached &&
				subtle.ConstantTimeCompare(cached[:], sum[:]) == 1 {
				next.ServeHTTP(w, r)
				return
			}

			// checked after the cache: behind a proxy or ssh tunnel every
			// client shares one address, a script with a stale password
			// must not lock out a client that is already logged in
			ip, _, _ := net.SplitHostPort(r.RemoteAddr)
			if limiter.blocked(ip) {
				http.Error(w, "too many failed logins, try again in a minute",
					http.StatusTooManyRequests)
				return
			}

			if subtle.ConstantTimeCompare([]byte(user), []byte(username)) != 1 ||
				nil != bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)) {
				limiter.fail(ip)
				_requestBasicAuth(w)
				return
			}

			accepted.Store(&sum)
			limiter.reset(ip)
			next.ServeHTTP(w, r)
		})
	}
}

// csrfGuard rejects requests a browser marks as coming from another site.
// The endpoints run commands, and a browser sends cached basic auth
// or the client certificate along with <img src="http://minisv/x/kill">
// embedded in any page. curl and scripts send neither header.
func csrfGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch site := r.Header.Get("Sec-Fetch-Site"); site {
		case "same-origin", "none":
		case "":
			// older browsers: Origin is sent on POST/DELETE. Checked only
			// without Sec-Fetch-Site: a reverse proxy rewrites Host, and
			// Origin would never match it
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if nil != err || u.Host != r.Host {
					http.Error(w, "cross-site request rejected", http.StatusForbidden)
					return
				}
			}
		default:
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// readBody reads at most limit bytes of the request body, answering
// 413 or 400 itself on failure
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	if nil == r.Body {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("no body"))
		return nil, false
	}
	defer r.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(err.Error()))
		return nil, false
	}
	if nil != err {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("body read error: " + err.Error()))
		return nil, false
	}
	return body, true
}

// staticHandler serves bootstrap, htmx and favicon embedded into the
// binary: no CDN dependency to trust and the UI works without internet
// access. Files only, no directory listing
func staticHandler() http.Handler {
	staticFS, err := fs.Sub(templatesFS, "templates/static")
	if err != nil {
		log.Fatalln("Error creating sub-filesystem for static files:", err)
	}
	files := http.StripPrefix("/static/", http.FileServerFS(staticFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}

// httpSrv is the server currently serving; httpInit replaces it on USR1
var httpSrv atomic.Pointer[http.Server]

// httpStop stops accepting requests, so no task is started while minisv
// is on its way out
func httpStop() {
	exiting.Store(true)
	srv := httpSrv.Load()
	if nil == srv {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); nil != err {
		log.Println("Error shutting down http server:", err)
	}
}

func httpInit() {
	srv := httpStart()
	httpSrv.Store(srv)

	// wait for USR1 signal to restart server (mainly for certificates reload)
	user1Chan := make(chan os.Signal, 1)
	signal.Notify(user1Chan, syscall.SIGUSR1)
	for range user1Chan {
		log.Println("Restarting http server...")
		// don't wait for active connections forever - the point
		// of the restart is to get the new certificates serving
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := srv.Shutdown(ctx); nil != err {
			log.Println("Error shutting down http server:", err)
		}
		cancel()
		srv = httpStart()
		httpSrv.Store(srv)
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

	mux.Handle("GET /static/", staticHandler())

	// API routes; everything that changes state goes through csrfGuard
	mux.HandleFunc("GET /api", httpAllStatusAPI)
	mux.HandleFunc("GET /api/{$}", httpAllStatusAPI)
	mux.HandleFunc("GET /api/config", httpGetConfigInfo)
	mux.HandleFunc("POST /api/{id}", csrfGuard(httpCreateTask))
	mux.HandleFunc("DELETE /api/{id}", csrfGuard(httpDeleteTask))
	mux.HandleFunc("GET /api/{id}/restart", csrfGuard(httpRestartTask))
	mux.HandleFunc("GET /api/{id}/run", csrfGuard(httpRunTask))
	mux.HandleFunc("POST /api/{id}/run", csrfGuard(httpRunTaskWithInput))
	mux.HandleFunc("GET /api/{id}/stop", csrfGuard(httpStopTask))
	mux.HandleFunc("GET /api/{id}/term", csrfGuard(httpSignalTask(syscall.SIGTERM)))
	mux.HandleFunc("GET /api/{id}/hup", csrfGuard(httpSignalTask(syscall.SIGHUP)))
	mux.HandleFunc("GET /api/{id}/kill", csrfGuard(httpSignalTask(syscall.SIGKILL)))
	mux.HandleFunc("GET /api/{id}/rotate", csrfGuard(httpLogRotateTask))
	mux.HandleFunc("GET /api/{id}/status", httpStatusOfTask)
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
		handler = basicAuth(config.HTTP.User, config.HTTP.Pass, config.authFailLimit())(handler)
	}
	handler = recoverPanics(handler)
	handler = logRequests(handler)

	listenString := fmt.Sprintf("%s:%d", config.HTTP.Addr, config.HTTP.Port)

	srv := &http.Server{
		Addr:    listenString,
		Handler: handler,
		// basic protection against connections stuck in headers/body
		// (slowloris); WriteTimeout is above the 3s waits in handlers
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	if config.HTTP.ServerCert != "" && config.HTTP.ServerKey != "" {
		// HTTPS
		if config.HTTP.ClientCert != "" {
			clientCert, err := os.ReadFile(config.HTTP.ClientCert)
			if nil != err {
				log.Fatalln("Error loading client cert:", err)
			}
			clientCertPool := x509.NewCertPool()
			// with an empty pool every client would be rejected without a hint why
			if !clientCertPool.AppendCertsFromPEM(clientCert) {
				log.Fatalln("No certificate found in client cert file", config.HTTP.ClientCert)
			}
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

func httpStatusOfTask(w http.ResponseWriter, r *http.Request) {
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

	if code, msg := task.startOneTime(); code != 0 {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(msg))
		return
	}

	go task.Run(nil)
	_, _ = w.Write([]byte("ok"))
}

func httpRunTaskWithInput(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r, maxStdinBytes)
	if !ok {
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

	if code, msg := task.startOneTime(); code != 0 {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(msg))
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

	if task.OneTime {
		// nobody is receiving when the task is idle, and a queued stop
		// would hit the next run instead
		sent := task.signalOneTime(func() {
			select {
			case task.sSignal <- true:
			default:
			}
		})
		if !sent {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte("not running"))
			return
		}
		_, _ = w.Write([]byte("ok"))
		return
	}

	select {
	case task.sSignal <- true:
		_, _ = w.Write([]byte("ok"))
	case <-time.After(3 * time.Second):
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("task is busy, try again later"))
	}
}

func httpSignalTask(sig os.Signal) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		task := getTask(w, r, true)
		if nil == task {
			return
		}

		send := func() {
			select {
			case task.cSignal <- sig:
			default:
			}
		}
		// see httpStopTask
		if task.OneTime && !task.signalOneTime(send) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte("not running"))
			return
		}
		if !task.OneTime {
			send()
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
	body, ok := readBody(w, r, maxTaskJSONBytes)
	if !ok {
		return
	}

	// "config" is taken by GET /api/config
	name := r.PathValue("id")
	if !taskNameRe.MatchString(name) || name == "config" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("task name must match " + taskNameRe.String() +
			" and not be \"config\""))
		return
	}

	var task Task
	err := json.Unmarshal(body, &task)
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
	_, ok = config.Tasks[name]

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
	task.initChannels()
	newTasks[name] = &task

	if exiting.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("minisv is exiting"))
		return
	}

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
		"logBufferLines": config.logBufferLines(),
	}
	writeJSON(w, configInfo)
}
