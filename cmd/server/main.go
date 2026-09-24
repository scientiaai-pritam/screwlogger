// Command server is the ScrewLogger LAN server: ingest API + health (M1).
// M2 adds the open query API, admin UI and dashboards.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"screwlogger/internal/server"
)

func main() {
	dbPath := flag.String("db", "screwlogger.db", "path to SQLite database")
	addr := flag.String("addr", ":8080", "listen address")
	enroll := flag.String("enroll", "", "enroll a device by name, print its one-time token, then exit")
	flag.Parse()

	store, err := server.OpenStore(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	if *enroll != "" {
		id, token, err := store.CreateDevice(*enroll)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("device_id: %s\ntoken (shown once, store hashed server-side): %s\n", id, token)
		return
	}

	mux := http.NewServeMux()
	mux.Handle("POST /v1/ingest", server.IngestHandler(store))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	log.Printf("screwlogger server listening on %s (db: %s)", *addr, *dbPath)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}
