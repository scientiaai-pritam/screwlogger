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
	apikey := flag.String("apikey", "", "create an API key with this label, print it once, then exit")
	adminPassword := flag.String("admin-password", "", "admin password (empty generates a random one, logged once)")
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

	if *apikey != "" {
		key, err := store.CreateAPIKey(*apikey)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("api key (shown once, store hashed server-side): %s\n", key)
		return
	}

	auth, plaintext, err := server.NewAdminAuth(*adminPassword)
	if err != nil {
		log.Fatal(err)
	}
	if *adminPassword == "" {
		log.Printf("admin password (auto-generated, shown once): %s", plaintext)
	}

	mux := server.NewMux(store, auth)

	log.Printf("screwlogger server listening on %s (db: %s)", *addr, *dbPath)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}
