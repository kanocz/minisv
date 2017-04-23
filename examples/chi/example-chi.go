package main

import (
	"log"
	"net/http"
	"time"

	reuseport "github.com/kavu/go_reuseport"
	"github.com/pressly/chi"
	"github.com/pressly/chi/middleware"
	"github.com/pressly/chi/render"
	"github.com/tylerb/graceful"
)

func helloUser(w http.ResponseWriter, r *http.Request) {

	name := chi.URLParam(r, "name")
	if "" == name {
		name = "world"
	}

	render.JSON(w, r, map[string]string{"action": "hello", "name": name})
}

func main() {

	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.CloseNotify)
	r.Use(middleware.Timeout(10 * time.Second))

	r.Route("/api", func(r chi.Router) {
		r.Route("/hello/:name", func(r chi.Router) {
			r.Get("/", helloUser)
		})
	})

	listener, err := reuseport.NewReusablePortListener("tcp4", "0.0.0.0:80")
	if nil != err {
		log.Fatalln("Error listening: ", err)
	}

	err = (&graceful.Server{
		Timeout: 7 * time.Second,
		Server: &http.Server{
			Addr:    "0.0.0.0:80",
			Handler: r,
		},
	}).Serve(listener)

	if nil != err {
		log.Println("ListenAndServe error: ", err)
	}
}
