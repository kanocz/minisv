package main

import (
	"log"
	"net/http"
	"time"

	reuseport "github.com/kavu/go_reuseport"
	"github.com/tylerb/graceful"
)

func helloHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("Hello world!"))
}

func getRemote(r *http.Request) string {
	remote := r.Header.Get("X-Real-IP")
	if "" == remote {
		remote = r.RemoteAddr
	}
	return remote
}

func logH(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s %s", getRemote(r), r.Method, r.URL)
		handler.ServeHTTP(w, r)
	})
}

func main() {
	http.HandleFunc("/api/hello", helloHandler)

	listener, err := reuseport.NewReusablePortListener("tcp4", "0.0.0.0:80")
	if nil != err {
		log.Fatalf("Error reuseport listen: %v", err)
	}

	server := &http.Server{
		Addr:           "0.0.0.0:80",
		Handler:        logH(http.DefaultServeMux),
		ReadTimeout:    20 * time.Second,
		WriteTimeout:   20 * time.Second,
		MaxHeaderBytes: 1 << 15,
	}

	gracefulServer := graceful.Server{
		Timeout: 5 * time.Second,
		Server:  server,
		Logger:  graceful.DefaultLogger(),
	}

	log.Fatal(gracefulServer.Serve(listener))
}
