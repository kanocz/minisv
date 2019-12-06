package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	reuseport "github.com/kavu/go_reuseport"
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

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT)
	signal.Notify(c, syscall.SIGTERM)
	go func() {
		<-c
		// sig is a ^C, handle it
		fmt.Println("shutting down..")

		// create context with timeout
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		// start http shutdown
		server.Shutdown(ctx)

		// verify, in worst case call cancel via defer
		select {
		case <-time.After(21 * time.Second):
			fmt.Println("this never happen... I hope")
			os.Exit(0)
		case <-ctx.Done():
		}
	}()

	log.Fatal(server.Serve(listener))
}
