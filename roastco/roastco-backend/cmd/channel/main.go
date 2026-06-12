// cmd/channel is the stubbed channel provider — a separate deployment that
// knows nothing about the CRM except the callback_url in each send request.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/roastco/backend/internal/channelsim"
	"github.com/roastco/backend/internal/envfile"
)

func main() {
	envfile.Load()
	sim := channelsim.New()
	// CHANNEL_PORT wins so the .env's PORT (which belongs to the CRM) can't
	// collide locally; bare PORT is still honored for deploys (Railway injects
	// PORT per service); default 8081.
	port := os.Getenv("CHANNEL_PORT")
	if port == "" {
		port = os.Getenv("PORT")
	}
	if port == "" {
		port = "8081"
	}
	log.Printf("channel service listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, sim.Router()))
}
